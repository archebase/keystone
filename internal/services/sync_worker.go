// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"archebase.com/keystone-edge/internal/cloud"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/jmoiron/sqlx"
	"github.com/minio/minio-go/v7"
)

// SyncWorkerConfig provides the runtime configuration for the sync worker.
type SyncWorkerConfig struct {
	BatchSize      int
	MaxConcurrent  int
	MaxRetries     int
	IntervalSec    int
	RetryBaseSec   int
	RetryMaxSec    int
	RetryJitterSec int
}

type syncEnqueueRequest struct {
	episodeID int64
	manual    bool
}

// SyncWorker is a background goroutine that periodically scans for approved
// episodes and uploads them to cloud via the data-platform gateway.
type SyncWorker struct {
	db          *sqlx.DB
	uploader    *cloud.Uploader
	minioClient *s3.Client
	minioBucket string
	cfg         SyncWorkerConfig
	syncCfg     *config.SyncConfig

	mu              sync.Mutex
	enqueuedEpisode map[int64]struct{}
	stopDone        chan struct{}

	running  atomic.Bool
	stopping atomic.Bool
	wg       sync.WaitGroup

	// runCtx is cancelled when Stop() is called so in-flight uploads and DB ops can exit promptly.
	runCtx    context.Context
	runCancel context.CancelFunc

	// enqueueCh allows the API handler to inject specific episode IDs for immediate scheduling.
	enqueueCh chan syncEnqueueRequest
	// jobCh is consumed by worker goroutines that execute uploads concurrently.
	jobCh chan syncEnqueueRequest

	workersWg sync.WaitGroup
}

var (
	// ErrEpisodeAlreadyEnqueued is returned when the episode is already in the sync queue.
	ErrEpisodeAlreadyEnqueued = errors.New("sync episode already enqueued")
	// ErrSyncQueueFull is returned when the non-blocking enqueue channel is full.
	ErrSyncQueueFull = errors.New("sync enqueue channel full")
	// ErrSyncAlreadyInProgress is returned when a conflicting sync operation is active.
	ErrSyncAlreadyInProgress = errors.New("sync already in progress")
	// ErrSyncWorkerNotRunning is returned when Start has not been called or after Stop.
	ErrSyncWorkerNotRunning = errors.New("sync worker is not running")

	errSyncRetryBackoffActive = errors.New("sync retry backoff active")
	errSyncRetryExhausted     = errors.New("sync retry max retries exceeded")
	errSyncAlreadyCompleted   = errors.New("sync already completed")
)

// NewSyncWorker creates a new sync worker. Call Start() to begin background processing.
func NewSyncWorker(db *sqlx.DB, uploader *cloud.Uploader, minioClient *s3.Client, minioBucket string, cfg SyncWorkerConfig, syncCfg *config.SyncConfig) *SyncWorker {
	return &SyncWorker{
		db:              db,
		uploader:        uploader,
		minioClient:     minioClient,
		minioBucket:     minioBucket,
		cfg:             cfg,
		syncCfg:         syncCfg,
		enqueueCh:       make(chan syncEnqueueRequest, 100),
		enqueuedEpisode: make(map[int64]struct{}),
	}
}

// Start begins the background sync worker loop.
func (w *SyncWorker) Start() {
	w.mu.Lock()
	if w.stopping.Load() {
		w.mu.Unlock()
		logger.Printf("[SYNC-WORKER] Start skipped: worker is stopping")
		return
	}
	if !w.running.CompareAndSwap(false, true) {
		w.mu.Unlock()
		return
	}

	w.stopDone = make(chan struct{})
	w.jobCh = make(chan syncEnqueueRequest, max(1, w.cfg.BatchSize*2))
	w.runCtx, w.runCancel = context.WithCancel(context.Background())
	jobCh := w.jobCh
	runCtx := w.runCtx
	w.mu.Unlock()

	workerCount := max(1, w.cfg.MaxConcurrent)
	for i := 0; i < workerCount; i++ {
		w.workersWg.Add(1)
		go w.worker(runCtx, jobCh)
	}

	w.wg.Add(1)
	go w.run(runCtx)
	logger.Printf("[SYNC-WORKER] Started (interval=%ds, batch=%d, concurrency=%d)",
		w.cfg.IntervalSec, w.cfg.BatchSize, w.cfg.MaxConcurrent)
}

