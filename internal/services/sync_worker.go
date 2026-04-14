// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"archebase.com/keystone-edge/internal/cloud"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

// SyncWorkerConfig provides the runtime configuration for the sync worker.
type SyncWorkerConfig struct {
	BatchSize     int
	MaxConcurrent int
	MaxRetries    int
	IntervalSec   int
}

// SyncWorker is a background goroutine that periodically scans for approved
// episodes and uploads them to cloud via the data-platform gateway.
type SyncWorker struct {
	db       *sqlx.DB
	uploader *cloud.Uploader
	cfg      SyncWorkerConfig
	syncCfg  *config.SyncConfig

	mu              sync.Mutex
	enqueuedEpisode map[int64]struct{}

	running atomic.Bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// runCtx is cancelled when Stop() is called so in-flight uploads and DB ops can exit promptly.
	runCtx    context.Context
	runCancel context.CancelFunc

	// enqueueCh allows the API handler to inject specific episode IDs for immediate upload.
	enqueueCh chan int64
}

// NewSyncWorker creates a new sync worker. Call Start() to begin background processing.
func NewSyncWorker(db *sqlx.DB, uploader *cloud.Uploader, cfg SyncWorkerConfig, syncCfg *config.SyncConfig) *SyncWorker {
	return &SyncWorker{
		db:              db,
		uploader:        uploader,
		cfg:             cfg,
		syncCfg:         syncCfg,
		stopCh:          make(chan struct{}),
		enqueueCh:       make(chan int64, 100),
		enqueuedEpisode: make(map[int64]struct{}),
	}
}

// Start begins the background sync worker loop.
func (w *SyncWorker) Start() {
	if !w.running.CompareAndSwap(false, true) {
		return
	}
	w.runCtx, w.runCancel = context.WithCancel(context.Background())
	w.wg.Add(1)
	go w.run()
	logger.Printf("[SYNC-WORKER] Started (interval=%ds, batch=%d, concurrency=%d)",
		w.cfg.IntervalSec, w.cfg.BatchSize, w.cfg.MaxConcurrent)
}

// Stop gracefully stops the sync worker.
func (w *SyncWorker) Stop() {
	if !w.running.CompareAndSwap(true, false) {
		return
	}
	if w.runCancel != nil {
		w.runCancel()
	}
	close(w.stopCh)
	w.wg.Wait()
	logger.Println("[SYNC-WORKER] Stopped")
}

// IsRunning returns whether the worker is currently running.
func (w *SyncWorker) IsRunning() bool {
	return w.running.Load()
}

// EnqueueEpisode adds a specific episode ID for immediate sync processing.
func (w *SyncWorker) EnqueueEpisode(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisode(ctx, episodeID, false)
}

// EnqueueEpisodeManual adds a specific episode ID for immediate sync processing,
// allowing explicit API-triggered retries even after automatic retries are exhausted.
func (w *SyncWorker) EnqueueEpisodeManual(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisode(ctx, episodeID, true)
}

func (w *SyncWorker) enqueueEpisode(ctx context.Context, episodeID int64, manual bool) error {
	if manual {
		if err := w.validateEpisodeForManualEnqueue(ctx, episodeID); err != nil {
			return err
		}
	}

	if !w.tryMarkEnqueued(episodeID) {
		return nil
	}

	select {
	case w.enqueueCh <- episodeID:
		return nil
	case <-ctx.Done():
		w.unmarkEnqueued(episodeID)
		return ctx.Err()
	default:
		w.unmarkEnqueued(episodeID)
		return fmt.Errorf("sync enqueue channel full")
	}
}

