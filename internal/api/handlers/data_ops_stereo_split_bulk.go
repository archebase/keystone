// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services/stereosplit"
	"github.com/gin-gonic/gin"
)

const stereoSplitBulkPageSize = 200

type dataOpsStereoSplitPreviewRow struct {
	EpisodeID         int64          `db:"episode_id"`
	StorageBackend    sql.NullString `db:"storage_backend"`
	Metadata          sql.NullString `db:"metadata"`
	CloudSource       sql.NullString `db:"cloud_publish_source"`
	ProcessingStatus  sql.NullString `db:"processing_status"`
	OrbitDeleteStatus sql.NullString `db:"orbit_delete_status"`
	SyncEvidence      bool           `db:"sync_evidence"`
}

// PreviewBulkStereoSplit estimates admission using the same stable reason
// order as materialization. Membership is re-evaluated when execution scans.
//
// @Summary      Preview bulk stereo split
// @Description  Estimates matched, eligible, and skipped Episode counts; execution rechecks eligibility while paging.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  false  "Bulk preview filters"
// @Success      200      {object}  DataOpsBulkEpisodePreviewResponse
// @Failure      400      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-stereo-split/preview [post]
func (h *DataOpsHandler) PreviewBulkStereoSplit(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) || !h.ensureStereoSplitReady(c) {
		return
	}
	_, query, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}
	preview, err := h.previewBulkStereoSplit(c.Request.Context(), query)
	if err != nil {
		logger.Printf("[STEREO_SPLIT] Bulk preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk stereo split"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// BulkStereoSplit creates a durable run whose members are materialized in
// pages and queued through the single-Episode admission transaction.
//
// @Summary      Run bulk stereo split
// @Description  Creates a durable paged bulk run and admits each eligible Episode to the bounded stereo-split queue.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  true  "Bulk filters, selection, and confirmation"
// @Success      202      {object}  DataOpsBulkEpisodeActionResponse
// @Failure      400      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-stereo-split [post]
func (h *DataOpsHandler) BulkStereoSplit(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) || !h.ensureStereoSplitReady(c) {
		return
	}
	request, query, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}

	h.bulkRunMu.Lock()
	defer h.bulkRunMu.Unlock()
	if current, exists, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionStereoSplit); err != nil {
		logger.Printf("[STEREO_SPLIT] Current bulk run lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current stereo split run"})
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "bulk stereo split already running",
			"run_id": current.RunID,
			"status": current.Status,
		})
		return
	}

	preview, err := h.previewBulkStereoSplit(c.Request.Context(), query)
	if err != nil {
		logger.Printf("[STEREO_SPLIT] Bulk execute preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk stereo split"})
		return
	}
	snapshotMax, err := h.stereoSplitSnapshotMax(c.Request.Context(), query)
	if err != nil {
		logger.Printf("[STEREO_SPLIT] Bulk snapshot boundary failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create stereo split snapshot boundary"})
		return
	}
	run, err := h.createStereoSplitBulkRun(c.Request.Context(), request, preview, snapshotMax)
	if err != nil {
		logger.Printf("[STEREO_SPLIT] Bulk run creation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bulk stereo split run"})
		return
	}
	if run.Status != dataOpsBulkRunStatusCompleted {
		h.startStereoSplitBulkRun(run.RunID)
	}
	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeActionResponse{
		Run:     run,
		Message: fmt.Sprintf("estimated %d episodes accepted for stereo split materialization", preview.MatchedCount),
	})
}

func (h *DataOpsHandler) ensureStereoSplitReady(c *gin.Context) bool {
	if !h.ensureStereoSplitConfigured(c) {
		return false
	}
	image, err := h.stereoSplit.CurrentImageConfig(c.Request.Context())
	if err != nil {
		writeStereoSplitError(c, err)
		return false
	}
	if strings.TrimSpace(image.ImageRef) == "" {
		writeStereoSplitError(c, stereosplit.ErrImageNotConfigured)
		return false
	}
	return true
}

