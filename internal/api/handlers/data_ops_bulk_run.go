// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/logger"
)

const (
	dataOpsBulkRunActionQA                 = "bulk_qa"
	dataOpsBulkRunActionMP4                = "bulk_mp4"
	dataOpsBulkRunActionSync               = "bulk_sync"
	dataOpsBulkRunActionLocalCleanup       = "local_cleanup"
	dataOpsBulkRunActionStereoSplit        = "stereo_split"
	dataOpsBulkRunActionDepthNormalization = "depth_normalization"

	dataOpsBulkRunStatusQueued          = "queued"
	dataOpsBulkRunStatusRunning         = "running"
	dataOpsBulkRunStatusCompleted       = "completed"
	dataOpsBulkRunStatusFailed          = "failed"
	dataOpsBulkRunStatusInterrupted     = "interrupted"
	dataOpsBulkRunStatusCancelRequested = "cancel_requested"
	dataOpsBulkRunStatusCanceled        = "canceled"
)

type dataOpsEpisodeQARunner interface {
	RunEpisodeQASuite(ctx context.Context, episodeID int64, mode QARunMode) (*EpisodeQASuiteResponse, error)
}

type dataOpsBulkRunEvent struct {
	name string
	run  DataOpsBulkRunResponse
}

type dataOpsBulkRunBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan dataOpsBulkRunEvent]struct{}
}

func newDataOpsBulkRunBroker() *dataOpsBulkRunBroker {
	return &dataOpsBulkRunBroker{subscribers: make(map[string]map[chan dataOpsBulkRunEvent]struct{})}
}

func (b *dataOpsBulkRunBroker) Subscribe(runID string, buffer int) (<-chan dataOpsBulkRunEvent, func()) {
	if b == nil {
		ch := make(chan dataOpsBulkRunEvent)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan dataOpsBulkRunEvent, buffer)
	b.mu.Lock()
	if b.subscribers == nil {
		b.subscribers = make(map[string]map[chan dataOpsBulkRunEvent]struct{})
	}
	if b.subscribers[runID] == nil {
		b.subscribers[runID] = make(map[chan dataOpsBulkRunEvent]struct{})
	}
	b.subscribers[runID][ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if subscribers := b.subscribers[runID]; subscribers != nil {
			if _, ok := subscribers[ch]; ok {
				delete(subscribers, ch)
				close(ch)
			}
			if len(subscribers) == 0 {
				delete(b.subscribers, runID)
			}
		}
	}
	return ch, unsubscribe
}

func (b *dataOpsBulkRunBroker) Publish(runID string, event dataOpsBulkRunEvent) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers[runID] {
		select {
		case ch <- event:
		default:
		}
	}
}

type dataOpsBulkQAEpisodeOutcome string

const (
	dataOpsBulkQAEpisodePassed           dataOpsBulkQAEpisodeOutcome = "passed"
	dataOpsBulkQAEpisodeFailed           dataOpsBulkQAEpisodeOutcome = "qa_failed"
	dataOpsBulkQAEpisodeProcessingFailed dataOpsBulkQAEpisodeOutcome = "processing_failed"
	dataOpsBulkQAEpisodeSkipped          dataOpsBulkQAEpisodeOutcome = "skipped"
)

type dataOpsBulkQAEpisodeResult struct {
	episodeID int64
	outcome   dataOpsBulkQAEpisodeOutcome
}

// DataOpsBulkRunResponse is the short-lived progress snapshot for one bulk action run.
type DataOpsBulkRunResponse struct {
	RunID                 string           `json:"run_id"`
	Action                string           `json:"action"`
	Status                string           `json:"status"`
	TotalCount            int64            `json:"total_count"`
	ProcessedCount        int64            `json:"processed_count"`
	PassedCount           int64            `json:"passed_count"`
	QAFailedCount         int64            `json:"qa_failed_count"`
	ProcessingFailedCount int64            `json:"processing_failed_count"`
	SkippedCount          int64            `json:"skipped_count"`
	CanceledCount         int64            `json:"canceled_count"`
	StartedAt             *time.Time       `json:"started_at"`
	CancelRequestedAt     *time.Time       `json:"cancel_requested_at"`
	UpdatedAt             time.Time        `json:"updated_at"`
	FinishedAt            *time.Time       `json:"finished_at"`
	ErrorMessage          string           `json:"error_message"`
	DownloadURL           string           `json:"download_url,omitempty"`
	PreviewCounts         map[string]int64 `json:"preview_counts,omitempty"`
	FinalCounts           map[string]int64 `json:"final_counts,omitempty"`
	MaterializedAt        *time.Time       `json:"materialized_at,omitempty"`
	CountsFrozenAt        *time.Time       `json:"counts_frozen_at,omitempty"`
}

