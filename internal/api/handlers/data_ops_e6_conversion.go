// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/services/e6conversion"
)

func (h *DataOpsHandler) registerE6ConversionRoutes(api *gin.RouterGroup) {
	api.GET("/episodes/:id/derivatives/e6-multimodal-conversion", h.GetE6Conversion)
	api.POST("/episodes/:id/derivatives/e6-multimodal-conversion/process", h.StartE6Conversion)
	api.POST("/episodes/:id/derivatives/e6-multimodal-conversion/retry", h.RetryE6Conversion)
	api.POST("/episodes/:id/derivatives/e6-multimodal-conversion/cancel", h.CancelE6Conversion)
	api.GET("/episodes/:id/derivatives/e6-multimodal-conversion/logs", h.GetE6ConversionLogs)
	api.POST("/episodes/:id/derivatives/e6-multimodal-conversion/qa", h.RetryE6ConversionQA)
	api.GET("/processing-settings/e6-multimodal-conversion", h.GetE6ConversionSettings)
	api.PUT("/processing-settings/e6-multimodal-conversion", h.UpdateE6ConversionSettings)
	api.GET("/processing-settings/e6-multimodal-conversion/history", h.ListE6ConversionSettingsHistory)
}

func e6EpisodeID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id", "code": "invalid_episode_id"})
		return 0, false
	}
	return id, true
}

func e6Actor(c *gin.Context) string {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return "admin"
	}
	if value := strings.TrimSpace(claims.OperatorID); value != "" {
		return value
	}
	if value := strings.TrimSpace(claims.Subject); value != "" {
		return value
	}
	return strings.TrimSpace(claims.Role)
}