// Stop gracefully stops the sync worker within the provided context deadline.
func (w *SyncWorker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.running.Load() {
		done := w.stopDone
		w.mu.Unlock()
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("sync worker stop timeout: %w", ctx.Err())
		}
	}

	if !w.stopping.CompareAndSwap(false, true) {
		done := w.stopDone
		w.mu.Unlock()
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("sync worker stop timeout: %w", ctx.Err())
		}
	}

	done := w.stopDone
	runCancel := w.runCancel
	w.running.Store(false)
	w.mu.Unlock()

	if runCancel != nil {
		runCancel()
	}

	if done == nil {
		return nil
	}

	select {
	case <-done:
		logger.Println("[SYNC-WORKER] Stopped")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sync worker stop timeout: %w", ctx.Err())
	}
}

// IsRunning returns whether the worker is currently running.
func (w *SyncWorker) IsRunning() bool {
	return w.running.Load()
}

// MaxRetries returns the configured automatic retry limit.
func (w *SyncWorker) MaxRetries() int {
	return w.cfg.MaxRetries
}

// EnqueueEpisode adds a specific episode ID for immediate sync processing.
func (w *SyncWorker) EnqueueEpisode(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisode(ctx, episodeID, false)
}

// EnqueueEpisodeManual adds a specific episode ID for immediate sync processing,
// allowing explicit API-triggered retries even after automatic retries are exhausted.
func (w *SyncWorker) EnqueueEpisodeManual(ctx context.Context, episodeID int64) error {
	if !w.running.Load() {
		return ErrSyncWorkerNotRunning
	}
	if err := w.persistPendingSyncLog(ctx, episodeID, true); err != nil {
		return err
	}
	w.enqueuePersistedEpisode(ctx, syncEnqueueRequest{episodeID: episodeID, manual: true})
	return nil
}

func (w *SyncWorker) enqueueEpisode(ctx context.Context, episodeID int64, manual bool) error {
	if !w.running.Load() {
		return ErrSyncWorkerNotRunning
	}

	if !w.tryMarkEnqueued(episodeID) {
		return ErrEpisodeAlreadyEnqueued
	}

	select {
	case w.enqueueCh <- syncEnqueueRequest{episodeID: episodeID, manual: manual}:
		return nil
	case <-ctx.Done():
		w.unmarkEnqueued(episodeID)
		return ctx.Err()
	default:
		w.unmarkEnqueued(episodeID)
		return ErrSyncQueueFull
	}
}

func (w *SyncWorker) enqueuePersistedEpisode(ctx context.Context, req syncEnqueueRequest) {
	if !w.tryMarkEnqueued(req.episodeID) {
		return
	}

	select {
	case w.enqueueCh <- req:
	case <-ctx.Done():
		w.unmarkEnqueued(req.episodeID)
	default:
		w.unmarkEnqueued(req.episodeID)
		logger.Printf("[SYNC-WORKER] Persistent enqueue for episode %d will be recovered by polling", req.episodeID)
	}
}