type dataOpsBulkRunRow struct {
	ID                    int64          `db:"id"`
	RunID                 string         `db:"run_id"`
	Action                string         `db:"action"`
	Status                string         `db:"status"`
	TotalCount            int64          `db:"total_count"`
	ProcessedCount        int64          `db:"processed_count"`
	PassedCount           int64          `db:"passed_count"`
	QAFailedCount         int64          `db:"qa_failed_count"`
	ProcessingFailedCount int64          `db:"processing_failed_count"`
	SkippedCount          int64          `db:"skipped_count"`
	CanceledCount         int64          `db:"canceled_count"`
	ErrorMessage          sql.NullString `db:"error_message"`
	StartedAt             sql.NullTime   `db:"started_at"`
	CancelRequestedAt     sql.NullTime   `db:"cancel_requested_at"`
	FinishedAt            sql.NullTime   `db:"finished_at"`
	CreatedAt             time.Time      `db:"created_at"`
	UpdatedAt             time.Time      `db:"updated_at"`
}

func (h *DataOpsHandler) bulkQARunner() dataOpsEpisodeQARunner {
	if h == nil {
		return nil
	}
	if h.qaRunner != nil {
		return h.qaRunner
	}
	if h.qa != nil {
		return h.qa
	}
	return nil
}

func (h *DataOpsHandler) ensureBulkRunBroker() *dataOpsBulkRunBroker {
	h.bulkRunBrokerMu.Lock()
	defer h.bulkRunBrokerMu.Unlock()
	if h.bulkRunBroker == nil {
		h.bulkRunBroker = newDataOpsBulkRunBroker()
	}
	return h.bulkRunBroker
}

func (h *DataOpsHandler) publishBulkRunEvent(name string, run DataOpsBulkRunResponse) {
	if h == nil {
		return
	}
	h.ensureBulkRunBroker().Publish(run.RunID, dataOpsBulkRunEvent{name: name, run: run})
}

func (h *DataOpsHandler) dataOpsBulkRunNow() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func defaultDataOpsBulkRunID(action string, now time.Time) (string, error) {
	var randomBytes [3]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%s_%s", action, now.UTC().Format("20060102_150405"), hex.EncodeToString(randomBytes[:])), nil
}

func dataOpsBulkRunResponseFromRow(row dataOpsBulkRunRow) DataOpsBulkRunResponse {
	resp := DataOpsBulkRunResponse{
		RunID:                 row.RunID,
		Action:                row.Action,
		Status:                row.Status,
		TotalCount:            row.TotalCount,
		ProcessedCount:        row.ProcessedCount,
		PassedCount:           row.PassedCount,
		QAFailedCount:         row.QAFailedCount,
		ProcessingFailedCount: row.ProcessingFailedCount,
		SkippedCount:          row.SkippedCount,
		CanceledCount:         row.CanceledCount,
		UpdatedAt:             row.UpdatedAt.UTC(),
	}
	if row.ErrorMessage.Valid {
		resp.ErrorMessage = row.ErrorMessage.String
	}
	if row.StartedAt.Valid {
		startedAt := row.StartedAt.Time.UTC()
		resp.StartedAt = &startedAt
	}
	if row.CancelRequestedAt.Valid {
		cancelRequestedAt := row.CancelRequestedAt.Time.UTC()
		resp.CancelRequestedAt = &cancelRequestedAt
	}
	if row.FinishedAt.Valid {
		finishedAt := row.FinishedAt.Time.UTC()
		resp.FinishedAt = &finishedAt
	}
	return resp
}