// GetE6Conversion returns the current E6 conversion derivative.
func (h *DataOpsHandler) GetE6Conversion(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e6Conversion.Get(c.Request.Context(), id)
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

// StartE6Conversion starts or resumes E6 conversion for one Episode.
func (h *DataOpsHandler) StartE6Conversion(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	value, created, err := h.e6Conversion.Start(c.Request.Context(), id, e6Actor(c))
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	c.JSON(code, gin.H{"derivative": value, "created": created})
}

// RetryE6Conversion retries a failed or canceled E6 conversion generation.
func (h *DataOpsHandler) RetryE6Conversion(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e6Conversion.Retry(c.Request.Context(), id, e6Actor(c))
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

// CancelE6Conversion requests cancellation of the current E6 generation.
func (h *DataOpsHandler) CancelE6Conversion(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e6Conversion.Cancel(c.Request.Context(), id, e6Actor(c))
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

// GetE6ConversionLogs returns the current E6 Orbit log tail.
func (h *DataOpsHandler) GetE6ConversionLogs(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	logs, err := h.e6Conversion.Logs(c.Request.Context(), id)
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// RetryE6ConversionQA retries QA for a successfully processed E6 generation.
func (h *DataOpsHandler) RetryE6ConversionQA(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e6Conversion.RetryQA(c.Request.Context(), id, e6Actor(c))
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

// SyncE6Conversion queues an approved E6 derivative for cloud sync.
func (h *DataOpsHandler) SyncE6Conversion(c *gin.Context) {
	id, ok := e6EpisodeID(c)
	if !ok {
		return
	}
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloud sync worker is unavailable", "code": "sync_unavailable"})
		return
	}
	if err := h.syncWorker.EnqueueE6ConversionManual(c.Request.Context(), id); err != nil {
		statusCode := http.StatusConflict
		code := "sync_rejected"
		if errors.Is(err, services.ErrSyncWorkerNotRunning) || errors.Is(err, services.ErrSyncQueueFull) {
			statusCode = http.StatusServiceUnavailable
			code = "sync_unavailable"
		}
		c.JSON(statusCode, gin.H{"error": "E6 conversion is not eligible for cloud sync", "code": code})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"episode_id": id, "source_type": services.SyncSourceE6Conversion})
}

// UpdateE6ConversionSettingsRequest contains an audited E6 image configuration update.
type UpdateE6ConversionSettingsRequest struct {
	ImageRef              string `json:"image_ref" binding:"required"`
	MaxConcurrent         int    `json:"max_concurrent" binding:"required"`
	ResourceLimitsEnabled *bool  `json:"resource_limits_enabled"`
	ExpectedRevisionID    int64  `json:"expected_revision_id" binding:"required"`
}

// GetE6ConversionSettings returns the current E6 processing configuration.
func (h *DataOpsHandler) GetE6ConversionSettings(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	value, err := h.e6Conversion.CurrentImageConfig(c.Request.Context())
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": value, "max_concurrent_limit": e6conversion.MaxConfigurableConcurrent})
}

// UpdateE6ConversionSettings updates the E6 processing configuration.
func (h *DataOpsHandler) UpdateE6ConversionSettings(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	var request UpdateE6ConversionSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedRevisionID <= 0 || request.MaxConcurrent < 1 || request.MaxConcurrent > e6conversion.MaxConfigurableConcurrent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image_ref, max_concurrent between 1 and 100, and positive expected_revision_id are required", "code": "invalid_settings"})
		return
	}
	limits := true
	if request.ResourceLimitsEnabled != nil {
		limits = *request.ResourceLimitsEnabled
	}
	value, err := h.e6Conversion.UpdateImageConfig(c.Request.Context(), request.ImageRef, request.MaxConcurrent, limits, request.ExpectedRevisionID, e6Actor(c))
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": value, "max_concurrent_limit": e6conversion.MaxConfigurableConcurrent})
}

// ListE6ConversionSettingsHistory returns the E6 processing configuration history.
func (h *DataOpsHandler) ListE6ConversionSettingsHistory(c *gin.Context) {
	if h.e6Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E6 conversion is unavailable", "code": "e6_conversion_unavailable"})
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	rows, err := h.e6Conversion.ListImageConfigHistory(c.Request.Context(), pagination.Limit, pagination.Offset)
	if err != nil {
		writeE6ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "limit": pagination.Limit, "offset": pagination.Offset})
}

func writeE6ConversionError(c *gin.Context, err error) {
	statusCode := http.StatusInternalServerError
	code := "e6_conversion_error"
	message := "E6 conversion operation failed"
	switch {
	case errors.Is(err, e6conversion.ErrNotFound), errors.Is(err, e6conversion.ErrEpisodeNotFound):
		statusCode = http.StatusNotFound
		code = "not_found"
		message = "E6 conversion derivative or episode was not found"
	case errors.Is(err, e6conversion.ErrDisabled), errors.Is(err, e6conversion.ErrImageNotConfigured):
		statusCode = http.StatusServiceUnavailable
		code = "e6_conversion_unavailable"
		message = "E6 conversion processing is not available"
	case errors.Is(err, e6conversion.ErrAlreadyDerived):
		statusCode = http.StatusConflict
		code = "already_derived"
		message = "episode is already converted"
	case errors.Is(err, e6conversion.ErrProcessingActive):
		statusCode = http.StatusConflict
		code = "processing_active"
		message = "E6 conversion processing is active"
	case errors.Is(err, e6conversion.ErrRetryRequired):
		statusCode = http.StatusConflict
		code = "retry_required"
		message = "E6 conversion retry is required"
	case errors.Is(err, e6conversion.ErrCleanupPending):
		statusCode = http.StatusConflict
		code = "orbit_delete_pending"
		message = "Orbit cleanup is pending"
	case errors.Is(err, e6conversion.ErrQANotApproved), errors.Is(err, e6conversion.ErrQAUnavailable):
		statusCode = http.StatusConflict
		code = "qa_unavailable"
		message = "E6 conversion QA is unavailable or not approved"
	case errors.Is(err, e6conversion.ErrConfigChanged):
		statusCode = http.StatusConflict
		code = "config_changed"
		message = "E6 conversion processing configuration changed"
	case errors.Is(err, e6conversion.ErrSourceUnavailable):
		statusCode = http.StatusUnprocessableEntity
		code = "source_unavailable"
		message = "E6 episode source is unavailable"
	}
	c.JSON(statusCode, gin.H{"error": message, "code": code})
}