func (h *DataOpsHandler) ensureStereoSplitConfigured(c *gin.Context) bool {
	if h == nil || h.stereoSplit == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "stereo split processing is not configured"})
		return false
	}
	return true
}

func (h *DataOpsHandler) previewBulkStereoSplit(ctx context.Context, query dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL() + `
		LEFT JOIN episode_derivatives d
		  ON d.episode_id = e.id AND d.kind = 'stereo_split'
	`
	where, args := buildDataOpsEpisodeWhere(query)
	rows := []dataOpsStereoSplitPreviewRow{}
	if err := h.db.SelectContext(ctx, &rows, `
		SELECT e.id AS episode_id, e.storage_backend, e.metadata, e.cloud_publish_source,
		       d.processing_status, d.orbit_delete_status,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id
		       ) THEN 1 ELSE 0 END AS sync_evidence
	`+fromSQL+where, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}

	reasons := map[string]int{}
	eligible := 0
	for _, row := range rows {
		reason, isEligible := stereoSplitPreviewDecision(row)
		if isEligible {
			eligible++
			continue
		}
		reasons[reason]++
	}
	order := []string{
		stereosplit.BulkReasonAlreadyDerived,
		stereosplit.BulkReasonCloudSourceLocked,
		stereosplit.BulkReasonProcessingActive,
		stereosplit.BulkReasonOrbitDeletePending,
		stereosplit.BulkReasonSourceUnavailable,
	}
	breakdown := make([]DataOpsBulkSkippedBreakdownItem, 0, len(reasons))
	for _, reason := range order {
		if count := reasons[reason]; count > 0 {
			breakdown = append(breakdown, DataOpsBulkSkippedBreakdownItem{Reason: reason, Count: count})
		}
	}
	return DataOpsBulkEpisodePreviewResponse{
		Status:           "preview",
		Action:           stereosplit.Kind,
		MatchedCount:     len(rows),
		EligibleCount:    eligible,
		SkippedCount:     len(rows) - eligible,
		SkippedBreakdown: breakdown,
		Warnings:         []string{"preview is an estimate; eligibility is checked again while members are materialized"},
	}, nil
}

func stereoSplitPreviewDecision(row dataOpsStereoSplitPreviewRow) (string, bool) {
	status := strings.TrimSpace(row.ProcessingStatus.String)
	if status == stereosplit.ProcessingSucceeded {
		return stereosplit.BulkReasonAlreadyDerived, false
	}
	cloudSource := strings.TrimSpace(row.CloudSource.String)
	if cloudSource == stereosplit.CloudSourceOriginal || (row.SyncEvidence && cloudSource != stereosplit.CloudSourceStereoSplit) {
		return stereosplit.BulkReasonCloudSourceLocked, false
	}
	switch status {
	case stereosplit.ProcessingQueued, stereosplit.ProcessingSubmitting, stereosplit.ProcessingPending,
		stereosplit.ProcessingRunning, stereosplit.ProcessingVerifying:
		return stereosplit.BulkReasonProcessingActive, false
	case stereosplit.ProcessingFailed, stereosplit.ProcessingCanceled:
		deleteStatus := strings.TrimSpace(row.OrbitDeleteStatus.String)
		if deleteStatus != stereosplit.DeleteCompleted && deleteStatus != stereosplit.DeleteNotRequired {
			return stereosplit.BulkReasonOrbitDeletePending, false
		}
	}
	if err := stereosplit.ValidateSource(row.StorageBackend.String, row.Metadata.String); err != nil {
		return stereosplit.BulkReasonSourceUnavailable, false
	}
	if status == stereosplit.ProcessingFailed || status == stereosplit.ProcessingCanceled {
		return stereosplit.BulkReasonEligibleRetry, true
	}
	return stereosplit.BulkReasonEligible, true
}