func (h *DataOpsHandler) createBulkQARun(ctx context.Context, totalCount int64) (DataOpsBulkRunResponse, error) {
	return h.createBulkRun(ctx, dataOpsBulkRunActionQA, totalCount)
}

func (h *DataOpsHandler) createBulkRun(ctx context.Context, action string, totalCount int64) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	runID, err := defaultDataOpsBulkRunID(action, now)
	if err != nil {
		return DataOpsBulkRunResponse{}, err
	}

	status := dataOpsBulkRunStatusQueued
	var startedAt interface{}
	var finishedAt interface{}
	if totalCount == 0 {
		status = dataOpsBulkRunStatusCompleted
		startedAt = now
		finishedAt = now
	}

	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO bulk_runs (
			run_id, action, status, total_count, processed_count, passed_count,
			qa_failed_count, processing_failed_count, skipped_count, error_message,
			started_at, finished_at, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, '', ?, ?, ?, ?)
	`, runID, action, status, totalCount, startedAt, finishedAt, now, now); err != nil {
		return DataOpsBulkRunResponse{}, err
	}

	return h.loadBulkRun(ctx, runID)
}

func (h *DataOpsHandler) loadBulkRun(ctx context.Context, runID string) (DataOpsBulkRunResponse, error) {
	var row dataOpsBulkRunRow
	if err := h.db.GetContext(ctx, &row, `
		SELECT id, run_id, action, status, total_count, processed_count, passed_count,
		       qa_failed_count, processing_failed_count, skipped_count, canceled_count, error_message,
		       started_at, cancel_requested_at, finished_at, created_at, updated_at
		FROM bulk_runs
		WHERE run_id = ?
	`, runID); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	resp := dataOpsBulkRunResponseFromRow(row)
	if row.Action == dataOpsBulkRunActionStereoSplit {
		if err := h.loadStereoSplitBulkRunMetadata(ctx, runID, &resp); err != nil {
			return DataOpsBulkRunResponse{}, err
		}
	}
	if row.Action == dataOpsBulkRunActionMP4 && (resp.Status == dataOpsBulkRunStatusCompleted || resp.Status == dataOpsBulkRunStatusCanceled) {
		// #nosec G703 -- the path uses a server-generated run ID loaded from bulk_runs.
		if _, err := os.Stat(h.bulkMP4ZipPath(resp.RunID)); err == nil {
			resp.DownloadURL = h.bulkMP4DownloadURL(resp.RunID)
		}
	}
	return resp, nil
}

// CancelBulkRun requests graceful cancellation of one active bulk run.
//
// @Summary      Cancel bulk run
// @Description  Stops dispatching new items. Stereo-split runs also request cancellation of already-admitted generations.
// @Tags         data-ops
// @Produce      json
// @Param        run_id  path      string  true  "Bulk run ID"
// @Success      200     {object}  DataOpsBulkRunResponse
// @Failure      404     {object}  map[string]string
// @Failure      409     {object}  map[string]string
// @Failure      503     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /data-ops/bulk-runs/{run_id}/cancel [post]
func (h *DataOpsHandler) CancelBulkRun(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}

	runID := strings.TrimSpace(c.Param("run_id"))
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	res, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE bulk_runs
		SET status = ?, cancel_requested_at = COALESCE(cancel_requested_at, ?), updated_at = ?
		WHERE run_id = ? AND status IN (?, ?, ?)
	`, dataOpsBulkRunStatusCancelRequested, now, now, runID,
		dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk run cancel update failed: run_id=%s err=%v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel bulk run"})
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		logger.Printf("[DATA_OPS] bulk run cancel affected rows failed: run_id=%s err=%v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel bulk run"})
		return
	}

	run, err := h.loadBulkRun(c.Request.Context(), runID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "bulk run not found"})
			return
		}
		logger.Printf("[DATA_OPS] bulk run reload after cancel failed: run_id=%s err=%v", runID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bulk run"})
		return
	}
	if affected == 0 && run.Status != dataOpsBulkRunStatusCancelRequested && run.Status != dataOpsBulkRunStatusCanceled {
		c.JSON(http.StatusConflict, gin.H{"error": "bulk run is already finished", "run": run})
		return
	}
	h.signalBulkRunCancellation(runID)
	if run.Status == dataOpsBulkRunStatusCancelRequested {
		h.publishBulkRunEvent("bulk_run_progress", run)
	}
	if run.Action == dataOpsBulkRunActionSync && h.syncWorker != nil {
		if _, err := h.syncWorker.CancelBulkRun(c.Request.Context(), runID); err != nil {
			// The persisted cancel request remains authoritative; the run monitor retries cleanup.
			logger.Printf("[DATA_OPS] bulk sync pending cancellation failed: run_id=%s err=%v", runID, err)
		}
	}
	if run.Action == dataOpsBulkRunActionStereoSplit && h.stereoSplit != nil {
		if err := h.cancelStereoSplitBulkRun(c.Request.Context(), runID); err != nil {
			logger.Printf("[STEREO_SPLIT] Bulk cancellation persistence failed: run_id=%s err=%v", runID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel stereo split run"})
			return
		}
	}
	c.JSON(http.StatusOK, run)
}