func (w *SyncWorker) persistPendingSyncLog(ctx context.Context, episodeID int64, manual bool) error {
	if w.db == nil {
		return nil
	}

	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pending sync_log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)
	var episode struct {
		ID          int64 `db:"id"`
		CloudSynced bool  `db:"cloud_synced"`
	}
	if err := tx.GetContext(ctx, &episode, `
		SELECT id, cloud_synced
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("episode %d not found", episodeID)
		}
		return fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	if episode.CloudSynced {
		return fmt.Errorf("episode %d already synced", episodeID)
	}

	var activeCount int
	if err := tx.GetContext(ctx, &activeCount, `
		SELECT COUNT(*)
		FROM sync_logs
		WHERE episode_id = ?
		  AND status IN ('pending', 'in_progress')
	`, episodeID); err != nil {
		return fmt.Errorf("query active sync_log count: %w", err)
	}
	if activeCount > 0 {
		return fmt.Errorf("%w for episode %d", ErrSyncAlreadyInProgress, episodeID)
	}

	var latest struct {
		ID           int64        `db:"id"`
		Status       string       `db:"status"`
		NextRetry    sql.NullTime `db:"next_retry_at"`
		AttemptCount int          `db:"attempt_count"`
	}
	err = tx.GetContext(ctx, &latest, `
		SELECT id, status, next_retry_at, attempt_count
		FROM sync_logs
		WHERE episode_id = ?
		ORDER BY id DESC
		LIMIT 1
	`+lockClause, episodeID)
	if err == sql.ErrNoRows {
		if err := insertPendingSyncLog(ctx, tx, episodeID, time.Now().UTC(), 0); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("lock latest sync_log: %w", err)
	}

	now := time.Now().UTC()
	switch latest.Status {
	case "pending", "in_progress":
		return fmt.Errorf("%w for episode %d", ErrSyncAlreadyInProgress, episodeID)
	case "completed":
		return fmt.Errorf("%w for episode %d", errSyncAlreadyCompleted, episodeID)
	case "failed":
		retryDue := !latest.NextRetry.Valid || !latest.NextRetry.Time.After(now)
		if latest.AttemptCount < w.cfg.MaxRetries && retryDue {
			if err := promoteFailedSyncLogToPending(ctx, tx, latest.ID, now); err != nil {
				return err
			}
			return tx.Commit()
		}
		if !manual && latest.AttemptCount >= w.cfg.MaxRetries {
			return fmt.Errorf("%w for episode %d", errSyncRetryExhausted, episodeID)
		}
		if !manual && !retryDue {
			return fmt.Errorf("%w for episode %d", errSyncRetryBackoffActive, episodeID)
		}
		if err := insertPendingSyncLog(ctx, tx, episodeID, now, 0); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("unknown sync status %q for episode %d", latest.Status, episodeID)
	}
}

func insertPendingSyncLog(ctx context.Context, tx *sqlx.Tx, episodeID int64, queuedAt time.Time, attemptCount int) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_logs (episode_id, status, attempt_count, started_at)
		VALUES (?, 'pending', ?, ?)
	`, episodeID, attemptCount, queuedAt); err != nil {
		return fmt.Errorf("insert pending sync_log: %w", err)
	}
	return nil
}

