// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/services/stereosplit"
	"github.com/gin-gonic/gin"
)

type dataOpsStereoSplitManager interface {
	Start(ctx context.Context, episodeID int64, actor string) (stereosplit.Derivative, bool, error)
	Get(ctx context.Context, episodeID int64) (stereosplit.Derivative, error)
	Retry(ctx context.Context, episodeID int64, actor string) (stereosplit.Derivative, error)
	Cancel(ctx context.Context, episodeID int64, actor string) (stereosplit.Derivative, error)
	RetryQA(ctx context.Context, episodeID int64, actor string) (stereosplit.Derivative, error)
	Logs(ctx context.Context, episodeID int64) (string, error)
	CurrentImageConfig(ctx context.Context) (stereosplit.ImageConfig, error)
	UpdateImageConfig(ctx context.Context, imageRef string, maxConcurrent int, expectedRevisionID int64, actor string) (stereosplit.ImageConfig, error)
	ListImageConfigHistory(ctx context.Context, limit, offset int) ([]stereosplit.ImageConfig, error)
	AdmitBulk(ctx context.Context, runID string, episodeID int64, actor string) (stereosplit.BulkAdmission, error)
}

func (h *DataOpsHandler) registerStereoSplitRoutes(api *gin.RouterGroup) {
	api.POST("/episodes/bulk-stereo-split/preview", h.PreviewBulkStereoSplit)
	api.POST("/episodes/bulk-stereo-split", h.BulkStereoSplit)
	api.GET("/episodes/:id/derivatives/stereo-split", h.GetStereoSplit)
	api.POST("/episodes/:id/derivatives/stereo-split/process", h.StartStereoSplit)
	api.POST("/episodes/:id/derivatives/stereo-split/retry", h.RetryStereoSplit)
	api.POST("/episodes/:id/derivatives/stereo-split/cancel", h.CancelStereoSplit)
	api.GET("/episodes/:id/derivatives/stereo-split/logs", h.GetStereoSplitLogs)
	api.POST("/episodes/:id/derivatives/stereo-split/qa", h.RetryStereoSplitQA)
	api.POST("/episodes/:id/derivatives/stereo-split/sync", h.SyncStereoSplit)
}

func (h *DataOpsHandler) registerStereoSplitSettingsRoutes(api *gin.RouterGroup) {
	api.GET("/processing-settings/stereo-split", h.GetStereoSplitSettings)
	api.PUT("/processing-settings/stereo-split", h.UpdateStereoSplitSettings)
	api.GET("/processing-settings/stereo-split/history", h.ListStereoSplitSettingsHistory)
}

// GetStereoSplit returns the current stereo-split derivative for an Episode.
//
// @Summary      Get stereo-split derivative
// @Description  Returns the current durable stereo-split generation and its processing, cleanup, and QA states.
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      200  {object}  stereosplit.Derivative
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split [get]
func (h *DataOpsHandler) GetStereoSplit(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	derivative, err := h.stereoSplit.Get(c.Request.Context(), episodeID)
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusOK, derivative)
}

// StartStereoSplit admits an Episode for stereo-split processing.
//
// @Summary      Start stereo split
// @Description  Creates the Episode's first stereo-split generation, or returns the existing generation idempotently.
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      200  {object}  map[string]any
// @Success      202  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/process [post]
func (h *DataOpsHandler) StartStereoSplit(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	if !h.ensureStereoSplitReady(c) {
		return
	}
	derivative, created, err := h.stereoSplit.Start(c.Request.Context(), episodeID, stereoSplitActor(c))
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	c.JSON(status, gin.H{"derivative": derivative, "created": created})
}

// RetryStereoSplit creates the next generation after a terminal failed or canceled run is cleaned up.
//
// @Summary      Retry stereo split
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      202  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/retry [post]
func (h *DataOpsHandler) RetryStereoSplit(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	derivative, err := h.stereoSplit.Retry(c.Request.Context(), episodeID, stereoSplitActor(c))
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": derivative})
}

// CancelStereoSplit requests cancellation of the current stereo-split generation.
//
// @Summary      Cancel stereo split
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      202  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/cancel [post]
func (h *DataOpsHandler) CancelStereoSplit(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	derivative, err := h.stereoSplit.Cancel(c.Request.Context(), episodeID, stereoSplitActor(c))
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": derivative})
}