func (h *DataOpsHandler) markBulkRunRunning(ctx context.Context, runID string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, started_at = COALESCE(started_at, ?), updated_at = ?
		WHERE run_id = ? AND status = ?
	`, dataOpsBulkRunStatusRunning, now, now, runID, dataOpsBulkRunStatusQueued); err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("mark bulk run running: %w", err)
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("load running bulk run: %w", err)
	}
	return run, nil
}

func (h *DataOpsHandler) incrementBulkQARunCounts(ctx context.Context, runID string, outcome dataOpsBulkQAEpisodeOutcome) (DataOpsBulkRunResponse, error) {
	var passedDelta int64
	var qaFailedDelta int64
	var processingFailedDelta int64
	var skippedDelta int64
	switch outcome {
	case dataOpsBulkQAEpisodePassed:
		passedDelta = 1
	case dataOpsBulkQAEpisodeFailed:
		qaFailedDelta = 1
	case dataOpsBulkQAEpisodeProcessingFailed:
		processingFailedDelta = 1
	case dataOpsBulkQAEpisodeSkipped:
		skippedDelta = 1
	default:
		return DataOpsBulkRunResponse{}, fmt.Errorf("unknown bulk qa outcome %q", outcome)
	}

	// #nosec G701 -- static SQL with placeholder-bound bulk run counters.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET processed_count = processed_count + 1,
		    passed_count = passed_count + ?,
		    qa_failed_count = qa_failed_count + ?,
		    processing_failed_count = processing_failed_count + ?,
		    skipped_count = skipped_count + ?,
		    updated_at = ?
		WHERE run_id = ? AND status IN (?, ?)
	`, passedDelta, qaFailedDelta, processingFailedDelta, skippedDelta, h.dataOpsBulkRunNow(), runID,
		dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("increment bulk run counts: %w", err)
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("load bulk run after count update: %w", err)
	}
	return run, nil
}

func (h *DataOpsHandler) markBulkRunTerminal(ctx context.Context, runID string, status string, errorMessage string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	if status == dataOpsBulkRunStatusFailed {
		// #nosec G701 -- static SQL with placeholder-bound bulk run values.
		if _, err := h.db.ExecContext(ctx, `
			UPDATE bulk_runs
			SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
			WHERE run_id = ? AND status IN (?, ?, ?)
		`, status, errorMessage, now, now, runID, dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
			return DataOpsBulkRunResponse{}, fmt.Errorf("mark failed bulk run terminal: %w", err)
		}
	} else {
		// #nosec G701 -- static SQL with placeholder-bound bulk run values.
		if _, err := h.db.ExecContext(ctx, `
			UPDATE bulk_runs
			SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
			WHERE run_id = ? AND status IN (?, ?)
		`, status, errorMessage, now, now, runID, dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning); err != nil {
			return DataOpsBulkRunResponse{}, fmt.Errorf("mark bulk run terminal: %w", err)
		}
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("load terminal bulk run: %w", err)
	}
	if run.Status == dataOpsBulkRunStatusCancelRequested {
		canceledRun, err := h.markBulkRunCanceled(ctx, runID)
		if err != nil {
			return DataOpsBulkRunResponse{}, fmt.Errorf("finalize concurrent bulk run cancellation: %w", err)
		}
		return canceledRun, nil
	}
	if eventName, ok := dataOpsBulkRunTerminalEventName(run.Status); ok {
		h.publishBulkRunEvent(eventName, run)
	}
	return run, nil
}