func (w *SyncWorker) validateEpisodeForManualEnqueue(ctx context.Context, episodeID int64) error {
	if w.db == nil {
		return nil
	}

	var latest struct {
		Status       string       `db:"status"`
		NextRetry    sql.NullTime `db:"next_retry_at"`
		AttemptCount int          `db:"attempt_count"`
	}
	err := w.db.GetContext(ctx, &latest, `
		SELECT sl.status, sl.next_retry_at, sl.attempt_count
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		WHERE sl.episode_id = ?
	`, episodeID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query latest sync_log: %w", err)
	}
	if latest.Status == "in_progress" {
		return fmt.Errorf("sync already in progress for episode %d", episodeID)
	}
	return nil
}

// EnqueuePendingEpisodes scans for all approved but un-synced episodes and enqueues them.
// Returns the number of episodes enqueued.
func (w *SyncWorker) EnqueuePendingEpisodes(ctx context.Context) (int, error) {
	ids, err := w.findPendingEpisodes(ctx, true)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if !w.tryMarkEnqueued(id) {
			continue
		}
		select {
		case w.enqueueCh <- id:
			count++
		default:
			w.unmarkEnqueued(id)
			logger.Printf("[SYNC-WORKER] Enqueue channel full, skipping episode %d", id)
		}
	}
	return count, nil
}

func (w *SyncWorker) run() {
	defer w.wg.Done()

	ctx := w.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	interval := time.Duration(w.cfg.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return

		case episodeID := <-w.enqueueCh:
			w.unmarkEnqueued(episodeID)
			w.processEpisodeWithMode(ctx, episodeID, true)

		case <-ticker.C:
			w.pollAndProcess(ctx)
		}
	}
}

func (w *SyncWorker) tryMarkEnqueued(episodeID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, exists := w.enqueuedEpisode[episodeID]; exists {
		return false
	}
	w.enqueuedEpisode[episodeID] = struct{}{}
	return true
}

func (w *SyncWorker) unmarkEnqueued(episodeID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.enqueuedEpisode, episodeID)
}

func (w *SyncWorker) pollAndProcess(ctx context.Context) {
	// First, retry any failed episodes that are due
	w.retryFailedEpisodes(ctx)

	// Then, find new pending episodes
	ids, err := w.findPendingEpisodes(ctx, false)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to find pending episodes: %v", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	logger.Printf("[SYNC-WORKER] Found %d episodes to sync", len(ids))

	// Process with concurrency limit
	sem := make(chan struct{}, w.cfg.MaxConcurrent)
	var wg sync.WaitGroup

	for _, id := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func(episodeID int64) {
			defer wg.Done()
			defer func() { <-sem }()
			w.processEpisode(ctx, episodeID)
		}(id)
	}

	wg.Wait()
}

func (w *SyncWorker) findPendingEpisodes(ctx context.Context, includeExhaustedFailures bool) ([]int64, error) {
	var ids []int64
	var err error
	query := `
		SELECT e.id
		FROM episodes e
		WHERE e.qa_status IN ('approved', 'inspector_approved')
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1
		    FROM sync_logs sl
		    INNER JOIN (
		      SELECT episode_id, MAX(id) AS latest_id
		      FROM sync_logs
		      GROUP BY episode_id
		    ) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		    WHERE sl.episode_id = e.id
		      AND sl.status = 'completed'
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl
		    WHERE sl.episode_id = e.id
		      AND sl.status = 'in_progress'
		  )
		  %s
		ORDER BY e.created_at ASC
		LIMIT ?
	`
	if !includeExhaustedFailures {
		query = fmt.Sprintf(query, `
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl
		    INNER JOIN (
		      SELECT episode_id, MAX(id) AS latest_id
		      FROM sync_logs
		      GROUP BY episode_id
		    ) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		    WHERE sl.episode_id = e.id
		      AND sl.status = 'failed'
		      AND sl.attempt_count >= ?
		  )`)
		err = w.db.SelectContext(ctx, &ids, query, w.cfg.MaxRetries, w.cfg.BatchSize)
	} else {
		query = fmt.Sprintf(query, "")
		err = w.db.SelectContext(ctx, &ids, query, w.cfg.BatchSize)
	}
	if err != nil {
		return nil, fmt.Errorf("query pending episodes: %w", err)
	}
	return ids, nil
}