func promoteFailedSyncLogToPending(ctx context.Context, tx *sqlx.Tx, syncLogID int64, queuedAt time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE sync_logs
		SET status = 'pending',
		    started_at = ?,
		    error_message = NULL,
		    duration_sec = NULL,
		    completed_at = NULL,
		    next_retry_at = NULL
		WHERE id = ?
		  AND status = 'failed'
	`, queuedAt, syncLogID)
	if err != nil {
		return fmt.Errorf("promote failed sync_log to pending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("promote failed sync_log rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("promote failed sync_log %d lost state", syncLogID)
	}
	return nil
}

func txLockClause(tx *sqlx.Tx) string {
	if tx.DriverName() == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}

func isSkippablePendingError(err error) bool {
	return errors.Is(err, ErrSyncAlreadyInProgress) ||
		errors.Is(err, errSyncRetryBackoffActive) ||
		errors.Is(err, errSyncRetryExhausted) ||
		errors.Is(err, errSyncAlreadyCompleted)
}

// EnqueuePendingEpisodes scans for all approved but un-synced episodes and enqueues them.
// Returns the number of episodes enqueued.
func (w *SyncWorker) EnqueuePendingEpisodes(ctx context.Context) (int, error) {
	if !w.running.Load() {
		return 0, ErrSyncWorkerNotRunning
	}

	ids, err := w.findPendingEpisodes(ctx, false)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if err := w.persistPendingSyncLog(ctx, id, false); err != nil {
			if isSkippablePendingError(err) {
				continue
			}
			logger.Printf("[SYNC-WORKER] Failed to persist pending sync for episode %d: %v", id, err)
			continue
		}
		count++
		w.enqueuePersistedEpisode(ctx, syncEnqueueRequest{episodeID: id, manual: false})
	}
	return count, nil
}

func (w *SyncWorker) run(ctx context.Context) {
	defer w.finalizeRun()

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
		case <-ctx.Done():
			return
		case req := <-w.enqueueCh:
			w.dispatchJob(ctx, req)
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

func (w *SyncWorker) worker(ctx context.Context, jobCh <-chan syncEnqueueRequest) {
	defer w.workersWg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-jobCh:
			w.processEnqueuedEpisode(ctx, req)
		}
	}
}

func (w *SyncWorker) processEnqueuedEpisode(ctx context.Context, req syncEnqueueRequest) {
	w.processEnqueuedEpisodeWith(ctx, req, w.processEpisodeWithMode)
}

func (w *SyncWorker) processEnqueuedEpisodeWith(ctx context.Context, req syncEnqueueRequest, process func(context.Context, int64, bool)) {
	defer w.unmarkEnqueued(req.episodeID)
	process(ctx, req.episodeID, req.manual)
}

func (w *SyncWorker) dispatchJob(ctx context.Context, req syncEnqueueRequest) {
	w.mu.Lock()
	jobCh := w.jobCh
	w.mu.Unlock()
	if jobCh == nil {
		w.unmarkEnqueued(req.episodeID)
		return
	}

	select {
	case <-ctx.Done():
		w.unmarkEnqueued(req.episodeID)
	case jobCh <- req:
	}
}

func (w *SyncWorker) clearPendingEnqueues() {
	for {
		select {
		case req := <-w.enqueueCh:
			w.unmarkEnqueued(req.episodeID)
		default:
			return
		}
	}
}

func (w *SyncWorker) clearPendingJobs() {
	w.mu.Lock()
	jobCh := w.jobCh
	w.mu.Unlock()
	if jobCh == nil {
		return
	}
	for {
		select {
		case req := <-jobCh:
			w.unmarkEnqueued(req.episodeID)
		default:
			return
		}
	}
}

func (w *SyncWorker) finalizeRun() {
	w.clearPendingJobs()
	w.clearPendingEnqueues()
	w.wg.Done()
	w.workersWg.Wait()

	w.mu.Lock()
	done := w.stopDone
	w.stopDone = nil
	w.jobCh = nil
	w.runCtx = nil
	w.runCancel = nil
	w.stopping.Store(false)
	w.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (w *SyncWorker) pollAndProcess(ctx context.Context) {
	// Recover persisted queued rows first; enqueueCh is only an acceleration path.
	w.dispatchPendingSyncLogs(ctx)

	// Then, retry any failed episodes that are due.
	w.retryFailedEpisodes(ctx)

	// Finally, find newly eligible episodes and persist them as queued work.
	ids, err := w.findPendingEpisodes(ctx, false)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to find pending episodes: %v", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	logger.Printf("[SYNC-WORKER] Found %d episodes to sync", len(ids))

	for _, id := range ids {
		if err := w.persistPendingSyncLog(ctx, id, false); err != nil {
			if isSkippablePendingError(err) {
				continue
			}
			logger.Printf("[SYNC-WORKER] Failed to persist pending sync for episode %d: %v", id, err)
			continue
		}
		w.dispatchPersistedJob(ctx, syncEnqueueRequest{episodeID: id, manual: false})
	}
}

func (w *SyncWorker) dispatchPendingSyncLogs(ctx context.Context) {
	ids, err := w.findPendingSyncLogEpisodes(ctx)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to find queued sync logs: %v", err)
		return
	}
	for _, id := range ids {
		w.dispatchPersistedJob(ctx, syncEnqueueRequest{episodeID: id, manual: false})
	}
}

func (w *SyncWorker) dispatchPersistedJob(ctx context.Context, req syncEnqueueRequest) {
	if !w.tryMarkEnqueued(req.episodeID) {
		return
	}
	w.dispatchJob(ctx, req)
}

func (w *SyncWorker) findPendingSyncLogEpisodes(ctx context.Context) ([]int64, error) {
	var ids []int64
	if err := w.db.SelectContext(ctx, &ids, `
		SELECT latest_log.episode_id
		FROM sync_logs latest_log
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) latest ON latest_log.episode_id = latest.episode_id AND latest_log.id = latest.latest_id
		INNER JOIN episodes e ON e.id = latest_log.episode_id
		WHERE latest_log.status = 'pending'
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		ORDER BY latest_log.started_at ASC, latest_log.id ASC
		LIMIT ?
	`, w.cfg.BatchSize); err != nil {
		return nil, fmt.Errorf("query pending sync logs: %w", err)
	}
	return ids, nil
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
		      AND sl.status IN ('pending', 'in_progress')
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
	now := time.Now().UTC()
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
		  AND (sl.next_retry_at IS NULL OR sl.next_retry_at <= ?)
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl2
		    WHERE sl2.episode_id = sl.episode_id
		      AND sl2.status IN ('pending', 'in_progress')
		)
		ORDER BY sl.started_at ASC
		LIMIT ?
	`, w.cfg.MaxRetries, now, w.cfg.BatchSize)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to query retryable episodes: %v", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	for _, id := range ids {
		if err := w.persistPendingSyncLog(ctx, id, false); err != nil {
			if isSkippablePendingError(err) {
				continue
			}
			logger.Printf("[SYNC-WORKER] Failed to queue retry for episode %d: %v", id, err)
			continue
		}
		w.dispatchPersistedJob(ctx, syncEnqueueRequest{episodeID: id, manual: false})
	}
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

	if mcapKey == "" {
		logger.Printf("[SYNC-WORKER] Episode %d has empty mcap_path, skipping", episodeID)
		return
	}

	// Build raw tags from sidecar JSON (best-effort: log and continue on failure).
	rawTags := map[string]string{
		"episode_id": ep.EpisodeUUID,
	}
	if sidecarTags, err := w.tagsFromSidecar(ctx, ep.SidecarPath); err != nil {
		logger.Printf("[SYNC-WORKER] Episode %d: failed to read sidecar tags, uploading without them: %v", episodeID, err)
	} else {
		for k, v := range sidecarTags {
			rawTags[k] = v
		}
	}

	// Reuse latest failed sync_log when retry is due, otherwise insert a new row.
	syncLogID, attemptCount, err := w.acquireSyncLogWithMode(ctx, episodeID, ep.McapPath, manual)
	if err != nil {
		//logger.Printf("[SYNC-WORKER] Failed to acquire sync log for episode %d: %v", episodeID, err)
		return
	}

	startTime := time.Now()

	// Execute upload
	result, err := w.uploader.Upload(ctx, cloud.UploadRequest{
		EpisodeID: ep.EpisodeUUID,
		McapKey:   mcapKey,
		RawTags:   rawTags,
	})
	if err != nil {
		duration := int64(time.Since(startTime).Seconds())
		w.markSyncFailed(ctx, syncLogID, episodeID, duration, err, attemptCount)
		return
	}

	// Success: update episode and sync_log
	duration := int64(time.Since(startTime).Seconds())
	w.markSyncCompleted(ctx, syncLogID, episodeID, result, duration)
}

