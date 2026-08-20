// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
)

// LocalCleanupHandler exposes administrator-only cleanup of cloud-synced local data.
type LocalCleanupHandler struct {
	cleanup *services.LocalCleanupService
}

// NewLocalCleanupHandler creates the handler for the local storage cleanup module.
func NewLocalCleanupHandler(cleanup *services.LocalCleanupService) *LocalCleanupHandler {
	return &LocalCleanupHandler{cleanup: cleanup}
}

// RegisterAdminRoutes registers irreversible local-object cleanup routes.
func (h *LocalCleanupHandler) RegisterAdminRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/sync/episodes/:id/local-cleanup", h.RequestCleanup)
	apiV1.GET("/sync/episodes/:id/local-cleanup", h.GetCleanup)
}

// RequestCleanup queues deletion of the original MinIO object.
func (h *LocalCleanupHandler) RequestCleanup(c *gin.Context) {
	if h == nil || h.cleanup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local cleanup is not configured"})
		return
	}
	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	actor := "admin"
	if claims := middleware.GetClaims(c); claims != nil && claims.OperatorID != "" {
		actor = claims.OperatorID
	}
	job, err := h.cleanup.RequestCleanupEpisode(c.Request.Context(), episodeID, actor)
	if err == nil {
		c.JSON(http.StatusAccepted, job)
		return
	}
	h.writeCleanupError(c, err)
}

// GetCleanup returns the persisted cleanup job status.
func (h *LocalCleanupHandler) GetCleanup(c *gin.Context) {
	if h == nil || h.cleanup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local cleanup is not configured"})
		return
	}
	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	job, err := h.cleanup.GetCleanupJob(c.Request.Context(), episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "local cleanup job not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load local cleanup status"})
		return
	}
	c.JSON(http.StatusOK, job)
}

func (h *LocalCleanupHandler) writeCleanupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
	case errors.Is(err, services.ErrLocalCleanupNotSynced), errors.Is(err, services.ErrLocalCleanupSyncActive), errors.Is(err, services.ErrLocalCleanupUnsupportedSource), errors.Is(err, services.ErrLocalCleanupSourceUnavailable):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue local cleanup"})
	}
}

// CleanupEpisode deletes the original MinIO object after cloud sync has completed.
//
// @Summary      Clear synced episode local data
// @Description  Deletes only the original MinIO object retained by a completed cloud sync snapshot
// @Tags         sync
// @Produce      json
// @Param        id   path      int  true  "Episode ID"
// @Success      200  {object}  services.LocalCleanupResult
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /sync/episodes/{id}/local-cleanup [post]
func (h *LocalCleanupHandler) CleanupEpisode(c *gin.Context) {
	if h == nil || h.cleanup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local cleanup is not configured"})
		return
	}
	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	actor := "admin"
	if claims := middleware.GetClaims(c); claims != nil && claims.OperatorID != "" {
		actor = claims.OperatorID
	}
	result, err := h.cleanup.CleanupEpisode(c.Request.Context(), episodeID, actor)
	if err == nil {
		c.JSON(http.StatusOK, result)
		return
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
	case errors.Is(err, services.ErrLocalCleanupNotSynced),
		errors.Is(err, services.ErrLocalCleanupSyncActive),
		errors.Is(err, services.ErrLocalCleanupUnsupportedSource),
		errors.Is(err, services.ErrLocalCleanupSourceUnavailable):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clean up local object"})
	}
}