// GetStereoSplitLogs returns the bounded Orbit log tail for the current generation.
//
// @Summary      Get stereo-split logs
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      200  {object}  map[string]string
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/logs [get]
func (h *DataOpsHandler) GetStereoSplitLogs(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	logs, err := h.stereoSplit.Logs(c.Request.Context(), episodeID)
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// RetryStereoSplitQA retries automatic QA for a successfully processed generation.
//
// @Summary      Retry stereo-split QA
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      202  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/qa [post]
func (h *DataOpsHandler) RetryStereoSplitQA(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	derivative, err := h.stereoSplit.RetryQA(c.Request.Context(), episodeID, stereoSplitActor(c))
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"derivative": derivative})
}

// SyncStereoSplit queues the approved derivative for administrator-triggered cloud sync.
//
// @Summary      Sync stereo-split derivative
// @Tags         data-ops
// @Produce      json
// @Param        id   path      int  true  "Episode database ID"
// @Success      202  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/stereo-split/sync [post]
func (h *DataOpsHandler) SyncStereoSplit(c *gin.Context) {
	episodeID, ok := stereoSplitEpisodeID(c)
	if !ok {
		return
	}
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloud sync worker is not available", "code": "sync_unavailable"})
		return
	}
	if err := h.syncWorker.EnqueueStereoSplitManual(c.Request.Context(), episodeID); err != nil {
		status := http.StatusConflict
		code := "sync_rejected"
		message := "stereo split derivative is not eligible for cloud sync"
		if errors.Is(err, services.ErrSyncWorkerNotRunning) {
			status, code, message = http.StatusServiceUnavailable, "sync_unavailable", "cloud sync worker is not available"
		} else if errors.Is(err, services.ErrCloudPublishSourceLocked) {
			code, message = "cloud_source_locked", "episode cloud publish source is locked"
		} else if errors.Is(err, services.ErrSyncQueueFull) {
			status, code, message = http.StatusServiceUnavailable, "sync_unavailable", "cloud sync queue is temporarily full"
		} else if errors.Is(err, services.ErrSyncAlreadyInProgress) || errors.Is(err, services.ErrEpisodeAlreadyEnqueued) {
			code, message = "sync_active", "cloud sync is already active"
		} else {
			logger.Printf("[DATA_OPS] stereo split sync rejected: episode=%d err=%v", episodeID, err)
		}
		c.JSON(status, gin.H{"error": message, "code": code})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"episode_id": episodeID, "source_type": services.SyncSourceStereoSplit})
}

// UpdateStereoSplitSettingsRequest updates the audited settings used by the reconciler.
type UpdateStereoSplitSettingsRequest struct {
	ImageRef           string `json:"image_ref" binding:"required"`
	MaxConcurrent      int    `json:"max_concurrent" binding:"required"`
	ExpectedRevisionID int64  `json:"expected_revision_id" binding:"required"`
}

// GetStereoSplitSettings returns the current processing settings and safe limits.
//
// @Summary      Get stereo-split processing settings
// @Tags         data-ops
// @Produce      json
// @Success      200  {object}  map[string]any
// @Failure      503  {object}  map[string]string
// @Router       /data-ops/processing-settings/stereo-split [get]
func (h *DataOpsHandler) GetStereoSplitSettings(c *gin.Context) {
	if !h.ensureStereoSplitConfigured(c) {
		return
	}
	config, err := h.stereoSplit.CurrentImageConfig(c.Request.Context())
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"config":               config,
		"max_concurrent_limit": stereosplit.MaxConfigurableConcurrent,
	})
}

// UpdateStereoSplitSettings changes the image used by future generations and
// the live global submission concurrency limit.
//
// @Summary      Update stereo-split processing settings
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      UpdateStereoSplitSettingsRequest  true  "Processing settings and current revision"
// @Success      200      {object}  map[string]any
// @Failure      400      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Router       /data-ops/processing-settings/stereo-split [put]
func (h *DataOpsHandler) UpdateStereoSplitSettings(c *gin.Context) {
	if !h.ensureStereoSplitConfigured(c) {
		return
	}
	var request UpdateStereoSplitSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedRevisionID <= 0 ||
		request.MaxConcurrent < 1 || request.MaxConcurrent > stereosplit.MaxConfigurableConcurrent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image_ref, max_concurrent between 1 and 100, and positive expected_revision_id are required"})
		return
	}
	config, err := h.stereoSplit.UpdateImageConfig(
		c.Request.Context(),
		request.ImageRef,
		request.MaxConcurrent,
		request.ExpectedRevisionID,
		stereoSplitActor(c),
	)
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"config":               config,
		"max_concurrent_limit": stereosplit.MaxConfigurableConcurrent,
	})
}