func (w *SyncWorker) acquireSyncLogWithMode(ctx context.Context, episodeID int64, sourcePath string, manual bool) (int64, int, error) {
	// NOTE: This must be lock-protected. A plain "check then insert" is vulnerable to TOCTOU
	// and, when there is no existing sync_logs row, there is nothing to lock with FOR UPDATE.
	// We serialize claims per-episode by locking the parent episodes row first.
	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin sync_log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)

	// Serialize per episode even when sync_logs is empty for this episode.
	var lockedEpisodeID int64
	if err := tx.GetContext(ctx, &lockedEpisodeID, `
		SELECT id
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("episode %d not found", episodeID)
		}
		return 0, 0, fmt.Errorf("lock episode %d: %w", episodeID, err)
	}

	var latest struct {
		ID           int64        `db:"id"`
		Status       string       `db:"status"`
		NextRetry    sql.NullTime `db:"next_retry_at"`
		AttemptCount int          `db:"attempt_count"`
	}
	latestQuery := `
			SELECT sl.id, sl.status, sl.next_retry_at, sl.attempt_count
			FROM sync_logs sl
			INNER JOIN (
			  SELECT episode_id, MAX(id) AS latest_id
			  FROM sync_logs
			  GROUP BY episode_id
			) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
			WHERE sl.episode_id = ?
		` + lockClause
	err = tx.GetContext(ctx, &latest, latestQuery, episodeID)
	if err == nil {
		now := time.Now().UTC()
		switch latest.Status {
		case "pending":
			claimedAttemptCount := latest.AttemptCount + 1
			if latest.AttemptCount < 1 {
				claimedAttemptCount = 1
			}
			res, updErr := tx.ExecContext(ctx, `
				UPDATE sync_logs
				SET status = 'in_progress',
				    source_path = ?,
				    started_at = ?,
				    error_message = NULL,
				    duration_sec = NULL,
				    completed_at = NULL,
				    next_retry_at = NULL,
				    attempt_count = ?
				WHERE id = ?
				  AND status = 'pending'
			`, sourcePath, now, claimedAttemptCount, latest.ID)
			if updErr != nil {
				return 0, 0, fmt.Errorf("claim pending sync_log: %w", updErr)
			}
			n, raErr := res.RowsAffected()
			if raErr != nil {
				return 0, 0, fmt.Errorf("claim pending sync_log rows affected: %w", raErr)
			}
			if n != 1 {
				return 0, 0, fmt.Errorf("pending claim lost for sync_log %d (state changed)", latest.ID)
			}
			if err := tx.Commit(); err != nil {
				return 0, 0, fmt.Errorf("commit pending sync_log claim: %w", err)
			}
			return latest.ID, claimedAttemptCount, nil
		case "in_progress":
			return 0, 0, fmt.Errorf("%w for episode %d", ErrSyncAlreadyInProgress, episodeID)
		case "completed":
			return 0, 0, fmt.Errorf("episode %d already has completed sync_log", episodeID)
		case "failed":
			retryDue := !latest.NextRetry.Valid || !latest.NextRetry.Time.After(now)
			if latest.AttemptCount < w.cfg.MaxRetries && retryDue {
				res, updErr := tx.ExecContext(ctx, `
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
				`, sourcePath, now, latest.ID)
				if updErr != nil {
					return 0, 0, fmt.Errorf("reuse sync_log: %w", updErr)
				}
				n, raErr := res.RowsAffected()
				if raErr != nil {
					return 0, 0, fmt.Errorf("reuse sync_log rows affected: %w", raErr)
				}
				if n != 1 {
					return 0, 0, fmt.Errorf("retry claim lost for sync_log %d (state changed)", latest.ID)
				}
				if err := tx.Commit(); err != nil {
					return 0, 0, fmt.Errorf("commit sync_log reuse: %w", err)
				}
				return latest.ID, latest.AttemptCount + 1, nil
			}

			if !manual && latest.AttemptCount >= w.cfg.MaxRetries {
				return 0, 0, fmt.Errorf("max retries exceeded for episode %d", episodeID)
			}
			if !manual && latest.NextRetry.Valid && latest.NextRetry.Time.After(now) {
				return 0, 0, fmt.Errorf("retry backoff active for episode %d", episodeID)
			}
			// manual=true intentionally bypasses exhausted-retry and backoff guards above.
			// Falling through to INSERT creates a fresh sync_log row (attempt_count=1)
			// so operator-triggered retries are recorded as a new attempt chain.
		}
	} else if err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("lock latest sync_log: %w", err)
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO sync_logs (episode_id, source_path, status, attempt_count, started_at)
		VALUES (?, ?, 'in_progress', 1, ?)
	`, episodeID, sourcePath, now)
	if err != nil {
		return 0, 0, fmt.Errorf("insert sync_log: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("sync_log last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit sync_log insert: %w", err)
	}
	return id, 1, nil
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

	logger.Printf("[SYNC-WORKER] Episode %d synced successfully: logical_upload_id=%s upload_id=%s object_key=%s duration=%ds",
		episodeID, result.LogicalUploadID, result.UploadID, result.ObjectKey, durationSec)
}