func (h *DataOpsHandler) markBulkRunCanceled(ctx context.Context, runID string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	res, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?,
		    canceled_count = CASE
		        WHEN total_count > processed_count THEN total_count - processed_count
		        ELSE 0
		    END,
		    finished_at = COALESCE(finished_at, ?),
		    updated_at = ?
		WHERE run_id = ? AND status = ?
	`, dataOpsBulkRunStatusCanceled, now, now, runID, dataOpsBulkRunStatusCancelRequested)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("mark bulk run canceled: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("read canceled bulk run affected rows: %w", err)
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("load canceled bulk run: %w", err)
	}
	if affected > 0 && run.Status == dataOpsBulkRunStatusCanceled {
		h.publishBulkRunEvent("bulk_run_canceled", run)
	}
	return run, nil
}

func (h *DataOpsHandler) markBulkRunCancellationFailed(ctx context.Context, runID string, errorMessage string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
		WHERE run_id = ? AND status = ?
	`, dataOpsBulkRunStatusFailed, errorMessage, now, now, runID, dataOpsBulkRunStatusCancelRequested); err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("mark bulk run cancellation failed: %w", err)
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, fmt.Errorf("load failed bulk run cancellation: %w", err)
	}
	if run.Status == dataOpsBulkRunStatusFailed {
		h.publishBulkRunEvent("bulk_run_failed", run)
	}
	return run, nil
}

func (h *DataOpsHandler) beginBulkRunExecution(runID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	execution := &dataOpsBulkRunExecution{cancel: cancel}
	h.bulkRunCancelMu.Lock()
	if h.bulkRunExecutions == nil {
		h.bulkRunExecutions = make(map[string]*dataOpsBulkRunExecution)
	}
	h.bulkRunExecutions[runID] = execution
	h.bulkRunCancelMu.Unlock()

	run, err := h.loadBulkRun(context.Background(), runID)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk run cancellation state lookup failed: run_id=%s err=%v", runID, err)
	} else if run.Status == dataOpsBulkRunStatusCancelRequested {
		execution.requestCancellation()
	}
	return ctx, func() {
		cancel()
		h.bulkRunCancelMu.Lock()
		delete(h.bulkRunExecutions, runID)
		h.bulkRunCancelMu.Unlock()
	}
}

func (h *DataOpsHandler) signalBulkRunCancellation(runID string) {
	h.bulkRunCancelMu.Lock()
	execution := h.bulkRunExecutions[runID]
	h.bulkRunCancelMu.Unlock()
	if execution != nil {
		execution.requestCancellation()
	}
}

func (h *DataOpsHandler) reserveBulkRunItem(runID string) bool {
	h.bulkRunCancelMu.Lock()
	execution := h.bulkRunExecutions[runID]
	h.bulkRunCancelMu.Unlock()
	if execution == nil {
		return false
	}
	return execution.reserveItem()
}

func (e *dataOpsBulkRunExecution) requestCancellation() {
	e.mu.Lock()
	e.cancelRequested = true
	e.cancel()
	e.mu.Unlock()
}

func (e *dataOpsBulkRunExecution) reserveItem() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.cancelRequested
}

func (h *DataOpsHandler) bulkRunCancellationRequested(ctx context.Context, runID string) bool {
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk run cancellation check failed: run_id=%s err=%v", runID, err)
		return false
	}
	return run.Status == dataOpsBulkRunStatusCancelRequested || run.Status == dataOpsBulkRunStatusCanceled
}

