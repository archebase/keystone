// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services/depthnorm"
	"github.com/gin-gonic/gin"
)

type dataOpsDepthNormalizationPreviewRow struct {
	EpisodeID        int64          `db:"episode_id"`
	DeviceType       sql.NullString `db:"device_type"`
	CloudSynced      bool           `db:"cloud_synced"`
	CloudSource      sql.NullString `db:"cloud_publish_source"`
	ProcessingStatus sql.NullString `db:"processing_status"`
	QAStatus         sql.NullString `db:"qa_status"`
	SyncEvidence     bool           `db:"sync_evidence"`
}

const (
	depthNormBulkReasonWrongDevice       = "wrong_device_type"
	depthNormBulkReasonAlreadyNormalized = "already_normalized"
	depthNormBulkReasonProcessingActive  = "processing_active"
	depthNormBulkReasonCloudSourceLocked = "cloud_source_locked"
)

// PreviewBulkDepthNormalization estimates which selected ZJ-WA1-D Episodes can
// be admitted to the edge-local depth-normalization queue.
//
// @Summary      Preview bulk depth normalization
// @Description  Estimates matched, eligible, and skipped Episodes before queued local depth normalization.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request body DataOpsBulkEpisodeActionRequest false "Bulk preview filters"
// @Success      200 {object} DataOpsBulkEpisodePreviewResponse
// @Failure      400 {object} map[string]string
// @Failure      503 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data-ops/episodes/bulk-depth-normalization/preview [post]
func (h *DataOpsHandler) PreviewBulkDepthNormalization(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) || !h.ensureDepthNormalizationConfigured(c) {
		return
	}
	_, query, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}
	preview, err := h.previewBulkDepthNormalization(c.Request.Context(), query)
	if err != nil {
		logger.Printf("[DEPTH-NORM] Bulk preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk depth normalization"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// BulkDepthNormalization queues depth normalization for a durable selected
// snapshot. The local executor remains bounded to one active conversion.
//
// @Summary      Run bulk depth normalization
// @Description  Creates a durable bulk run and admits selected ZJ-WA1-D Episodes to the local depth-normalization queue.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request body DataOpsBulkEpisodeActionRequest true "Bulk filters, selection, and confirmation"
// @Success      202 {object} DataOpsBulkEpisodeActionResponse
// @Failure      400 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      503 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data-ops/episodes/bulk-depth-normalization [post]
func (h *DataOpsHandler) BulkDepthNormalization(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) || !h.ensureDepthNormalizationConfigured(c) {
		return
	}
	_, query, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}

	h.bulkRunMu.Lock()
	defer h.bulkRunMu.Unlock()
	if current, exists, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionDepthNormalization); err != nil {
		logger.Printf("[DEPTH-NORM] Current bulk run lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current depth normalization run"})
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "bulk depth normalization already running",
			"run_id": current.RunID,
			"status": current.Status,
		})
		return
	}

	ids, err := h.selectBulkEpisodeIDs(c.Request.Context(), query)
	if err != nil {
		logger.Printf("[DEPTH-NORM] Bulk ID snapshot failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to select data operation episodes"})
		return
	}
	run, err := h.createBulkRun(c.Request.Context(), dataOpsBulkRunActionDepthNormalization, int64(len(ids)))
	if err != nil {
		logger.Printf("[DEPTH-NORM] Bulk run create failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bulk depth normalization run"})
		return
	}
	logger.Printf("[DEPTH-NORM] Bulk admission accepted: run_id=%s total=%d", run.RunID, len(ids))
	if len(ids) > 0 {
		go h.runBulkDepthNormalization(run.RunID, ids)
	}
	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeActionResponse{
		Run:     run,
		Message: fmt.Sprintf("%d episodes accepted for bulk depth-normalization admission", len(ids)),
	})
}

func (h *DataOpsHandler) ensureDepthNormalizationConfigured(c *gin.Context) bool {
	if h == nil || h.depthNorm == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "depth normalization is unavailable"})
		return false
	}
	return true
}

