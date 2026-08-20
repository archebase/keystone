// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
)

type dataOpsLocalCleanupPreviewRow struct {
	Matched         int64 `db:"matched_count"`
	Eligible        int64 `db:"eligible_count"`
	AlreadyDeleted  int64 `db:"already_deleted_count"`
	NotSynced       int64 `db:"not_synced_count"`
	SyncActive      int64 `db:"sync_active_count"`
	Unsupported     int64 `db:"unsupported_source_count"`
	MissingSnapshot int64 `db:"missing_snapshot_count"`
}

// PreviewBulkLocalCleanup previews a filtered local MinIO cleanup operation.
func (h *DataOpsHandler) PreviewBulkLocalCleanup(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}
	preview, err := h.previewBulkLocalCleanup(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] local cleanup preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk local cleanup"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// BulkLocalCleanup queues filtered local MinIO cleanup jobs.
func (h *DataOpsHandler) BulkLocalCleanup(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	_, q, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}
	if h.localCleanup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local cleanup is not configured"})
		return
	}
	h.bulkRunMu.Lock()
	defer h.bulkRunMu.Unlock()
	if current, exists, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionLocalCleanup); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current bulk run"})
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "bulk local cleanup already running", "run_id": current.RunID, "status": current.Status})
		return
	}
	ids, err := h.selectBulkEpisodeIDs(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to select cleanup episodes"})
		return
	}
	run, err := h.createBulkRun(c.Request.Context(), dataOpsBulkRunActionLocalCleanup, int64(len(ids)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bulk local cleanup run"})
		return
	}
	if len(ids) > 0 {
		go h.runBulkLocalCleanup(run.RunID, ids)
	}
	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeActionResponse{Run: run, Message: fmt.Sprintf("%d episodes accepted for local cleanup", len(ids))})
}

func (h *DataOpsHandler) previewBulkLocalCleanup(ctx context.Context, q dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	var row dataOpsLocalCleanupPreviewRow
	query := `SELECT COUNT(*) AS matched_count,
		COALESCE(SUM(CASE WHEN e.cloud_synced = TRUE AND COALESCE(e.local_storage_status, 'available') <> 'deleted'
			AND NOT EXISTS (SELECT 1 FROM sync_logs sa WHERE sa.episode_id=e.id AND sa.status IN ('pending','in_progress'))
			AND EXISTS (SELECT 1 FROM sync_logs sc WHERE sc.episode_id=e.id AND sc.status='completed' AND JSON_UNQUOTE(JSON_EXTRACT(sc.source_snapshot,'$.backend'))='minio' AND JSON_UNQUOTE(JSON_EXTRACT(sc.source_snapshot,'$.source_type'))='original') THEN 1 ELSE 0 END),0) AS eligible_count,
		COALESCE(SUM(CASE WHEN COALESCE(e.local_storage_status,'available')='deleted' THEN 1 ELSE 0 END),0) AS already_deleted_count,
		COALESCE(SUM(CASE WHEN e.cloud_synced=FALSE THEN 1 ELSE 0 END),0) AS not_synced_count,
		COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM sync_logs sa WHERE sa.episode_id=e.id AND sa.status IN ('pending','in_progress')) THEN 1 ELSE 0 END),0) AS sync_active_count,
		COALESCE(SUM(CASE WHEN e.cloud_synced=TRUE AND EXISTS (SELECT 1 FROM sync_logs sc WHERE sc.episode_id=e.id AND sc.status='completed') AND NOT EXISTS (SELECT 1 FROM sync_logs sc2 WHERE sc2.episode_id=e.id AND sc2.status='completed' AND JSON_UNQUOTE(JSON_EXTRACT(sc2.source_snapshot,'$.backend'))='minio' AND JSON_UNQUOTE(JSON_EXTRACT(sc2.source_snapshot,'$.source_type'))='original') THEN 1 ELSE 0 END),0) AS unsupported_source_count,
		COALESCE(SUM(CASE WHEN e.cloud_synced=TRUE AND NOT EXISTS (SELECT 1 FROM sync_logs sc3 WHERE sc3.episode_id=e.id AND sc3.status='completed') THEN 1 ELSE 0 END),0) AS missing_snapshot_count ` + fromSQL + where
	if err := h.db.GetContext(ctx, &row, query, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}
	skipped := row.Matched - row.Eligible
	if skipped < 0 {
		skipped = 0
	}
	return DataOpsBulkEpisodePreviewResponse{Status: "ok", Action: dataOpsBulkRunActionLocalCleanup, MatchedCount: int(row.Matched), EligibleCount: int(row.Eligible), SkippedCount: int(skipped), SkippedBreakdown: []DataOpsBulkSkippedBreakdownItem{{Reason: "already_deleted", Count: int(row.AlreadyDeleted)}, {Reason: "not_synced", Count: int(row.NotSynced)}, {Reason: "sync_active", Count: int(row.SyncActive)}, {Reason: "unsupported_source", Count: int(row.Unsupported)}, {Reason: "missing_snapshot", Count: int(row.MissingSnapshot)}}, Warnings: []string{"仅删除 Keystone 当前配置的本地 MinIO 原始 MCAP，不影响云端数据"}}, nil
}

func (h *DataOpsHandler) runBulkLocalCleanup(runID string, ids []int64) {
	_, _ = h.markBulkRunRunning(context.Background(), runID)
	for _, episodeID := range ids {
		if h.bulkRunCancellationRequested(context.Background(), runID) {
			_, _ = h.markBulkRunCanceled(context.Background(), runID)
			return
		}
		_, err := h.localCleanup.RequestCleanupEpisode(context.Background(), episodeID, "admin")
		if err != nil {
			_, _ = h.incrementBulkRunCounts(context.Background(), runID, false, err)
			continue
		}
		_, _ = h.incrementBulkRunCounts(context.Background(), runID, true, nil)
	}
	_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusCompleted, "")
}

func (h *DataOpsHandler) incrementBulkRunCounts(ctx context.Context, runID string, success bool, actionErr error) (DataOpsBulkRunResponse, error) {
	failed := int64(0)
	passed := int64(0)
	if success {
		passed = 1
	} else {
		failed = 1
	}
	message := ""
	if actionErr != nil {
		message = strings.TrimSpace(actionErr.Error())
	}
	_, err := h.db.ExecContext(ctx, `UPDATE bulk_runs SET processed_count=processed_count+1, passed_count=passed_count+?, processing_failed_count=processing_failed_count+?, error_message=CASE WHEN ? <> '' THEN ? ELSE error_message END, updated_at=? WHERE run_id=? AND status IN (?,?)`, passed, failed, message, message, h.dataOpsBulkRunNow(), runID, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested)
	if err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return h.loadBulkRun(ctx, runID)
}