func (w *SyncWorker) retryFailedEpisodes(ctx context.Context) {
	var ids []int64
	err := w.db.SelectContext(ctx, &ids, `
		SELECT sl.episode_id
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		WHERE sl.status = 'failed'
		  AND sl.attempt_count < ?
		  AND (sl.next_retry_at IS NULL OR sl.next_retry_at <= NOW())
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl2
		    WHERE sl2.episode_id = sl.episode_id
		      AND sl2.status = 'in_progress'
		  )
		ORDER BY sl.started_at ASC
		LIMIT ?
	`, w.cfg.MaxRetries, w.cfg.BatchSize)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to query retryable episodes: %v", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	sem := make(chan struct{}, w.cfg.MaxConcurrent)
	var wg sync.WaitGroup

	for _, id := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func(episodeID int64) {
			defer wg.Done()
			defer func() { <-sem }()
			w.processEpisode(ctx, episodeID)
		}(id)
	}

	wg.Wait()
}

func (w *SyncWorker) processEpisode(ctx context.Context, episodeID int64) {
	w.processEpisodeWithMode(ctx, episodeID, false)
}

func (w *SyncWorker) processEpisodeWithMode(ctx context.Context, episodeID int64, manual bool) {
	// Fetch episode details
	var ep struct {
		ID          int64  `db:"id"`
		EpisodeUUID string `db:"episode_id"`
		McapPath    string `db:"mcap_path"`
		SidecarPath string `db:"sidecar_path"`
		CloudSynced bool   `db:"cloud_synced"`
	}
	err := w.db.GetContext(ctx, &ep, `
		SELECT id, episode_id, mcap_path, sidecar_path, cloud_synced
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`, episodeID)
	if err == sql.ErrNoRows {
		logger.Printf("[SYNC-WORKER] Episode %d not found, skipping", episodeID)
		return
	}
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to query episode %d: %v", episodeID, err)
		return
	}

	if ep.CloudSynced {
		//logger.Printf("[SYNC-WORKER] Episode %d already synced, skipping", episodeID)
		return
	}

	// Extract the MinIO object key from the stored path (strip bucket prefix)
	mcapKey := stripBucketPrefix(ep.McapPath)
	sidecarKey := stripBucketPrefix(ep.SidecarPath)

	if mcapKey == "" {
		logger.Printf("[SYNC-WORKER] Episode %d has empty mcap_path, skipping", episodeID)
		return
	}

	// Reuse latest failed sync_log when retry is due, otherwise insert a new row.
	syncLogID, err := w.acquireSyncLogWithMode(ctx, episodeID, ep.McapPath, manual)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to acquire sync log for episode %d: %v", episodeID, err)
		return
	}

	startTime := time.Now()

	// Execute upload
	result, err := w.uploader.Upload(ctx, cloud.UploadRequest{
		EpisodeID:  ep.EpisodeUUID,
		McapKey:    mcapKey,
		SidecarKey: sidecarKey,
		RawTags: map[string]string{
			"episode_id": ep.EpisodeUUID,
		},
	})
	if err != nil {
		duration := int64(time.Since(startTime).Seconds())
		w.markSyncFailed(ctx, syncLogID, episodeID, duration, err)
		return
	}

	// Success: update episode and sync_log
	duration := int64(time.Since(startTime).Seconds())
	w.markSyncCompleted(ctx, syncLogID, episodeID, result, duration)
}

// acquireSyncLog returns the sync_logs id for this upload attempt. It reuses the latest
// failed row for the episode when a retry is due (and increments attempt_count); otherwise
// inserts a new row with attempt_count = 1.
func (w *SyncWorker) acquireSyncLog(ctx context.Context, episodeID int64, sourcePath string) (int64, error) {
	return w.acquireSyncLogWithMode(ctx, episodeID, sourcePath, false)
}

