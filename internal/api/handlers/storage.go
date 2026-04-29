// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
)

// StorageHandler provides MinIO/S3 helper endpoints for the frontend.
type StorageHandler struct {
	s3      *s3.Client
	authCfg *config.AuthConfig
}

// NewStorageHandler creates a new StorageHandler.
func NewStorageHandler(s3Client *s3.Client, authCfg *config.AuthConfig) *StorageHandler {
	return &StorageHandler{s3: s3Client, authCfg: authCfg}
}

func (h *StorageHandler) requireBearerToken(c *gin.Context) bool {
	if h.authCfg == nil {
		logger.Printf("[S3] auth config is nil; refusing request")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth is not configured"})
		return false
	}

	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authorization header"})
		return false
	}
	if _, err := auth.ParseToken(parts[1], h.authCfg); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return false
	}
	return true
}

// RegisterRoutes registers storage-related routes on the given router group.
func (h *StorageHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/storage/presign", h.PresignGetObject)
	r.GET("/storage/object", h.GetObject)
}

type presignResponse struct {
	URL string `json:"url"`
}

var (
	bucketNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	objectSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

func validateS3Location(bucket, objectName string) (string, string, error) {
	bucket = strings.TrimSpace(bucket)
	objectName = strings.TrimSpace(objectName)
	if bucket == "" || objectName == "" {
		return "", "", fmt.Errorf("bucket and object are required")
	}

	if !bucketNameRe.MatchString(bucket) || strings.Contains(bucket, "..") {
		return "", "", fmt.Errorf("invalid bucket")
	}

	if strings.Contains(objectName, "%") {
		unescaped, err := url.PathUnescape(objectName)
		if err != nil {
			return "", "", fmt.Errorf("invalid object encoding")
		}
		objectName = unescaped
	}

	if !utf8.ValidString(objectName) {
		return "", "", fmt.Errorf("invalid object")
	}
	for _, r := range objectName {
		if r < 0x20 || r == 0x7f {
			return "", "", fmt.Errorf("invalid object")
		}
	}

	if strings.HasPrefix(objectName, "/") || strings.Contains(objectName, `\`) {
		return "", "", fmt.Errorf("invalid object")
	}

	cleaned := path.Clean("/" + objectName)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", "", fmt.Errorf("invalid object")
	}

	segments := strings.Split(cleaned, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("invalid object")
		}
		if !objectSegment.MatchString(seg) {
			return "", "", fmt.Errorf("invalid object")
		}
	}

	return bucket, cleaned, nil
}

// PresignGetObject returns a presigned GET URL for an object.
// The returned URL is formatted for the frontend's /s3 proxy:
//
//	/s3/<bucket>/<object>?X-Amz-...
func (h *StorageHandler) PresignGetObject(c *gin.Context) {
	if h.s3 == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "storage is not configured"})
		return
	}

	if !h.requireBearerToken(c) {
		return
	}

	bucket, objectName, err := validateS3Location(c.Query("bucket"), c.Query("object"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bucket or object"})
		return
	}

	expSeconds := 600
	if raw := strings.TrimSpace(c.Query("expires_seconds")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 3600 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expires_seconds must be between 1 and 3600"})
			return
		}
		expSeconds = v
	}

	u, err := h.s3.PresignedGetObject(c.Request.Context(), bucket, objectName, time.Duration(expSeconds)*time.Second, nil)
	if err != nil {
		logger.Printf("[S3] presign failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to presign url"})
		return
	}

	// Use the presigned URL's escaped path verbatim to avoid signature mismatch.
	// Then prefix it with /s3 for frontend proxy routing.
	path := "/s3" + u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	c.JSON(http.StatusOK, presignResponse{URL: path})
}

// GetObject proxies object download through Keystone so frontend does not
// directly depend on MinIO endpoint/proxy target configuration.
func (h *StorageHandler) GetObject(c *gin.Context) {
	if h.s3 == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "storage is not configured"})
		return
	}

	if !h.requireBearerToken(c) {
		return
	}

	bucket, objectName, err := validateS3Location(c.Query("bucket"), c.Query("object"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bucket or object"})
		return
	}

	ctx := c.Request.Context()

	stat, err := h.s3.StatObject(ctx, bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" || errResp.StatusCode == 404 {
			c.JSON(http.StatusNotFound, gin.H{"error": "object not found"})
			return
		}
		logger.Printf("[S3] proxy stat failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch object metadata"})
		return
	}

	// Conditional requests (best-effort) for browser/media readers.
	if inm := strings.TrimSpace(c.GetHeader("If-None-Match")); inm != "" && strings.Trim(inm, "\"") == strings.Trim(stat.ETag, "\"") {
		c.Status(http.StatusNotModified)
		return
	}
	if ims := strings.TrimSpace(c.GetHeader("If-Modified-Since")); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !stat.LastModified.After(t) {
			c.Status(http.StatusNotModified)
			return
		}
	}

	var (
		statusCode   = http.StatusOK
		contentRange string
		contentLen   = stat.Size
		getOpts      minio.GetObjectOptions
	)
	if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" {
		start, end, ok := parseByteRange(rangeHeader, stat.Size)
		if !ok {
			c.Header("Content-Range", fmt.Sprintf("bytes */%d", stat.Size))
			c.Status(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if err := getOpts.SetRange(start, end); err != nil {
			logger.Printf("[S3] proxy range invalid: bucket=%s, object=%s, range=%s, err=%v", bucket, objectName, rangeHeader, err)
			c.Status(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		statusCode = http.StatusPartialContent
		contentLen = end - start + 1
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, stat.Size)
	}

	obj, err := h.s3.GetObject(ctx, bucket, objectName, getOpts)
	if err != nil {
		logger.Printf("[S3] proxy get failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch object"})
		return
	}
	defer func() {
		if err := obj.Close(); err != nil {
			logger.Printf("[S3] proxy body close failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		}
	}()

	if v := strings.TrimSpace(stat.ContentType); v != "" {
		c.Header("Content-Type", v)
	}
	c.Header("Accept-Ranges", "bytes")
	if etag := strings.TrimSpace(stat.ETag); etag != "" {
		c.Header("ETag", etag)
	}
	if !stat.LastModified.IsZero() {
		c.Header("Last-Modified", stat.LastModified.UTC().Format(http.TimeFormat))
	}
	if contentRange != "" {
		c.Header("Content-Range", contentRange)
	}
	c.Header("Content-Length", strconv.FormatInt(contentLen, 10))

	c.Header("Content-Disposition", "inline; filename*=UTF-8''"+url.QueryEscape(objectName))

	c.Status(statusCode)
	if _, err := io.Copy(c.Writer, obj); err != nil {
		logger.Printf("[S3] proxy stream interrupted: bucket=%s, object=%s, err=%v", bucket, objectName, err)
	}
}

func parseByteRange(hdr string, size int64) (start, end int64, ok bool) {
	// Minimal support for "bytes=<start>-<end>" and "bytes=<start>-" and "bytes=-<suffix>".
	hdr = strings.TrimSpace(hdr)
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(hdr, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a := strings.TrimSpace(parts[0])
	b := strings.TrimSpace(parts[1])

	if size <= 0 {
		return 0, 0, false
	}

	// Suffix: "-N" means last N bytes.
	if a == "" {
		suf, err := strconv.ParseInt(b, 10, 64)
		if err != nil || suf <= 0 {
			return 0, 0, false
		}
		if suf > size {
			suf = size
		}
		return size - suf, size - 1, true
	}

	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	if b == "" {
		return s, size - 1, true
	}
	e, err := strconv.ParseInt(b, 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true
}
