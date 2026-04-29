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
	// Require a valid collector token unless explicitly allowed in dev.
	if h.authCfg == nil || h.authCfg.AllowNoAuthOnDev {
		return true
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

	// Keep presigned URL short-lived since it is only used server-side.
	u, err := h.s3.PresignedGetObject(c.Request.Context(), bucket, objectName, 2*time.Minute, nil)
	if err != nil {
		logger.Printf("[S3] proxy presign failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare object download"})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build upstream request"})
		return
	}

	// Forward key headers used by media/file readers.
	for _, header := range []string{"Range", "If-None-Match", "If-Modified-Since"} {
		if v := strings.TrimSpace(c.GetHeader(header)); v != "" {
			req.Header.Set(header, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Printf("[S3] proxy get failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch object"})
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Printf("[S3] proxy body close failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		}
	}()

	for _, header := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"ETag", "Last-Modified", "Cache-Control",
	} {
		if v := strings.TrimSpace(resp.Header.Get(header)); v != "" {
			c.Header(header, v)
		}
	}
	if dispo := strings.TrimSpace(resp.Header.Get("Content-Disposition")); dispo != "" {
		c.Header("Content-Disposition", dispo)
	} else {
		c.Header("Content-Disposition", "inline; filename*=UTF-8''"+url.QueryEscape(objectName))
	}

	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		logger.Printf("[S3] proxy stream interrupted: bucket=%s, object=%s, err=%v", bucket, objectName, err)
	}
}
