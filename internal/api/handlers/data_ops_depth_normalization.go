// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services/depthnorm"
)

type dataOpsDepthNormalizer interface {
	Start(ctx context.Context, episodeID int64, actor string) (depthnorm.Derivative, bool, error)
}

type dataOpsDepthNormalizationResponse struct {
	ID               int64   `json:"id"`
	EpisodeID        int64   `json:"episode_id"`
	Generation       int     `json:"generation"`
	ProcessingStatus string  `json:"processing_status"`
	QAStatus         string  `json:"qa_status"`
	McapPath         *string `json:"mcap_path"`
	Checksum         *string `json:"checksum"`
	FileSizeBytes    *int64  `json:"file_size_bytes"`
	ProcessingError  *string `json:"processing_error"`
}

type dataOpsDepthNormalizationRow struct {
	ID               int64          `db:"id" json:"id"`
	EpisodeID        int64          `db:"episode_id" json:"episode_id"`
	Generation       int            `db:"generation" json:"generation"`
	ProcessingStatus string         `db:"processing_status" json:"processing_status"`
	QAStatus         string         `db:"qa_status" json:"qa_status"`
	McapPath         sql.NullString `db:"mcap_path" json:"mcap_path"`
	Checksum         sql.NullString `db:"checksum" json:"checksum"`
	FileSizeBytes    sql.NullInt64  `db:"file_size_bytes" json:"file_size_bytes"`
	ProcessingError  sql.NullString `db:"processing_error" json:"processing_error"`
}

// SetDepthNormalizer wires edge-local depth normalization processing.
func (h *DataOpsHandler) SetDepthNormalizer(manager dataOpsDepthNormalizer) {
	if h != nil {
		h.depthNorm = manager
	}
}

// GetDepthNormalization returns the local depth normalization derivative state.
// @Summary      Get depth normalization state
// @Description  Returns the ZJ-WA1-D Episode's local depth normalization derivative, if present.
// @Tags         data-ops
// @Produce      json
// @Param        id path int true "Episode numeric ID"
// @Success      200 {object} dataOpsDepthNormalizationResponse
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/depth-normalization [get]
func (h *DataOpsHandler) GetDepthNormalization(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	var row dataOpsDepthNormalizationRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT id, episode_id, generation, processing_status, qa_status,
		       mcap_path, checksum, file_size_bytes, processing_error
		FROM episode_derivatives
		WHERE episode_id=? AND kind='depth_normalization'
	`, episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "depth normalization derivative not found"})
		return
	}
	if err != nil {
		logger.Printf("[DATA_OPS] depth normalization lookup failed: episode=%d err=%v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load depth normalization"})
		return
	}
	c.JSON(http.StatusOK, dataOpsDepthNormalizationResponse{
		ID: row.ID, EpisodeID: row.EpisodeID, Generation: row.Generation,
		ProcessingStatus: row.ProcessingStatus, QAStatus: row.QAStatus,
		McapPath: nullableString(row.McapPath), Checksum: nullableString(row.Checksum),
		FileSizeBytes: nullableInt64(row.FileSizeBytes), ProcessingError: nullableString(row.ProcessingError),
	})
}

// StartDepthNormalization admits or retries one local derivative generation.
// @Summary      Start depth normalization
// @Description  Creates or retries an edge-local ZJ-WA1-D depth normalization derivative.
// @Tags         data-ops
// @Produce      json
// @Param        id path int true "Episode numeric ID"
// @Success      202 {object} map[string]interface{}
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data-ops/episodes/{id}/derivatives/depth-normalization [post]
func (h *DataOpsHandler) StartDepthNormalization(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	if h.depthNorm == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "depth normalization is unavailable"})
		return
	}
	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	derivative, started, err := h.depthNorm.Start(c.Request.Context(), episodeID, "data-ops")
	if err != nil {
		switch {
		case errors.Is(err, depthnorm.ErrAlreadyDerived):
			c.JSON(http.StatusConflict, gin.H{"error": "episode is already depth normalized"})
		case errors.Is(err, depthnorm.ErrProcessingActive):
			c.JSON(http.StatusConflict, gin.H{"error": "depth normalization processing is active"})
		case errors.Is(err, depthnorm.ErrCloudSourceLocked):
			c.JSON(http.StatusConflict, gin.H{"error": "episode cloud source is locked"})
		case errors.Is(err, depthnorm.ErrEpisodeNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		default:
			logger.Printf("[DATA_OPS] depth normalization start failed: episode=%d err=%v", episodeID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start depth normalization"})
		}
		return
	}
	status := http.StatusAccepted
	if !started {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{
		"derivative": derivative,
		"started":    started,
	})
}
