// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services/autosync"
	"github.com/gin-gonic/gin"
)

type autoSyncSettingsManager interface {
	CurrentConfig(ctx context.Context) (autosync.Config, error)
	UpdateConfig(ctx context.Context, enabled bool, expectedRevisionID int64, actor string) (autosync.Config, error)
}

// AutoSyncSettingsHandler exposes the global automatic processing setting.
type AutoSyncSettingsHandler struct {
	manager autoSyncSettingsManager
}

// UpdateAutoSyncSettingsRequest appends one automatic-sync setting revision.
type UpdateAutoSyncSettingsRequest struct {
	Enabled            *bool `json:"enabled"`
	ExpectedRevisionID int64 `json:"expected_revision_id"`
}

// NewAutoSyncSettingsHandler constructs the system-setting handler.
func NewAutoSyncSettingsHandler(manager autoSyncSettingsManager) *AutoSyncSettingsHandler {
	return &AutoSyncSettingsHandler{manager: manager}
}

// RegisterRoutes registers administrator-only routes on the supplied group.
func (h *AutoSyncSettingsHandler) RegisterRoutes(api *gin.RouterGroup) {
	api.GET("/processing-settings/auto-sync", h.Get)
	api.PUT("/processing-settings/auto-sync", h.Update)
}

// Get returns the current automatic-sync setting.
//
// @Summary      Get automatic-sync setting
// @Tags         processing-settings
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  map[string]any
// @Failure      500  {object}  map[string]string
// @Router       /processing-settings/auto-sync [get]
func (h *AutoSyncSettingsHandler) Get(c *gin.Context) {
	if h == nil || h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "automatic sync is not available"})
		return
	}
	config, err := h.manager.CurrentConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": config})
}

// Update changes whether future supported uploads enter automatic processing.
//
// @Summary      Update automatic-sync setting
// @Tags         processing-settings
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request  body  UpdateAutoSyncSettingsRequest  true  "Setting and current revision"
// @Success      200  {object}  map[string]any
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /processing-settings/auto-sync [put]
func (h *AutoSyncSettingsHandler) Update(c *gin.Context) {
	if h == nil || h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "automatic sync is not available"})
		return
	}
	var request UpdateAutoSyncSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.Enabled == nil || request.ExpectedRevisionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled and positive expected_revision_id are required"})
		return
	}
	config, err := h.manager.UpdateConfig(
		c.Request.Context(),
		*request.Enabled,
		request.ExpectedRevisionID,
		autoSyncActor(c),
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": config})
}

func autoSyncActor(c *gin.Context) string {
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
	if actor := strings.TrimSpace(claims.Role); actor != "" {
		return actor
	}
	return "admin"
}

func (h *AutoSyncSettingsHandler) writeError(c *gin.Context, err error) {
	if errors.Is(err, autosync.ErrConfigChanged) {
		c.JSON(http.StatusConflict, gin.H{
			"error": "automatic sync configuration changed",
			"code":  "config_changed",
		})
		return
	}
	logger.Printf("[AUTO_SYNC] Settings request failed: %v", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "automatic sync settings operation failed"})
}