func (w *SyncWorker) markSyncFailed(ctx context.Context, syncLogID, episodeID, durationSec int64, uploadErr error, attemptCount int) {
	now := time.Now().UTC()
	errMsg := uploadErr.Error()

	backoff := w.nextRetryDelay(attemptCount)
	nextRetry := now.Add(backoff)

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

func (w *SyncWorker) nextRetryDelay(attemptCount int) time.Duration {
	baseSec := w.cfg.RetryBaseSec
	if baseSec <= 0 {
		baseSec = 30
	}

	maxSec := w.cfg.RetryMaxSec
	if maxSec <= 0 {
		maxSec = 1800
	}
	if maxSec < baseSec {
		maxSec = baseSec
	}

	jitterSec := w.cfg.RetryJitterSec
	if jitterSec < 0 {
		jitterSec = 0
	}

	if attemptCount < 1 {
		attemptCount = 1
	}

	exponent := attemptCount - 1
	if exponent > 20 {
		exponent = 20
	}

	backoffSec := math.Min(float64(baseSec)*math.Pow(2, float64(exponent)), float64(maxSec))
	jitter := 0
	if jitterSec > 0 {
		// #nosec G404 -- retry backoff jitter only, not cryptographic randomness
		jitter = rand.Intn(jitterSec + 1)
	}

	totalSec := backoffSec + float64(jitter)
	if totalSec > float64(maxSec) {
		totalSec = float64(maxSec)
	}

	return time.Duration(totalSec * float64(time.Second))
}

// tagsFromSidecar reads the sidecar JSON from MinIO and returns it as a flat string map
// for use as RawTags. topics_summary is excluded. Returns nil map and an error if the
// sidecar path is empty, the object cannot be read, or the JSON is malformed.
func (w *SyncWorker) tagsFromSidecar(ctx context.Context, sidecarPath string) (map[string]string, error) {
	key := stripBucketPrefix(sidecarPath)
	if key == "" {
		return nil, fmt.Errorf("empty sidecar_path")
	}
	if w.minioClient == nil {
		return nil, fmt.Errorf("minio client not available")
	}

	obj, err := w.minioClient.GetObject(ctx, w.minioBucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get sidecar object %s: %w", key, err)
	}
	defer func() {
		_ = obj.Close()
	}()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read sidecar object %s: %w", key, err)
	}

	tags, err := flattenSidecar(data)
	if err != nil {
		return nil, fmt.Errorf("flatten sidecar %s: %w", key, err)
	}
	return tags, nil
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
