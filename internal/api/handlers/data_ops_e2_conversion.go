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
	"archebase.com/keystone-edge/internal/services/e2conversion"
)

func (h *DataOpsHandler) registerE2ConversionRoutes(api *gin.RouterGroup) {
	api.GET("/episodes/:id/derivatives/e2-multimodal-conversion", h.GetE2Conversion)
	api.POST("/episodes/:id/derivatives/e2-multimodal-conversion/process", h.StartE2Conversion)
	api.POST("/episodes/:id/derivatives/e2-multimodal-conversion/retry", h.RetryE2Conversion)
	api.POST("/episodes/:id/derivatives/e2-multimodal-conversion/cancel", h.CancelE2Conversion)
	api.GET("/episodes/:id/derivatives/e2-multimodal-conversion/logs", h.GetE2ConversionLogs)
	api.POST("/episodes/:id/derivatives/e2-multimodal-conversion/qa", h.RetryE2ConversionQA)
	api.GET("/processing-settings/e2-multimodal-conversion", h.GetE2ConversionSettings)
	api.PUT("/processing-settings/e2-multimodal-conversion", h.UpdateE2ConversionSettings)
	api.GET("/processing-settings/e2-multimodal-conversion/history", h.ListE2ConversionSettingsHistory)
}

func e2EpisodeID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id", "code": "invalid_episode_id"})
		return 0, false
	}
	return id, true
}

func e2Actor(c *gin.Context) string {
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

func (h *DataOpsHandler) GetE2Conversion(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e2Conversion.Get(c.Request.Context(), id)
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *DataOpsHandler) StartE2Conversion(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	value, created, err := h.e2Conversion.Start(c.Request.Context(), id, e2Actor(c))
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	c.JSON(code, gin.H{"derivative": value, "created": created})
}

func (h *DataOpsHandler) RetryE2Conversion(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e2Conversion.Retry(c.Request.Context(), id, e2Actor(c))
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

func (h *DataOpsHandler) CancelE2Conversion(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e2Conversion.Cancel(c.Request.Context(), id, e2Actor(c))
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

func (h *DataOpsHandler) GetE2ConversionLogs(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	logs, err := h.e2Conversion.Logs(c.Request.Context(), id)
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

func (h *DataOpsHandler) RetryE2ConversionQA(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	value, err := h.e2Conversion.RetryQA(c.Request.Context(), id, e2Actor(c))
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": value})
}

func (h *DataOpsHandler) SyncE2Conversion(c *gin.Context) {
	id, ok := e2EpisodeID(c)
	if !ok {
		return
	}
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloud sync worker is unavailable", "code": "sync_unavailable"})
		return
	}
	if err := h.syncWorker.EnqueueE2ConversionManual(c.Request.Context(), id); err != nil {
		statusCode := http.StatusConflict
		code := "sync_rejected"
		if errors.Is(err, services.ErrSyncWorkerNotRunning) || errors.Is(err, services.ErrSyncQueueFull) {
			statusCode = http.StatusServiceUnavailable
			code = "sync_unavailable"
		}
		c.JSON(statusCode, gin.H{"error": "E2 conversion is not eligible for cloud sync", "code": code})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"episode_id": id, "source_type": services.SyncSourceE2Conversion})
}

type UpdateE2ConversionSettingsRequest struct {
	ImageRef              string `json:"image_ref" binding:"required"`
	MaxConcurrent         int    `json:"max_concurrent" binding:"required"`
	ResourceLimitsEnabled *bool  `json:"resource_limits_enabled"`
	ExpectedRevisionID    int64  `json:"expected_revision_id" binding:"required"`
}

func (h *DataOpsHandler) GetE2ConversionSettings(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	value, err := h.e2Conversion.CurrentImageConfig(c.Request.Context())
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": value, "max_concurrent_limit": e2conversion.MaxConfigurableConcurrent})
}
func (h *DataOpsHandler) UpdateE2ConversionSettings(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	var request UpdateE2ConversionSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedRevisionID <= 0 || request.MaxConcurrent < 1 || request.MaxConcurrent > e2conversion.MaxConfigurableConcurrent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image_ref, max_concurrent between 1 and 100, and positive expected_revision_id are required", "code": "invalid_settings"})
		return
	}
	limits := true
	if request.ResourceLimitsEnabled != nil {
		limits = *request.ResourceLimitsEnabled
	}
	value, err := h.e2Conversion.UpdateImageConfig(c.Request.Context(), request.ImageRef, request.MaxConcurrent, limits, request.ExpectedRevisionID, e2Actor(c))
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": value, "max_concurrent_limit": e2conversion.MaxConfigurableConcurrent})
}
func (h *DataOpsHandler) ListE2ConversionSettingsHistory(c *gin.Context) {
	if h.e2Conversion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "E2 conversion is unavailable", "code": "e2_conversion_unavailable"})
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	rows, err := h.e2Conversion.ListImageConfigHistory(c.Request.Context(), pagination.Limit, pagination.Offset)
	if err != nil {
		writeE2ConversionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "limit": pagination.Limit, "offset": pagination.Offset})
}

func writeE2ConversionError(c *gin.Context, err error) {
	statusCode := http.StatusInternalServerError
	code := "e2_conversion_error"
	message := "E2 conversion operation failed"
	switch {
	case errors.Is(err, e2conversion.ErrNotFound), errors.Is(err, e2conversion.ErrEpisodeNotFound):
		statusCode = http.StatusNotFound
		code = "not_found"
		message = "E2 conversion derivative or episode was not found"
	case errors.Is(err, e2conversion.ErrDisabled), errors.Is(err, e2conversion.ErrImageNotConfigured):
		statusCode = http.StatusServiceUnavailable
		code = "e2_conversion_unavailable"
		message = "E2 conversion processing is not available"
	case errors.Is(err, e2conversion.ErrAlreadyDerived):
		statusCode = http.StatusConflict
		code = "already_derived"
		message = "episode is already converted"
	case errors.Is(err, e2conversion.ErrProcessingActive):
		statusCode = http.StatusConflict
		code = "processing_active"
		message = "E2 conversion processing is active"
	case errors.Is(err, e2conversion.ErrRetryRequired):
		statusCode = http.StatusConflict
		code = "retry_required"
		message = "E2 conversion retry is required"
	case errors.Is(err, e2conversion.ErrCleanupPending):
		statusCode = http.StatusConflict
		code = "orbit_delete_pending"
		message = "Orbit cleanup is pending"
	case errors.Is(err, e2conversion.ErrQANotApproved), errors.Is(err, e2conversion.ErrQAUnavailable):
		statusCode = http.StatusConflict
		code = "qa_unavailable"
		message = "E2 conversion QA is unavailable or not approved"
	case errors.Is(err, e2conversion.ErrConfigChanged):
		statusCode = http.StatusConflict
		code = "config_changed"
		message = "E2 conversion processing configuration changed"
	case errors.Is(err, e2conversion.ErrSourceUnavailable):
		statusCode = http.StatusUnprocessableEntity
		code = "source_unavailable"
		message = "E2 episode source is unavailable"
	}
	c.JSON(statusCode, gin.H{"error": message, "code": code})
}