func (h *DataOpsHandler) stereoSplitSnapshotMax(ctx context.Context, query dataOpsEpisodeQuery) (int64, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(query)
	var maxID sql.NullInt64
	if err := h.db.GetContext(ctx, &maxID, "SELECT MAX(e.id) "+fromSQL+where, args...); err != nil {
		return 0, err
	}
	return maxID.Int64, nil
}

func (h *DataOpsHandler) createStereoSplitBulkRun(
	ctx context.Context,
	request DataOpsBulkEpisodeActionRequest,
	preview DataOpsBulkEpisodePreviewResponse,
	snapshotMax int64,
) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	runID, err := defaultDataOpsBulkRunID(dataOpsBulkRunActionStereoSplit, now)
	if err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("encode stereo split bulk request: %w", err)
	}
	previewCounts := map[string]int64{
		"matched":  int64(preview.MatchedCount),
		"eligible": int64(preview.EligibleCount),
		"skipped":  int64(preview.SkippedCount),
	}
	previewJSON, err := json.Marshal(previewCounts)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("encode stereo split preview counts: %w", err)
	}
	status := dataOpsBulkRunStatusQueued
	var startedAt, finishedAt, materializedAt, countsFrozenAt any
	var finalCounts any
	if preview.MatchedCount == 0 {
		status = dataOpsBulkRunStatusCompleted
		startedAt, finishedAt, materializedAt, countsFrozenAt = now, now, now, now
		finalJSON, marshalErr := json.Marshal(stereoSplitEmptyFinalCounts(preview.MatchedCount))
		if marshalErr != nil {
			return DataOpsBulkRunResponse{}, marshalErr
		}
		finalCounts = string(finalJSON)
	}
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO bulk_runs (
			run_id, action, status, total_count, processed_count, passed_count,
			qa_failed_count, processing_failed_count, skipped_count, canceled_count,
			error_message, request, preview_counts, snapshot_max_episode_id,
			materialize_cursor, materialized_at, final_counts, counts_frozen_at,
			started_at, finished_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0, '', ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
	`, runID, dataOpsBulkRunActionStereoSplit, status, preview.MatchedCount,
		string(requestJSON), string(previewJSON), snapshotMax, materializedAt, finalCounts,
		countsFrozenAt, startedAt, finishedAt, now, now); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return h.loadBulkRun(ctx, runID)
}

func stereoSplitEmptyFinalCounts(previewEstimated int) map[string]int64 {
	return map[string]int64{
		"preview_estimated":         int64(previewEstimated),
		"materialized_items":        0,
		"admitted":                  0,
		"skipped":                   0,
		"canceled_before_admission": 0,
		"queued":                    0,
		"pending":                   0,
		"running":                   0,
		"verifying":                 0,
		"succeeded":                 0,
		"failed":                    0,
		"processing_canceled":       0,
		"qa_approved":               0,
		"qa_failed":                 0,
		"orbit_delete_completed":    0,
		"orbit_delete_not_required": 0,
	}
}

func (h *DataOpsHandler) loadStereoSplitBulkRunMetadata(ctx context.Context, runID string, response *DataOpsBulkRunResponse) error {
	if response == nil {
		return nil
	}
	var row struct {
		PreviewCounts  sql.NullString `db:"preview_counts"`
		FinalCounts    sql.NullString `db:"final_counts"`
		MaterializedAt sql.NullTime   `db:"materialized_at"`
		CountsFrozenAt sql.NullTime   `db:"counts_frozen_at"`
	}
	if err := h.db.GetContext(ctx, &row, `
		SELECT preview_counts, final_counts, materialized_at, counts_frozen_at
		FROM bulk_runs WHERE run_id = ? AND action = ?
	`, runID, dataOpsBulkRunActionStereoSplit); err != nil {
		return err
	}
	if row.PreviewCounts.Valid {
		_ = json.Unmarshal([]byte(row.PreviewCounts.String), &response.PreviewCounts)
	}
	if row.FinalCounts.Valid {
		_ = json.Unmarshal([]byte(row.FinalCounts.String), &response.FinalCounts)
	}
	if row.MaterializedAt.Valid {
		value := row.MaterializedAt.Time.UTC()
		response.MaterializedAt = &value
	}
	if row.CountsFrozenAt.Valid {
		value := row.CountsFrozenAt.Time.UTC()
		response.CountsFrozenAt = &value
	}
	return nil
}

type stereoSplitBulkStoredRun struct {
	Request      sql.NullString `db:"request"`
	SnapshotMax  sql.NullInt64  `db:"snapshot_max_episode_id"`
	Cursor       sql.NullInt64  `db:"materialize_cursor"`
	Status       string         `db:"status"`
	Materialized sql.NullTime   `db:"materialized_at"`
	ErrorMessage sql.NullString `db:"error_message"`
}

func (h *DataOpsHandler) startStereoSplitBulkRun(runID string) {
	h.stereoBulkMu.Lock()
	if h.stereoBulkRuns == nil {
		h.stereoBulkRuns = make(map[string]struct{})
	}
	if _, exists := h.stereoBulkRuns[runID]; exists {
		h.stereoBulkMu.Unlock()
		return
	}
	h.stereoBulkRuns[runID] = struct{}{}
	h.stereoBulkMu.Unlock()
	go func() {
		defer func() {
			h.stereoBulkMu.Lock()
			delete(h.stereoBulkRuns, runID)
			h.stereoBulkMu.Unlock()
		}()
		h.runStereoSplitBulk(runID)
	}()
}

func (h *DataOpsHandler) runStereoSplitBulk(runID string) {
	ctx := context.Background()
	if _, err := h.markBulkRunRunning(ctx, runID); err != nil {
		logger.Printf("[STEREO_SPLIT] Mark bulk run running failed: run_id=%s err=%v", runID, err)
		return
	}
	for {
		stored, err := h.loadStereoSplitStoredRun(ctx, runID)
		if err != nil {
			logger.Printf("[STEREO_SPLIT] Load bulk materializer state failed: run_id=%s err=%v", runID, err)
			return
		}
		if stored.Materialized.Valid {
			break
		}
		if stored.Status == dataOpsBulkRunStatusCancelRequested {
			if err := h.closeCanceledStereoSplitMaterialization(ctx, runID); err != nil {
				logger.Printf("[STEREO_SPLIT] Close canceled materialization failed: run_id=%s err=%v", runID, err)
			}
			break
		}
		var request DataOpsBulkEpisodeActionRequest
		if !stored.Request.Valid || json.Unmarshal([]byte(stored.Request.String), &request) != nil {
			h.failStereoSplitMaterialization(ctx, runID, "stored bulk request is invalid")
			break
		}
		query, err := dataOpsQueryFromBulkRequest(request)
		if err != nil {
			h.failStereoSplitMaterialization(ctx, runID, err.Error())
			break
		}
		ids, err := h.selectStereoSplitMaterializePage(ctx, query, stored.Cursor.Int64, stored.SnapshotMax.Int64)
		if err != nil {
			h.failStereoSplitMaterialization(ctx, runID, err.Error())
			break
		}
		if len(ids) == 0 {
			if _, err := h.db.ExecContext(ctx, `
				UPDATE bulk_runs SET materialized_at = COALESCE(materialized_at, ?), updated_at = ?
				WHERE run_id = ? AND action = ?
			`, h.dataOpsBulkRunNow(), h.dataOpsBulkRunNow(), runID, dataOpsBulkRunActionStereoSplit); err != nil {
				logger.Printf("[STEREO_SPLIT] Finish materialization failed: run_id=%s err=%v", runID, err)
				return
			}
			break
		}
		for _, episodeID := range ids {
			stored, err = h.loadStereoSplitStoredRun(ctx, runID)
			if err != nil || stored.Status == dataOpsBulkRunStatusCancelRequested {
				if err := h.closeCanceledStereoSplitMaterialization(ctx, runID); err != nil {
					logger.Printf("[STEREO_SPLIT] Cancel materialization failed: run_id=%s err=%v", runID, err)
				}
				break
			}
			if _, err := h.stereoSplit.AdmitBulk(ctx, runID, episodeID, "bulk:"+runID); err != nil {
				h.failStereoSplitMaterialization(ctx, runID, err.Error())
				break
			}
		}
		if run, _, err := h.refreshStereoSplitBulkRun(ctx, runID); err == nil {
			h.publishBulkRunEvent("bulk_run_progress", run)
		}
	}

	for {
		run, terminal, err := h.refreshStereoSplitBulkRun(ctx, runID)
		if err != nil {
			logger.Printf("[STEREO_SPLIT] Refresh bulk run failed: run_id=%s err=%v", runID, err)
			time.Sleep(time.Second)
			continue
		}
		if terminal {
			if event, ok := dataOpsBulkRunTerminalEventName(run.Status); ok {
				h.publishBulkRunEvent(event, run)
			}
			return
		}
		h.publishBulkRunEvent("bulk_run_progress", run)
		time.Sleep(500 * time.Millisecond)
	}
}

func (h *DataOpsHandler) loadStereoSplitStoredRun(ctx context.Context, runID string) (stereoSplitBulkStoredRun, error) {
	var run stereoSplitBulkStoredRun
	err := h.db.GetContext(ctx, &run, `
		SELECT request, snapshot_max_episode_id, materialize_cursor, status, materialized_at, error_message
		FROM bulk_runs WHERE run_id = ? AND action = ?
	`, runID, dataOpsBulkRunActionStereoSplit)
	return run, err
}

func (h *DataOpsHandler) selectStereoSplitMaterializePage(
	ctx context.Context,
	query dataOpsEpisodeQuery,
	cursor, snapshotMax int64,
) ([]int64, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(query)
	args = append(args, cursor, snapshotMax, stereoSplitBulkPageSize)
	ids := []int64{}
	err := h.db.SelectContext(ctx, &ids, `
		SELECT e.id
	`+fromSQL+where+`
		  AND e.id > ? AND e.id <= ?
		ORDER BY e.id ASC
		LIMIT ?
	`, args...)
	return ids, err
}

func (h *DataOpsHandler) failStereoSplitMaterialization(ctx context.Context, runID, message string) {
	now := h.dataOpsBulkRunNow()
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET error_message = ?, materialized_at = COALESCE(materialized_at, ?), updated_at = ?
		WHERE run_id = ? AND action = ?
	`, message, now, now, runID, dataOpsBulkRunActionStereoSplit); err != nil {
		logger.Printf("[STEREO_SPLIT] Persist materialization failure failed: run_id=%s err=%v", runID, err)
	}
}