// ListStereoSplitSettingsHistory returns the append-only settings audit history.
//
// @Summary      List stereo-split settings history
// @Tags         data-ops
// @Produce      json
// @Param        limit   query     int  false  "Max results"
// @Param        offset  query     int  false  "Pagination offset"
// @Success      200     {object}  map[string]any
// @Failure      400     {object}  map[string]string
// @Router       /data-ops/processing-settings/stereo-split/history [get]
func (h *DataOpsHandler) ListStereoSplitSettingsHistory(c *gin.Context) {
	if !h.ensureStereoSplitConfigured(c) {
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	rows, err := h.stereoSplit.ListImageConfigHistory(c.Request.Context(), pagination.Limit, pagination.Offset)
	if err != nil {
		writeStereoSplitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "limit": pagination.Limit, "offset": pagination.Offset})
}

func stereoSplitEpisodeID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return 0, false
	}
	return id, true
}

func stereoSplitActor(c *gin.Context) string {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return "admin"
	}
	if actor := strings.TrimSpace(claims.OperatorID); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(claims.Subject); actor != "" {
		return actor
	}
	return strings.TrimSpace(claims.Role)
}

func writeStereoSplitError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "stereo_split_error"
	message := "stereo split operation failed"
	switch {
	case errors.Is(err, stereosplit.ErrNotFound), errors.Is(err, stereosplit.ErrEpisodeNotFound):
		status, code, message = http.StatusNotFound, "not_found", "stereo split derivative or episode was not found"
	case errors.Is(err, stereosplit.ErrDisabled), errors.Is(err, stereosplit.ErrImageNotConfigured):
		status, code, message = http.StatusServiceUnavailable, "stereo_split_unavailable", "stereo split processing is not available"
	case errors.Is(err, stereosplit.ErrCloudSourceLocked):
		status, code, message = http.StatusConflict, "cloud_source_locked", "episode cloud publish source is locked"
	case errors.Is(err, stereosplit.ErrAlreadyDerived):
		status, code, message = http.StatusConflict, "already_derived", "episode is already derived"
	case errors.Is(err, stereosplit.ErrProcessingActive):
		status, code, message = http.StatusConflict, "processing_active", "stereo split processing is active"
	case errors.Is(err, stereosplit.ErrRetryRequired):
		status, code, message = http.StatusConflict, "retry_required", "stereo split retry is required"
	case errors.Is(err, stereosplit.ErrCleanupPending):
		status, code, message = http.StatusConflict, "orbit_delete_pending", "Orbit cleanup is pending"
	case errors.Is(err, stereosplit.ErrConfigChanged):
		status, code, message = http.StatusConflict, "config_changed", "stereo split processing configuration changed"
	case errors.Is(err, stereosplit.ErrInvalidMaxConcurrent):
		status, code, message = http.StatusBadRequest, "invalid_max_concurrent", "stereo split max concurrent must be between 1 and 100"
	case errors.Is(err, stereosplit.ErrSourceUnavailable):
		status, code, message = http.StatusUnprocessableEntity, "source_unavailable", "episode source is unavailable"
	case errors.Is(err, stereosplit.ErrQAUnavailable), errors.Is(err, stereosplit.ErrQANotApproved):
		status, code, message = http.StatusConflict, "qa_unavailable", "stereo split QA is unavailable or not approved"
	case errors.Is(err, stereosplit.ErrCloudSyncActive):
		status, code, message = http.StatusConflict, "sync_active", "stereo split cloud sync is active"
	default:
		if strings.Contains(strings.ToLower(err.Error()), "image") || strings.Contains(strings.ToLower(err.Error()), "repository") || strings.Contains(strings.ToLower(err.Error()), "sha256") {
			status, code, message = http.StatusBadRequest, "invalid_image_ref", "invalid stereo split image reference"
		} else {
			logger.Printf("[DATA_OPS] stereo split request failed: err=%v", err)
		}
	}
	c.JSON(status, gin.H{"error": message, "code": code})
}