func (h *DataOpsHandler) previewBulkDepthNormalization(ctx context.Context, query dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL() + `
		LEFT JOIN episode_derivatives d
		  ON d.episode_id=e.id AND d.kind='depth_normalization'
	`
	where, args := buildDataOpsEpisodeWhere(query)
	rows := []dataOpsDepthNormalizationPreviewRow{}
	if err := h.db.SelectContext(ctx, &rows, `
		SELECT e.id AS episode_id, r.device_type, e.cloud_synced, e.cloud_publish_source,
		       d.processing_status, d.qa_status,
		       CASE WHEN EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id=e.id) THEN 1 ELSE 0 END AS sync_evidence
	`+fromSQL+where, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}

	reasons := map[string]int{}
	eligible := 0
	for _, row := range rows {
		reason, ok := depthNormalizationPreviewDecision(row)
		if ok {
			eligible++
			continue
		}
		reasons[reason]++
	}
	order := []string{
		depthNormBulkReasonWrongDevice,
		depthNormBulkReasonAlreadyNormalized,
		depthNormBulkReasonCloudSourceLocked,
		depthNormBulkReasonProcessingActive,
	}
	breakdown := make([]DataOpsBulkSkippedBreakdownItem, 0, len(reasons))
	for _, reason := range order {
		if count := reasons[reason]; count > 0 {
			breakdown = append(breakdown, DataOpsBulkSkippedBreakdownItem{Reason: reason, Count: count})
		}
	}
	return DataOpsBulkEpisodePreviewResponse{
		Status:           "preview",
		Action:           dataOpsBulkRunActionDepthNormalization,
		MatchedCount:     len(rows),
		EligibleCount:    eligible,
		SkippedCount:     len(rows) - eligible,
		SkippedBreakdown: breakdown,
		Warnings:         []string{"eligible episodes are queued locally; the edge worker processes one conversion at a time"},
	}, nil
}

func depthNormalizationPreviewDecision(row dataOpsDepthNormalizationPreviewRow) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(row.DeviceType.String), depthnorm.DeviceTypeZJWA1D) {
		return depthNormBulkReasonWrongDevice, false
	}
	if row.CloudSynced || row.SyncEvidence || strings.TrimSpace(row.CloudSource.String) == "original" {
		return depthNormBulkReasonCloudSourceLocked, false
	}
	switch strings.TrimSpace(row.ProcessingStatus.String) {
	case "succeeded":
		return depthNormBulkReasonAlreadyNormalized, false
	case "queued", "running", "verifying":
		return depthNormBulkReasonProcessingActive, false
	case "", "failed", "canceled":
		return "", true
	default:
		return depthNormBulkReasonProcessingActive, false
	}
}

func (h *DataOpsHandler) runBulkDepthNormalization(runID string, ids []int64) {
	runCtx, finishRun := h.beginBulkRunExecution(runID)
	defer finishRun()
	started, err := h.markBulkRunRunning(context.Background(), runID)
	if err != nil {
		logger.Printf("[DEPTH-NORM] Bulk run start failed: run_id=%s err=%v", runID, err)
		return
	}
	if started.Status == dataOpsBulkRunStatusCancelRequested {
		if _, err := h.markBulkRunCanceled(context.Background(), runID); err != nil {
			logger.Printf("[DEPTH-NORM] Bulk run pre-start cancellation failed: run_id=%s err=%v", runID, err)
		}
		return
	}

	for _, episodeID := range ids {
		if runCtx.Err() != nil || !h.reserveBulkRunItem(runID) {
			break
		}
		outcome := dataOpsBulkQAEpisodePassed
		_, _, startErr := h.depthNorm.Start(runCtx, episodeID, "bulk:"+runID)
		if startErr != nil {
			switch {
			case errors.Is(startErr, depthnorm.ErrAlreadyDerived),
				errors.Is(startErr, depthnorm.ErrProcessingActive),
				errors.Is(startErr, depthnorm.ErrCloudSourceLocked),
				errors.Is(startErr, depthnorm.ErrEpisodeNotFound):
				outcome = dataOpsBulkQAEpisodeSkipped
			default:
				outcome = dataOpsBulkQAEpisodeProcessingFailed
				logger.Printf("[DEPTH-NORM] Bulk admission failed: run_id=%s episode=%d err=%v", runID, episodeID, startErr)
			}
		}
		run, countErr := h.incrementBulkQARunCounts(context.Background(), runID, outcome)
		if countErr != nil {
			logger.Printf("[DEPTH-NORM] Bulk progress update failed: run_id=%s episode=%d err=%v", runID, episodeID, countErr)
			continue
		}
		h.publishBulkRunEvent("bulk_run_progress", run)
	}

	if runCtx.Err() != nil || h.bulkRunCancellationRequested(context.Background(), runID) {
		if _, err := h.markBulkRunCanceled(context.Background(), runID); err != nil {
			logger.Printf("[DEPTH-NORM] Bulk cancellation failed: run_id=%s err=%v", runID, err)
		}
		return
	}
	finalRun, err := h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusCompleted, "")
	if err != nil {
		logger.Printf("[DEPTH-NORM] Bulk completion failed: run_id=%s err=%v", runID, err)
		return
	}
	h.publishBulkRunEvent("bulk_run_completed", finalRun)
}
