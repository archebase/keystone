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
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	dataOpsBulkRunActionQA = "bulk_qa"

	dataOpsBulkRunStatusQueued      = "queued"
	dataOpsBulkRunStatusRunning     = "running"
	dataOpsBulkRunStatusCompleted   = "completed"
	dataOpsBulkRunStatusFailed      = "failed"
	dataOpsBulkRunStatusInterrupted = "interrupted"
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
	RunID                 string     `json:"run_id"`
	Action                string     `json:"action"`
	Status                string     `json:"status"`
	TotalCount            int64      `json:"total_count"`
	ProcessedCount        int64      `json:"processed_count"`
	PassedCount           int64      `json:"passed_count"`
	QAFailedCount         int64      `json:"qa_failed_count"`
	ProcessingFailedCount int64      `json:"processing_failed_count"`
	SkippedCount          int64      `json:"skipped_count"`
	StartedAt             *time.Time `json:"started_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	FinishedAt            *time.Time `json:"finished_at"`
	ErrorMessage          string     `json:"error_message"`
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
	ErrorMessage          sql.NullString `db:"error_message"`
	StartedAt             sql.NullTime   `db:"started_at"`
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
	if h.bulkRunBroker == nil {
		h.bulkRunMu.Lock()
		defer h.bulkRunMu.Unlock()
	}
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
		UpdatedAt:             row.UpdatedAt.UTC(),
	}
	if row.ErrorMessage.Valid {
		resp.ErrorMessage = row.ErrorMessage.String
	}
	if row.StartedAt.Valid {
		startedAt := row.StartedAt.Time.UTC()
		resp.StartedAt = &startedAt
	}
	if row.FinishedAt.Valid {
		finishedAt := row.FinishedAt.Time.UTC()
		resp.FinishedAt = &finishedAt
	}
	return resp
}

func (h *DataOpsHandler) createBulkQARun(ctx context.Context, totalCount int64) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	runID, err := defaultDataOpsBulkRunID(dataOpsBulkRunActionQA, now)
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
	`, runID, dataOpsBulkRunActionQA, status, totalCount, startedAt, finishedAt, now, now); err != nil {
		return DataOpsBulkRunResponse{}, err
	}

	return h.loadBulkRun(ctx, runID)
}

func (h *DataOpsHandler) loadBulkRun(ctx context.Context, runID string) (DataOpsBulkRunResponse, error) {
	var row dataOpsBulkRunRow
	if err := h.db.GetContext(ctx, &row, `
		SELECT id, run_id, action, status, total_count, processed_count, passed_count,
		       qa_failed_count, processing_failed_count, skipped_count, error_message,
		       started_at, finished_at, created_at, updated_at
		FROM bulk_runs
		WHERE run_id = ?
	`, runID); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return dataOpsBulkRunResponseFromRow(row), nil
}

func (h *DataOpsHandler) markBulkRunRunning(ctx context.Context, runID string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, started_at = COALESCE(started_at, ?), updated_at = ?
		WHERE run_id = ? AND status = ?
	`, dataOpsBulkRunStatusRunning, now, now, runID, dataOpsBulkRunStatusQueued); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return h.loadBulkRun(ctx, runID)
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
		WHERE run_id = ? AND status = ?
	`, passedDelta, qaFailedDelta, processingFailedDelta, skippedDelta, h.dataOpsBulkRunNow(), runID, dataOpsBulkRunStatusRunning); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	return h.loadBulkRun(ctx, runID)
}

func (h *DataOpsHandler) markBulkRunTerminal(ctx context.Context, runID string, status string, errorMessage string) (DataOpsBulkRunResponse, error) {
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
		WHERE run_id = ?
	`, status, errorMessage, now, now, runID); err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	run, err := h.loadBulkRun(ctx, runID)
	if err != nil {
		return DataOpsBulkRunResponse{}, err
	}
	if eventName, ok := dataOpsBulkRunTerminalEventName(run.Status); ok {
		h.publishBulkRunEvent(eventName, run)
	}
	return run, nil
}

// InterruptActiveBulkQARuns marks stale in-flight bulk QA runs as interrupted on service startup.
func (h *DataOpsHandler) InterruptActiveBulkQARuns(ctx context.Context) error {
	if h == nil || h.db == nil {
		return nil
	}
	now := h.dataOpsBulkRunNow()
	// #nosec G701 -- static SQL with placeholder-bound bulk run values.
	_, err := h.db.ExecContext(ctx, `
		UPDATE bulk_runs
		SET status = ?, error_message = ?, finished_at = COALESCE(finished_at, ?), updated_at = ?
		WHERE action = ? AND status IN (?, ?)
	`, dataOpsBulkRunStatusInterrupted, "service restarted before bulk qa completed", now, now, dataOpsBulkRunActionQA, dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning)
	return err
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

// GetCurrentBulkRun returns the active bulk QA run, if one exists.
//
// @Summary      Get current bulk run
// @Description  Returns the active bulk QA run snapshot, or 204 when no run is active.
// @Tags         data-ops
// @Produce      json
// @Param        action  query     string  true  "Bulk action, currently bulk_qa"
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
	if c.Query("action") != dataOpsBulkRunActionQA {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be bulk_qa"})
		return
	}

	run, ok, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionQA)
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
		       qa_failed_count, processing_failed_count, skipped_count, error_message,
		       started_at, finished_at, created_at, updated_at
		FROM bulk_runs
		WHERE action = ? AND status IN (?, ?)
		ORDER BY updated_at DESC, id DESC
		LIMIT 1
	`, action, dataOpsBulkRunStatusQueued, dataOpsBulkRunStatusRunning); err != nil {
		if err == sql.ErrNoRows {
			return DataOpsBulkRunResponse{}, false, nil
		}
		return DataOpsBulkRunResponse{}, false, err
	}
	return dataOpsBulkRunResponseFromRow(row), true, nil
}

func dataOpsBulkRunTerminalEventName(status string) (string, bool) {
	switch status {
	case dataOpsBulkRunStatusCompleted:
		return "bulk_run_completed", true
	case dataOpsBulkRunStatusFailed:
		return "bulk_run_failed", true
	case dataOpsBulkRunStatusInterrupted:
		return "bulk_run_interrupted", true
	default:
		return "", false
	}
}

func isAllowedDataOpsBulkRunSSEEventName(eventName string) bool {
	switch eventName {
	case "bulk_run_snapshot", "bulk_run_progress", "bulk_run_completed", "bulk_run_failed", "bulk_run_interrupted", "ping":
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
