// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
)

// CalibrationBackfillRequest describes one explicit Episode calibration backfill request.
type CalibrationBackfillRequest struct {
	EpisodeID    int64  `json:"episode_id" binding:"required"`
	CameraSerial string `json:"camera_serial" binding:"required,trimmednotblank"`
}

// BackfillEpisodeCalibration uploads and binds the selected camera calibration without re-uploading MCAP.
// @Summary Backfill Episode calibration
// @Tags data-ops
// @Accept json
// @Produce json
// @Param request body CalibrationBackfillRequest true "Episode and camera calibration"
// @Success 200 {object} services.CalibrationBackfillResult
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Router /data-ops/episodes/calibration-backfill [post]
func (h *DataOpsHandler) BackfillEpisodeCalibration(c *gin.Context) {
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cloud sync worker is not available"})
		return
	}
	var request CalibrationBackfillRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.EpisodeID <= 0 || strings.TrimSpace(request.CameraSerial) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "positive episode_id and camera_serial are required"})
		return
	}
	result, err := h.syncWorker.BackfillEpisodeCalibration(c.Request.Context(), request.EpisodeID, request.CameraSerial)
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "has not been synced") || strings.Contains(err.Error(), "already bound") {
			status = http.StatusConflict
		} else if errors.Is(err, services.ErrSyncWorkerNotRunning) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