func (h *DataOpsHandler) closeCanceledStereoSplitMaterialization(ctx context.Context, runID string) error {
	now := h.dataOpsBulkRunNow()
	_, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs SET materialized_at = COALESCE(materialized_at, ?), updated_at = ?
		WHERE run_id = ? AND action = ?;
	`, now, now, runID, dataOpsBulkRunActionStereoSplit)
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `
		UPDATE bulk_run_items SET admission_status = ?, result_reason = ?, updated_at = ?
		WHERE bulk_run_id = ? AND admission_status = ?
	`, stereosplit.BulkAdmissionCanceled, "canceled_before_admission", now, runID, stereosplit.BulkAdmissionPending)
	return err
}

type stereoSplitBulkItemStateRow struct {
	AdmissionStatus   string         `db:"admission_status"`
	ResultReason      sql.NullString `db:"result_reason"`
	ResultSnapshot    sql.NullString `db:"result_snapshot"`
	ProcessingStatus  sql.NullString `db:"processing_status"`
	QAStatus          sql.NullString `db:"qa_status"`
	OrbitDeleteStatus sql.NullString `db:"orbit_delete_status"`
}

type stereoSplitBulkResultSnapshot struct {
	ProcessingStatus  string `json:"processing_status"`
	QAStatus          string `json:"qa_status"`
	OrbitDeleteStatus string `json:"orbit_delete_status"`
}

func (h *DataOpsHandler) refreshStereoSplitBulkRun(ctx context.Context, runID string) (DataOpsBulkRunResponse, bool, error) {
	stored, err := h.loadStereoSplitStoredRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, false, err
	}
	rows := []stereoSplitBulkItemStateRow{}
	if err := h.db.SelectContext(ctx, &rows, `
		SELECT i.admission_status, i.result_reason, i.result_snapshot,
		       d.processing_status, d.qa_status, d.orbit_delete_status
		FROM bulk_run_items i
		LEFT JOIN episode_derivatives d
		  ON d.id = i.derivative_id AND d.generation = i.derivative_generation
		WHERE i.bulk_run_id = ?
		ORDER BY i.id ASC
	`, runID); err != nil {
		return DataOpsBulkRunResponse{}, false, err
	}

	counts := stereoSplitEmptyFinalCounts(0)
	counts["materialized_items"] = int64(len(rows))
	var terminalAdmitted int64
	var pendingItems int64
	for _, row := range rows {
		switch row.AdmissionStatus {
		case stereosplit.BulkAdmissionPending:
			pendingItems++
		case stereosplit.BulkAdmissionSkipped:
			counts["skipped"]++
		case stereosplit.BulkAdmissionCanceled:
			counts["canceled_before_admission"]++
		case stereosplit.BulkAdmissionAdmitted:
			counts["admitted"]++
			processingStatus := row.ProcessingStatus.String
			qaStatus := row.QAStatus.String
			deleteStatus := row.OrbitDeleteStatus.String
			if row.ResultSnapshot.Valid {
				var snapshot stereoSplitBulkResultSnapshot
				if err := json.Unmarshal([]byte(row.ResultSnapshot.String), &snapshot); err != nil {
					return DataOpsBulkRunResponse{}, false, fmt.Errorf("decode stereo split result snapshot: %w", err)
				}
				processingStatus, qaStatus, deleteStatus = snapshot.ProcessingStatus, snapshot.QAStatus, snapshot.OrbitDeleteStatus
				terminalAdmitted++
			}
			incrementStereoSplitStateCounts(counts, processingStatus, qaStatus, deleteStatus)
		}
	}

	var previewCounts map[string]int64
	var previewJSON sql.NullString
	if err := h.db.GetContext(ctx, &previewJSON, "SELECT preview_counts FROM bulk_runs WHERE run_id = ?", runID); err != nil {
		return DataOpsBulkRunResponse{}, false, err
	}
	if previewJSON.Valid {
		_ = json.Unmarshal([]byte(previewJSON.String), &previewCounts)
	}
	counts["preview_estimated"] = previewCounts["matched"]
	processed := counts["skipped"] + counts["canceled_before_admission"] + terminalAdmitted
	processingFailed := counts["failed"]
	canceled := counts["canceled_before_admission"] + counts["processing_canceled"]
	total := counts["preview_estimated"]
	if stored.Materialized.Valid {
		total = counts["materialized_items"]
	}
	now := h.dataOpsBulkRunNow()
	terminal := stored.Materialized.Valid && pendingItems == 0 && terminalAdmitted == counts["admitted"]
	if terminal {
		finalJSON, err := json.Marshal(counts)
		if err != nil {
			return DataOpsBulkRunResponse{}, false, err
		}
		status := dataOpsBulkRunStatusCompleted
		if stored.Status == dataOpsBulkRunStatusCancelRequested {
			status = dataOpsBulkRunStatusCanceled
		} else if strings.TrimSpace(stored.ErrorMessage.String) != "" {
			status = dataOpsBulkRunStatusFailed
		}
		if _, err := h.db.ExecContext(ctx, `
			UPDATE bulk_runs
			SET status = ?, total_count = ?, processed_count = ?, passed_count = ?,
			    qa_failed_count = ?, processing_failed_count = ?, skipped_count = ?, canceled_count = ?,
			    final_counts = ?, counts_frozen_at = COALESCE(counts_frozen_at, ?),
			    finished_at = COALESCE(finished_at, ?), updated_at = ?
			WHERE run_id = ? AND action = ? AND counts_frozen_at IS NULL
		`, status, total, processed, counts["succeeded"], counts["qa_failed"], processingFailed,
			counts["skipped"], canceled, string(finalJSON), now, now, now, runID, dataOpsBulkRunActionStereoSplit); err != nil {
			return DataOpsBulkRunResponse{}, false, err
		}
	} else {
		if _, err := h.db.ExecContext(ctx, `
			UPDATE bulk_runs
			SET total_count = ?, processed_count = ?, passed_count = ?, qa_failed_count = ?,
			    processing_failed_count = ?, skipped_count = ?, canceled_count = ?, updated_at = ?
			WHERE run_id = ? AND action = ? AND counts_frozen_at IS NULL
		`, total, processed, counts["succeeded"], counts["qa_failed"], processingFailed,
			counts["skipped"], canceled, now, runID, dataOpsBulkRunActionStereoSplit); err != nil {
			return DataOpsBulkRunResponse{}, false, err
		}
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, false, err
	}
	return run, terminal, nil
}

func incrementStereoSplitStateCounts(counts map[string]int64, processingStatus, qaStatus, deleteStatus string) {
	switch processingStatus {
	case stereosplit.ProcessingQueued:
		counts["queued"]++
	case stereosplit.ProcessingSubmitting, stereosplit.ProcessingPending:
		counts["pending"]++
	case stereosplit.ProcessingRunning:
		counts["running"]++
	case stereosplit.ProcessingVerifying:
		counts["verifying"]++
	case stereosplit.ProcessingSucceeded:
		counts["succeeded"]++
	case stereosplit.ProcessingFailed:
		counts["failed"]++
	case stereosplit.ProcessingCanceled:
		counts["processing_canceled"]++
	}
	switch qaStatus {
	case stereosplit.QAApproved:
		counts["qa_approved"]++
	case stereosplit.QAFailed:
		counts["qa_failed"]++
	}
	switch deleteStatus {
	case stereosplit.DeleteCompleted:
		counts["orbit_delete_completed"]++
	case stereosplit.DeleteNotRequired:
		counts["orbit_delete_not_required"]++
	}
}

// ResumeStereoSplitBulkRuns restarts durable materializers/finalizers after a
// Keystone restart. Other legacy bulk actions retain their existing semantics.
func (h *DataOpsHandler) ResumeStereoSplitBulkRuns(ctx context.Context) error {
	if h == nil || h.db == nil || h.stereoSplit == nil {
		return nil
	}
	runIDs := []string{}
	if err := h.db.SelectContext(ctx, &runIDs, `
		SELECT run_id FROM bulk_runs
		WHERE action = ? AND status IN (?, ?, ?)
		ORDER BY id ASC
	`, dataOpsBulkRunActionStereoSplit, dataOpsBulkRunStatusQueued,
		dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
		return fmt.Errorf("load active stereo split bulk runs: %w", err)
	}
	for _, runID := range runIDs {
		h.startStereoSplitBulkRun(runID)
	}
	return nil
}

// ListBulkRunItems returns immutable membership and result snapshots for a
// stereo-split run. Legacy actions currently have no item history.
//
// @Summary      List bulk-run items
// @Description  Returns immutable admission and terminal result snapshots for a stereo-split bulk run.
// @Tags         data-ops
// @Produce      json
// @Param        run_id  path      string  true   "Bulk run ID"
// @Param        limit   query     int     false  "Max results"
// @Param        offset  query     int     false  "Pagination offset"
// @Success      200     {object}  map[string]any
// @Failure      400     {object}  map[string]string
// @Failure      404     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /data-ops/bulk-runs/{run_id}/items [get]
func (h *DataOpsHandler) ListBulkRunItems(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	runID := strings.TrimSpace(c.Param("run_id"))
	var action string
	if err := h.db.GetContext(c.Request.Context(), &action, "SELECT action FROM bulk_runs WHERE run_id = ?", runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bulk run not found"})
			return
		}
		logger.Printf("[DATA_OPS] load bulk run %s failed: %v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bulk run"})
		return
	}
	if action != dataOpsBulkRunActionStereoSplit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bulk run does not have item history"})
		return
	}
	var total int
	if err := h.db.GetContext(c.Request.Context(), &total, "SELECT COUNT(*) FROM bulk_run_items WHERE bulk_run_id = ?", runID); err != nil {
		logger.Printf("[DATA_OPS] count bulk run %s items failed: %v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count bulk run items"})
		return
	}
	type itemRow struct {
		ID                   int64          `db:"id" json:"id"`
		EpisodeID            int64          `db:"episode_id" json:"episode_id"`
		DerivativeID         sql.NullInt64  `db:"derivative_id" json:"-"`
		DerivativeGeneration sql.NullInt64  `db:"derivative_generation" json:"-"`
		AdmissionStatus      string         `db:"admission_status" json:"admission_status"`
		ResultReason         sql.NullString `db:"result_reason" json:"-"`
		ResultSnapshot       sql.NullString `db:"result_snapshot" json:"-"`
		CreatedAt            time.Time      `db:"created_at" json:"created_at"`
		UpdatedAt            time.Time      `db:"updated_at" json:"updated_at"`
	}
	rows := []itemRow{}
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, episode_id, derivative_id, derivative_generation, admission_status,
		       result_reason, result_snapshot, created_at, updated_at
		FROM bulk_run_items WHERE bulk_run_id = ? ORDER BY id ASC LIMIT ? OFFSET ?
	`, runID, pagination.Limit, pagination.Offset); err != nil {
		logger.Printf("[DATA_OPS] list bulk run %s items failed: %v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bulk run items"})
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		item := gin.H{
			"id": row.ID, "episode_id": row.EpisodeID, "admission_status": row.AdmissionStatus,
			"created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
		}
		if row.DerivativeID.Valid {
			item["derivative_id"] = row.DerivativeID.Int64
		}
		if row.DerivativeGeneration.Valid {
			item["derivative_generation"] = row.DerivativeGeneration.Int64
		}
		if row.ResultReason.Valid {
			item["result_reason"] = row.ResultReason.String
		}
		if row.ResultSnapshot.Valid {
			var snapshot any
			if json.Unmarshal([]byte(row.ResultSnapshot.String), &snapshot) == nil {
				item["result_snapshot"] = snapshot
			}
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "limit": pagination.Limit, "offset": pagination.Offset})
}

func (h *DataOpsHandler) cancelStereoSplitBulkRun(ctx context.Context, runID string) error {
	if err := h.closeCanceledStereoSplitMaterialization(ctx, runID); err != nil {
		return err
	}
	episodeIDs := []int64{}
	if err := h.db.SelectContext(ctx, &episodeIDs, `
		SELECT episode_id FROM bulk_run_items
		WHERE bulk_run_id = ? AND admission_status = ? AND result_snapshot IS NULL
	`, runID, stereosplit.BulkAdmissionAdmitted); err != nil {
		return err
	}
	for _, episodeID := range episodeIDs {
		if _, err := h.stereoSplit.Cancel(ctx, episodeID, "bulk:"+runID); err != nil &&
			!errors.Is(err, stereosplit.ErrAlreadyDerived) && !errors.Is(err, stereosplit.ErrNotFound) {
			logger.Printf("[STEREO_SPLIT] Bulk cancellation failed: run_id=%s episode_id=%d err=%v", runID, episodeID, err)
		}
	}
	h.startStereoSplitBulkRun(runID)
	return nil
}
