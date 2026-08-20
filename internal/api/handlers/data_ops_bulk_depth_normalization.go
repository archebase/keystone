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
	"time"

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

	skipped := 0
	admittedIDs := make([]int64, 0, len(ids))
	for _, episodeID := range ids {
		if runCtx.Err() != nil || h.bulkRunCancellationRequested(context.Background(), runID) {
			break
		}
		_, _, startErr := h.depthNorm.Start(runCtx, episodeID, "bulk:"+runID)
		if startErr == nil {
			admittedIDs = append(admittedIDs, episodeID)
			continue
		}
		switch {
		case errors.Is(startErr, depthnorm.ErrAlreadyDerived),
			errors.Is(startErr, depthnorm.ErrProcessingActive),
			errors.Is(startErr, depthnorm.ErrCloudSourceLocked),
			errors.Is(startErr, depthnorm.ErrEpisodeNotFound):
			skipped++
		default:
			skipped++
			logger.Printf("[DEPTH-NORM] Bulk admission failed: run_id=%s episode=%d err=%v", runID, episodeID, startErr)
		}
	}

	// Admission is not completion: keep the bulk run active while the local
	// depth worker converts the admitted Episodes one at a time.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if runCtx.Err() != nil || h.bulkRunCancellationRequested(context.Background(), runID) {
			if _, err := h.markBulkRunCanceled(context.Background(), runID); err != nil {
				logger.Printf("[DEPTH-NORM] Bulk cancellation failed: run_id=%s err=%v", runID, err)
			}
			return
		}
		completed, err := h.refreshBulkDepthNormalizationRun(context.Background(), runID, admittedIDs, skipped)
		if err != nil {
			logger.Printf("[DEPTH-NORM] Bulk progress refresh failed: run_id=%s err=%v", runID, err)
		} else {
			h.publishBulkRunEvent("bulk_run_progress", completed)
			if completed.ProcessedCount >= completed.TotalCount {
				finalRun, err := h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusCompleted, "")
				if err != nil {
					logger.Printf("[DEPTH-NORM] Bulk completion failed: run_id=%s err=%v", runID, err)
					return
				}
				h.publishBulkRunEvent("bulk_run_completed", finalRun)
				return
			}
		}
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *DataOpsHandler) refreshBulkDepthNormalizationRun(ctx context.Context, runID string, episodeIDs []int64, skipped int) (DataOpsBulkRunResponse, error) {
	if len(episodeIDs) == 0 {
		now := h.dataOpsBulkRunNow()
		if _, err := h.db.ExecContext(ctx, `UPDATE bulk_runs SET processed_count=?, skipped_count=?, updated_at=? WHERE run_id=? AND action=?`, skipped, skipped, now, runID, dataOpsBulkRunActionDepthNormalization); err != nil {
			return DataOpsBulkRunResponse{}, err
		}
		return h.loadBulkRun(ctx, runID)
	}
	placeholders := make([]string, len(episodeIDs))
	args := make([]interface{}, 0, len(episodeIDs)+1)
	args = append(args, depthnorm.Kind)
	for i, id := range episodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	var counts struct {
		Succeeded int `db:"succeeded"`
		Failed    int `db:"failed"`
		Canceled  int `db:"canceled"`
		Active    int `db:"active"`
	}
	if err := h.db.GetContext(ctx, &counts, `
		SELECT
			COALESCE(SUM(CASE WHEN ed.processing_status='succeeded' THEN 1 ELSE 0 END),0) AS succeeded,
			COALESCE(SUM(CASE WHEN ed.processing_status='failed' THEN 1 ELSE 0 END),0) AS failed,
			COALESCE(SUM(CASE WHEN ed.processing_status='canceled' THEN 1 ELSE 0 END),0) AS canceled,
			COALESCE(SUM(CASE WHEN ed.processing_status IN ('queued','running','verifying') THEN 1 ELSE 0 END),0) AS active
		FROM episode_derivatives ed
		WHERE ed.kind=? AND ed.episode_id IN (`+strings.Join(placeholders, ",")+")", args...); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	processed := int64(skipped + counts.Succeeded + counts.Failed + counts.Canceled)
	now := h.dataOpsBulkRunNow()
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET processed_count=?, passed_count=?, processing_failed_count=?, skipped_count=?, updated_at=?
		WHERE run_id=? AND action=? AND status IN (?,?)
	`, processed, counts.Succeeded, counts.Failed, skipped+counts.Canceled, now, runID, dataOpsBulkRunActionDepthNormalization, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return h.loadBulkRun(ctx, runID)
}