func (w *SyncWorker) acquireSyncLogWithMode(ctx context.Context, episodeID int64, sourcePath string, manual bool) (int64, error) {
	var reuseID int64
	err := w.db.GetContext(ctx, &reuseID, `
		SELECT sl.id FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		WHERE sl.episode_id = ?
		  AND sl.status = 'failed'
		  AND sl.attempt_count < ?
		  AND (sl.next_retry_at IS NULL OR sl.next_retry_at <= NOW())
	`, episodeID, w.cfg.MaxRetries)
	if err == nil && reuseID > 0 {
		now := time.Now().UTC()
		// Atomic claim: only one worker may move this row from failed → in_progress
		// (WHERE matches the reuse SELECT so a lost race yields RowsAffected 0).
		res, updErr := w.db.ExecContext(ctx, `
			UPDATE sync_logs
			SET status = 'in_progress',
			    source_path = ?,
			    started_at = ?,
			    error_message = NULL,
			    duration_sec = NULL,
			    completed_at = NULL,
			    next_retry_at = NULL,
			    attempt_count = attempt_count + 1
			WHERE id = ?
			  AND status = 'failed'
			  AND attempt_count < ?
			  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		`, sourcePath, now, reuseID, w.cfg.MaxRetries)
		if updErr != nil {
			return 0, fmt.Errorf("reuse sync_log: %w", updErr)
		}
		n, raErr := res.RowsAffected()
		if raErr != nil {
			return 0, fmt.Errorf("reuse sync_log rows affected: %w", raErr)
		}
		if n != 1 {
			return 0, fmt.Errorf("retry claim lost for sync_log %d (concurrent worker or state changed)", reuseID)
		}
		return reuseID, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("query reusable sync_log: %w", err)
	}

	var latest struct {
		Status       string       `db:"status"`
		NextRetry    sql.NullTime `db:"next_retry_at"`
		AttemptCount int          `db:"attempt_count"`
	}
	qErr := w.db.GetContext(ctx, &latest, `
		SELECT sl.status, sl.next_retry_at, sl.attempt_count
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		WHERE sl.episode_id = ?
	`, episodeID)
	if qErr == nil {
		switch latest.Status {
		case "in_progress":
			return 0, fmt.Errorf("sync already in progress for episode %d", episodeID)
		case "failed":
			if !manual && latest.AttemptCount >= w.cfg.MaxRetries {
				return 0, fmt.Errorf("max retries exceeded for episode %d", episodeID)
			}
			if !manual && latest.NextRetry.Valid && latest.NextRetry.Time.After(time.Now().UTC()) {
				return 0, fmt.Errorf("retry backoff active for episode %d", episodeID)
			}
		}
	} else if qErr != sql.ErrNoRows {
		return 0, fmt.Errorf("query latest sync_log: %w", qErr)
	}

	return w.insertSyncLog(ctx, episodeID, sourcePath, manual)
}

