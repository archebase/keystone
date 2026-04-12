// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

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

// RegisterRoutes registers storage-related routes on the given router group.
func (h *StorageHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/storage/presign", h.PresignGetObject)
}

type presignResponse struct {
	URL string `json:"url"`
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

	// Require a valid collector token unless explicitly allowed in dev.
	if h.authCfg != nil && !h.authCfg.AllowNoAuthOnDev {
		authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authorization header"})
			return
		}
		if _, err := auth.ParseToken(parts[1], h.authCfg); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
	}

	bucket := strings.TrimSpace(c.Query("bucket"))
	objectName := strings.TrimSpace(c.Query("object"))
	if bucket == "" || objectName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bucket and object are required"})
		return
	}
	if strings.Contains(bucket, "..") || strings.Contains(objectName, "..") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bucket or object"})
		return
	}
	objectName = strings.TrimPrefix(objectName, "/")

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