// InterruptActiveBulkRuns recovers canceled sync runs, then marks the remaining stale
// in-flight bulk runs as interrupted on service startup.
func (h *DataOpsHandler) InterruptActiveBulkRuns(ctx context.Context, maxSyncRetries int) error {
	if h == nil || h.db == nil {
		return nil
	}
	now := h.dataOpsBulkRunNow()
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bulk run startup recovery: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			logger.Printf("[DATA_OPS] bulk run startup recovery rollback failed: %v", err)
		}
	}()

	if _, err := tx.ExecContext(ctx, `
		UPDATE sync_logs
		SET status = 'canceled',
		    error_message = COALESCE(error_message, 'bulk run cancellation recovered after service restart'),
		    next_retry_at = NULL,
		    completed_at = COALESCE(completed_at, ?)
		WHERE bulk_run_id IN (
		    SELECT run_id
		    FROM bulk_runs
		    WHERE action = ? AND status = ?
		)
		  AND (
		    status = 'pending'
		    OR (status = 'failed' AND next_retry_at IS NOT NULL AND attempt_count < ?)
		  )
	`, now, dataOpsBulkRunActionSync, dataOpsBulkRunStatusCancelRequested, maxSyncRetries); err != nil {
		return fmt.Errorf("cancel queued sync work during bulk run startup recovery: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?,
		    canceled_count = CASE
		        WHEN total_count > processed_count THEN total_count - processed_count
		        ELSE 0
		    END,
		    finished_at = COALESCE(finished_at, ?),
		    updated_at = ?
		WHERE action = ? AND status = ?
	`, dataOpsBulkRunStatusCanceled, now, now, dataOpsBulkRunActionSync, dataOpsBulkRunStatusCancelRequested); err != nil {
		return fmt.Errorf("finalize canceled sync runs during bulk run startup recovery: %w", err)
	}

	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := tx.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
		WHERE action IN (?, ?, ?) AND status IN (?, ?, ?)
	`, dataOpsBulkRunStatusInterrupted, "service restarted before bulk action completed", now, now,
		dataOpsBulkRunActionQA, dataOpsBulkRunActionMP4, dataOpsBulkRunActionSync,
		dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
		return fmt.Errorf("interrupt active bulk runs during startup recovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bulk run startup recovery: %w", err)
	}
	return nil
}

// GetBulkRun returns the latest stored snapshot for one bulk run.
//
// @Summary      Get bulk run snapshot
// @Description  Returns the current aggregate snapshot for one bulk action run.
// @Tags         data-ops
// @Produce      json
// @Param        run_id  path      string  true  "Bulk run ID"
// @Success      200     {object}  DataOpsBulkRunResponse
// @Failure      404     {object}  map[string]string
// @Failure      503     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /data-ops/bulk-runs/{run_id} [get]
func (h *DataOpsHandler) GetBulkRun(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	run, err := h.loadBulkRun(c.Request.Context(), c.Param("run_id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "bulk run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bulk run"})
		return
	}
	c.JSON(http.StatusOK, run)
}

// GetCurrentBulkRun returns the active bulk run for an action, if one exists.
//
// @Summary      Get current bulk run
// @Description  Returns the active bulk run snapshot, or 204 when no run is active.
// @Tags         data-ops
// @Produce      json
// @Param        action  query     string  true  "Bulk action: bulk_qa, bulk_mp4, bulk_sync, or stereo_split"
// @Success      200     {object}  DataOpsBulkRunResponse
// @Success      204
// @Failure      400     {object}  map[string]string
// @Failure      503     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /data-ops/bulk-runs/current [get]
func (h *DataOpsHandler) GetCurrentBulkRun(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	action := c.Query("action")
	if !isAllowedDataOpsBulkRunAction(action) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be bulk_qa, bulk_mp4, bulk_sync, stereo_split, or depth_normalization"})
		return
	}

	run, ok, err := h.currentBulkRun(c.Request.Context(), action)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current bulk run"})
		return
	}
	if !ok {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, run)
}