func (w *SyncWorker) insertSyncLog(ctx context.Context, episodeID int64, sourcePath string, manual bool) (int64, error) {
	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin sync_log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var latest struct {
		Status       string       `db:"status"`
		NextRetry    sql.NullTime `db:"next_retry_at"`
		AttemptCount int          `db:"attempt_count"`
	}
	err = tx.GetContext(ctx, &latest, `
		SELECT sl.status, sl.next_retry_at, sl.attempt_count
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		WHERE sl.episode_id = ?
		FOR UPDATE
	`, episodeID)
	if err == nil {
		switch latest.Status {
		case "in_progress":
			return 0, fmt.Errorf("sync already in progress for episode %d", episodeID)
		case "completed":
			return 0, fmt.Errorf("episode %d already has completed sync_log", episodeID)
		case "failed":
			if !manual && latest.AttemptCount >= w.cfg.MaxRetries {
				return 0, fmt.Errorf("max retries exceeded for episode %d", episodeID)
			}
			if !manual && latest.NextRetry.Valid && latest.NextRetry.Time.After(time.Now().UTC()) {
				return 0, fmt.Errorf("retry backoff active for episode %d", episodeID)
			}
		}
	} else if err != sql.ErrNoRows {
		return 0, fmt.Errorf("lock latest sync_log: %w", err)
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO sync_logs (episode_id, source_path, status, attempt_count, started_at)
		VALUES (?, ?, 'in_progress', 1, ?)
	`, episodeID, sourcePath, now)
	if err != nil {
		return 0, fmt.Errorf("insert sync_log: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sync_log last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit sync_log insert: %w", err)
	}
	return id, nil
}

func (w *SyncWorker) markSyncCompleted(ctx context.Context, syncLogID, episodeID int64, result *cloud.UploadResult, durationSec int64) {
	now := time.Now().UTC()

	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to begin transaction for episode %d: %v", episodeID, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Update sync_log
	if _, err := tx.ExecContext(ctx, `
		UPDATE sync_logs
		SET status = 'completed',
		    destination_path = ?,
		    bytes_transferred = ?,
		    duration_sec = ?,
		    completed_at = ?
		WHERE id = ?
	`, result.ObjectKey, result.FileSize, durationSec, now, syncLogID); err != nil {
		logger.Printf("[SYNC-WORKER] Failed to update sync log %d: %v", syncLogID, err)
		return
	}

	// Update episode
	if _, err := tx.ExecContext(ctx, `
		UPDATE episodes
		SET cloud_synced = TRUE,
		    cloud_synced_at = ?,
		    cloud_mcap_path = ?,
		    cloud_processed = FALSE
		WHERE id = ? AND deleted_at IS NULL
	`, now, result.ObjectKey, episodeID); err != nil {
		logger.Printf("[SYNC-WORKER] Failed to update episode %d cloud status: %v", episodeID, err)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[SYNC-WORKER] Failed to commit sync completion for episode %d: %v", episodeID, err)
		return
	}

	logger.Printf("[SYNC-WORKER] Episode %d synced successfully: upload_id=%s object_key=%s duration=%ds",
		episodeID, result.UploadID, result.ObjectKey, durationSec)
}

func (w *SyncWorker) markSyncFailed(ctx context.Context, syncLogID, episodeID, durationSec int64, uploadErr error) {
	now := time.Now().UTC()
	errMsg := uploadErr.Error()

	// attempt_count is the 1-based index of this upload attempt (incremented when a failed row is reused).
	var attemptCount int
	if err := w.db.GetContext(ctx, &attemptCount, "SELECT attempt_count FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		attemptCount = 1
	}

	// Exponential backoff after this failure: min(2^attempt * 5s, 30s)
	backoffSec := math.Min(math.Pow(2, float64(attemptCount))*5, 30)
	nextRetry := now.Add(time.Duration(backoffSec) * time.Second)

	if _, err := w.db.ExecContext(ctx, `
		UPDATE sync_logs
		SET status = 'failed',
		    error_message = ?,
		    duration_sec = ?,
		    completed_at = ?,
		    next_retry_at = ?
		WHERE id = ?
	`, errMsg, durationSec, now, nextRetry, syncLogID); err != nil {
		logger.Printf("[SYNC-WORKER] Failed to update sync log %d as failed: %v", syncLogID, err)
	}

	logger.Printf("[SYNC-WORKER] Episode %d sync failed: %v (attempt=%d, next_retry=%v)",
		episodeID, uploadErr, attemptCount, nextRetry.Format(time.RFC3339))
}

// stripBucketPrefix removes the leading "bucket/" prefix from a stored path.
// Stored paths look like "edge-factory-default/factory-default/device/date/task.mcap".
func stripBucketPrefix(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	if idx := strings.Index(path, "/"); idx > 0 {
		return path[idx+1:]
	}
	return path
}