// StreamBulkRun streams progress events for one bulk run.
//
// @Summary      Stream bulk run progress
// @Description  Streams aggregate bulk run snapshots using Server-Sent Events.
// @Tags         data-ops
// @Produce      text/event-stream
// @Param        run_id  path  string  true  "Bulk run ID"
// @Success      200
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /data-ops/bulk-runs/{run_id}/stream [get]
func (h *DataOpsHandler) StreamBulkRun(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}

	runID := strings.TrimSpace(c.Param("run_id"))
	events, unsubscribe := h.ensureBulkRunBroker().Subscribe(runID, 64)
	defer unsubscribe()

	run, err := h.loadBulkRun(c.Request.Context(), runID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "bulk run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bulk run"})
		return
	}

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	if err := writeDataOpsBulkRunSSE(w, "bulk_run_snapshot", run); err != nil {
		return
	}
	if eventName, ok := dataOpsBulkRunTerminalEventName(run.Status); ok {
		_ = writeDataOpsBulkRunSSE(w, eventName, run)
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeDataOpsBulkRunSSE(w, event.name, event.run); err != nil {
				return
			}
			if _, terminal := dataOpsBulkRunTerminalEventName(event.run.Status); terminal {
				return
			}
		case <-heartbeat.C:
			if err := writeDataOpsBulkRunSSE(w, "ping", gin.H{"ts": h.dataOpsBulkRunNow().Format(time.RFC3339)}); err != nil {
				return
			}
		}
	}
}

func (h *DataOpsHandler) currentBulkRun(ctx context.Context, action string) (DataOpsBulkRunResponse, bool, error) {
	var row dataOpsBulkRunRow
	if err := h.db.GetContext(ctx, &row, `
		SELECT id, run_id, action, status, total_count, processed_count, passed_count,
		       qa_failed_count, processing_failed_count, skipped_count, canceled_count, error_message,
		       started_at, cancel_requested_at, finished_at, created_at, updated_at
		FROM bulk_runs
		WHERE action = ? AND status IN (?, ?, ?)
		ORDER BY updated_at DESC, id DESC
		LIMIT 1
		`, action, dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning, dataOpsBulkRunStatusCancelRequested); err != nil {
		if err == sql.ErrNoRows {
			return DataOpsBulkRunResponse{}, false, nil
		}
		return DataOpsBulkRunResponse{}, false, err
	}
	if row.Action == dataOpsBulkRunActionStereoSplit {
		loaded, err := h.loadBulkRun(ctx, row.RunID)
		return loaded, err == nil, err
	}
	return dataOpsBulkRunResponseFromRow(row), true, nil
}

func isAllowedDataOpsBulkRunAction(action string) bool {
	switch action {
	case dataOpsBulkRunActionQA, dataOpsBulkRunActionMP4, dataOpsBulkRunActionSync, dataOpsBulkRunActionLocalCleanup, dataOpsBulkRunActionStereoSplit, dataOpsBulkRunActionDepthNormalization:
		return true
	default:
		return false
	}
}

func dataOpsBulkRunTerminalEventName(status string) (string, bool) {
	switch status {
	case dataOpsBulkRunStatusCompleted:
		return "bulk_run_completed", true
	case dataOpsBulkRunStatusFailed:
		return "bulk_run_failed", true
	case dataOpsBulkRunStatusInterrupted:
		return "bulk_run_interrupted", true
	case dataOpsBulkRunStatusCanceled:
		return "bulk_run_canceled", true
	default:
		return "", false
	}
}

func isAllowedDataOpsBulkRunSSEEventName(eventName string) bool {
	switch eventName {
	case "bulk_run_snapshot", "bulk_run_progress", "bulk_run_completed", "bulk_run_failed", "bulk_run_interrupted", "bulk_run_canceled", "ping":
		return true
	default:
		return false
	}
}

func writeDataOpsBulkRunSSE(w gin.ResponseWriter, eventName string, payload interface{}) error {
	if !isAllowedDataOpsBulkRunSSEEventName(eventName) {
		return fmt.Errorf("unsupported bulk run sse event %q", eventName)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: ")); err != nil {
		return err
	}
	if _, err := w.Write([]byte(eventName)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	w.Flush()
	return nil
}
