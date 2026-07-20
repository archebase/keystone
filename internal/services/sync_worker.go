// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"crypto/md5" // #nosec G501 -- Hilbert raw-data API requires an MD5 bagDigest field.
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/jmoiron/sqlx"
	"github.com/minio/minio-go/v7"
)

// SyncWorkerConfig provides the runtime configuration for the sync worker.
type SyncWorkerConfig struct {
	BatchSize       int
	MaxConcurrent   int
	MaxRetries      int
	AutoScanEnabled bool
	IntervalSec     int
	RetryBaseSec    int
	RetryMaxSec     int
	RetryJitterSec  int
}

type syncEnqueueRequest struct {
	episodeID int64
	manual    bool
	resync    bool
}

type syncEpisodeUploadRow struct {
	ID                      int64           `db:"id"`
	EpisodeUUID             string          `db:"episode_id"`
	DCPlanID                sql.NullInt64   `db:"dc_plan_id"`
	LocalDCPlanID           sql.NullInt64   `db:"local_dc_plan_id"`
	ProjectedDCPlanID       sql.NullInt64   `db:"projected_dc_plan_id"`
	WorkspaceID             sql.NullInt64   `db:"workspace_id"`
	DCPlanName              sql.NullString  `db:"dc_plan_name"`
	DCType                  sql.NullString  `db:"dc_type"`
	McapPath                string          `db:"mcap_path"`
	SidecarPath             string          `db:"sidecar_path"`
	CloudSynced             bool            `db:"cloud_synced"`
	Metadata                sql.NullString  `db:"metadata"`
	WorkstationID           sql.NullInt64   `db:"workstation_id"`
	DataCollectorOperatorID sql.NullString  `db:"data_collector_operator_id"`
	DataCollectorName       sql.NullString  `db:"data_collector_name"`
	DurationSec             sql.NullFloat64 `db:"duration_sec"`
	CreatedAt               time.Time       `db:"created_at"`
}

type hilbertRawDataClient interface {
	RegisterRawData(ctx context.Context, request auth.HilbertRawDataRegisterRequest) (int64, error)
	GetRawDataUploadCredentials(ctx context.Context, workspaceID, rawDataID int64) (*auth.HilbertRawDataUploadCredentials, error)
	FinishRawDataUpload(ctx context.Context, workspaceID, rawDataID int64) error
}

type tosObjectUploader interface {
	PutObject(ctx context.Context, target cloud.TOSS3UploadTarget, reader io.Reader, size int64, progress cloud.UploadProgressFunc) (string, error)
}

// SourceObjectReader reads source MCAP objects for Hilbert sync.
type SourceObjectReader interface {
	StatObject(ctx context.Context, bucket, objectName string) (int64, error)
	OpenObject(ctx context.Context, bucket, objectName string) (io.ReadCloser, error)
}

type minioSourceObjectReader struct {
	client *s3.Client
}

// NewMinioSourceObjectReader adapts the configured S3 client for source reads.
func NewMinioSourceObjectReader(client *s3.Client) SourceObjectReader {
	if client == nil {
		return nil
	}
	return minioSourceObjectReader{client: client}
}

func (r minioSourceObjectReader) StatObject(ctx context.Context, bucket, objectName string) (int64, error) {
	if r.client == nil {
		return 0, fmt.Errorf("minio client not available")
	}
	objInfo, err := r.client.StatObject(ctx, bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return objInfo.Size, nil
}

func (r minioSourceObjectReader) OpenObject(ctx context.Context, bucket, objectName string) (io.ReadCloser, error) {
	if r.client == nil {
		return nil, fmt.Errorf("minio client not available")
	}
	return r.client.GetObject(ctx, bucket, objectName, minio.GetObjectOptions{})
}

type hilbertEpisodeUploadContext struct {
	DCPlanID    int64
	WorkspaceID int64
}

func (c hilbertEpisodeUploadContext) clientHints() map[string]string { //nolint:unused // Reserved for direct DP upload mode.
	return map[string]string{
		"dc_plan_id":   strconv.FormatInt(c.DCPlanID, 10),
		"workspace_id": strconv.FormatInt(c.WorkspaceID, 10),
	}
}

func hilbertUploadContext(ep syncEpisodeUploadRow) (hilbertEpisodeUploadContext, error) {
	if !ep.DCPlanID.Valid || ep.DCPlanID.Int64 <= 0 {
		if ep.LocalDCPlanID.Valid && ep.LocalDCPlanID.Int64 > 0 {
			return hilbertEpisodeUploadContext{}, newNonRetryableSyncError(
				"episode %d has local_dc_plan_id %d but Hilbert upload requires dc_plan_id",
				ep.ID,
				ep.LocalDCPlanID.Int64,
			)
		}
		return hilbertEpisodeUploadContext{}, newNonRetryableSyncError("episode %d missing dc_plan_id", ep.ID)
	}
	if !ep.ProjectedDCPlanID.Valid || ep.ProjectedDCPlanID.Int64 != ep.DCPlanID.Int64 {
		return hilbertEpisodeUploadContext{}, newNonRetryableSyncError("dc_plan %d not found or deleted", ep.DCPlanID.Int64)
	}
	if !ep.WorkspaceID.Valid || ep.WorkspaceID.Int64 <= 0 {
		workspaceID := int64(0)
		if ep.WorkspaceID.Valid {
			workspaceID = ep.WorkspaceID.Int64
		}
		return hilbertEpisodeUploadContext{}, newNonRetryableSyncError(
			"dc_plan %d has invalid workspace_id %d",
			ep.DCPlanID.Int64,
			workspaceID,
		)
	}
	return hilbertEpisodeUploadContext{DCPlanID: ep.DCPlanID.Int64, WorkspaceID: ep.WorkspaceID.Int64}, nil
}

// SyncProgressSnapshot is the latest in-memory progress for an active episode sync.
type SyncProgressSnapshot struct {
	UploadedBytes int64
	TotalBytes    int64
	UpdatedAt     time.Time
}

// SyncWorker is a background goroutine that processes queued cloud sync work
// and optionally discovers approved episodes for automatic cloud upload.
type SyncWorker struct {
	db          *sqlx.DB
	uploader    *cloud.Uploader
	minioClient *s3.Client
	minioBucket string
	hilbert     hilbertRawDataClient
	tosUploader tosObjectUploader
	source      SourceObjectReader
	cfg         SyncWorkerConfig
	syncCfg     *config.SyncConfig

	mu              sync.Mutex
	enqueuedEpisode map[int64]struct{}
	stopDone        chan struct{}

	progressMu        sync.RWMutex
	progressByEpisode map[int64]SyncProgressSnapshot

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
	errSyncNonRetryableFailed = errors.New("sync latest failure is non-retryable")

	hilbertRawDataIDDestinationPrefix = "hilbert:raw_data_id:"
)

// NewSyncWorker creates a new sync worker. Call Start() to begin background processing.
func NewSyncWorker(db *sqlx.DB, uploader *cloud.Uploader, minioClient *s3.Client, minioBucket string, cfg SyncWorkerConfig, syncCfg *config.SyncConfig) *SyncWorker {
	return &SyncWorker{
		db:                db,
		uploader:          uploader,
		minioClient:       minioClient,
		minioBucket:       minioBucket,
		cfg:               cfg,
		syncCfg:           syncCfg,
		enqueueCh:         make(chan syncEnqueueRequest, 100),
		enqueuedEpisode:   make(map[int64]struct{}),
		progressByEpisode: make(map[int64]SyncProgressSnapshot),
	}
}

// SetHilbertRawDataClient configures the Hilbert raw-data control plane client.
func (w *SyncWorker) SetHilbertRawDataClient(client hilbertRawDataClient) {
	if w == nil {
		return
	}
	w.hilbert = client
}

// SetTOSObjectUploader configures the object-storage uploader used by Hilbert raw-data sync.
func (w *SyncWorker) SetTOSObjectUploader(uploader tosObjectUploader) {
	if w == nil {
		return
	}
	w.tosUploader = uploader
}

// SetSourceObjectReader configures how the worker reads source MCAP objects.
func (w *SyncWorker) SetSourceObjectReader(reader SourceObjectReader) {
	if w == nil {
		return
	}
	w.source = reader
}

func (w *SyncWorker) sourceReader() SourceObjectReader {
	if w == nil {
		return nil
	}
	if w.source != nil {
		return w.source
	}
	if w.minioClient != nil {
		return minioSourceObjectReader{client: w.minioClient}
	}
	return nil
}

func (w *SyncWorker) setEpisodeProgress(episodeID int64, uploadedBytes int64, totalBytes int64) {
	if w == nil {
		return
	}
	if uploadedBytes < 0 {
		uploadedBytes = 0
	}
	if totalBytes < 0 {
		totalBytes = 0
	}
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	if w.progressByEpisode == nil {
		w.progressByEpisode = make(map[int64]SyncProgressSnapshot)
	}
	w.progressByEpisode[episodeID] = SyncProgressSnapshot{
		UploadedBytes: uploadedBytes,
		TotalBytes:    totalBytes,
		UpdatedAt:     time.Now().UTC(),
	}
}

func (w *SyncWorker) finishEpisodeProgress(episodeID int64) {
	if w == nil {
		return
	}
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	delete(w.progressByEpisode, episodeID)
}

// GetEpisodeProgress returns the current in-memory upload progress for an episode.
func (w *SyncWorker) GetEpisodeProgress(episodeID int64) (SyncProgressSnapshot, bool) {
	if w == nil {
		return SyncProgressSnapshot{}, false
	}
	w.progressMu.RLock()
	defer w.progressMu.RUnlock()
	progress, ok := w.progressByEpisode[episodeID]
	return progress, ok
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
	startedAt := time.Now()
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
			return syncWorkerStopTimeoutError(ctx, startedAt)
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
			return syncWorkerStopTimeoutError(ctx, startedAt)
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
		return syncWorkerStopTimeoutError(ctx, startedAt)
	}
}

func syncWorkerStopTimeoutError(ctx context.Context, startedAt time.Time) error {
	timeout := time.Since(startedAt).Round(time.Millisecond)
	if deadline, ok := ctx.Deadline(); ok {
		if d := deadline.Sub(startedAt); d > 0 {
			timeout = d.Round(time.Millisecond)
		}
	}
	logger.Printf("[SYNC-WORKER] Stop timeout after %s (timeout_ms=%d): %v", timeout, timeout.Milliseconds(), ctx.Err())
	return fmt.Errorf("sync worker stop timeout after %s: %w", timeout, ctx.Err())
}

// IsRunning returns whether the worker is currently running.
func (w *SyncWorker) IsRunning() bool {
	return w.running.Load()
}

// MaxRetries returns the configured automatic retry limit.
func (w *SyncWorker) MaxRetries() int {
	return w.cfg.MaxRetries
}

// AutoScanEnabled returns whether the worker periodically discovers newly eligible episodes.
func (w *SyncWorker) AutoScanEnabled() bool {
	return w.cfg.AutoScanEnabled
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
	if err := w.persistPendingSyncLog(ctx, episodeID, true, false); err != nil {
		return err
	}
	w.enqueuePersistedEpisode(ctx, syncEnqueueRequest{episodeID: episodeID, manual: true})
	return nil
}

// EnqueueEpisodeResync queues a new upload attempt for an episode that has already synced.
func (w *SyncWorker) EnqueueEpisodeResync(ctx context.Context, episodeID int64) error {
	if !w.running.Load() {
		return ErrSyncWorkerNotRunning
	}
	if err := w.persistResyncSyncLog(ctx, episodeID); err != nil {
		return err
	}
	w.enqueuePersistedEpisode(ctx, syncEnqueueRequest{episodeID: episodeID, manual: true, resync: true})
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

func (w *SyncWorker) persistPendingSyncLog(ctx context.Context, episodeID int64, manual bool, allowSynced bool) error {
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
		ID          int64  `db:"id"`
		CloudSynced bool   `db:"cloud_synced"`
		QaStatus    string `db:"qa_status"`
	}
	if err := tx.GetContext(ctx, &episode, `
		SELECT id, cloud_synced, COALESCE(qa_status, '') AS qa_status
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("episode %d not found", episodeID)
		}
		return fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	if episode.CloudSynced && !allowSynced {
		return fmt.Errorf("episode %d already synced", episodeID)
	}
	if episode.QaStatus != "approved" {
		return fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, episode.QaStatus)
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
		retryDue := latest.NextRetry.Valid && !latest.NextRetry.Time.After(now)
		if latest.AttemptCount < w.cfg.MaxRetries && retryDue {
			if err := promoteFailedSyncLogToPending(ctx, tx, latest.ID, now); err != nil {
				return err
			}
			return tx.Commit()
		}
		if !manual && !latest.NextRetry.Valid {
			return fmt.Errorf("%w for episode %d", errSyncNonRetryableFailed, episodeID)
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

func (w *SyncWorker) persistResyncSyncLog(ctx context.Context, episodeID int64) error {
	if w.db == nil {
		return nil
	}

	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resync sync_log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)
	var episode struct {
		ID          int64  `db:"id"`
		CloudSynced bool   `db:"cloud_synced"`
		QaStatus    string `db:"qa_status"`
	}
	if err := tx.GetContext(ctx, &episode, `
		SELECT id, cloud_synced, COALESCE(qa_status, '') AS qa_status
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("episode %d not found", episodeID)
		}
		return fmt.Errorf("lock episode %d for resync: %w", episodeID, err)
	}
	if !episode.CloudSynced {
		return fmt.Errorf("episode %d has not completed cloud sync", episodeID)
	}
	if episode.QaStatus != "approved" {
		return fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, episode.QaStatus)
	}

	var activeCount int
	if err := tx.GetContext(ctx, &activeCount, `
		SELECT COUNT(*)
		FROM sync_logs
		WHERE episode_id = ?
		  AND status IN ('pending', 'in_progress')
	`, episodeID); err != nil {
		return fmt.Errorf("query active resync sync_log count: %w", err)
	}
	if activeCount > 0 {
		return fmt.Errorf("%w for episode %d", ErrSyncAlreadyInProgress, episodeID)
	}

	if err := insertPendingSyncLog(ctx, tx, episodeID, time.Now().UTC(), 0); err != nil {
		return err
	}
	return tx.Commit()
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
		errors.Is(err, errSyncAlreadyCompleted) ||
		errors.Is(err, errSyncNonRetryableFailed)
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
		if err := w.persistPendingSyncLog(ctx, id, false, false); err != nil {
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

func (w *SyncWorker) processEnqueuedEpisodeWith(ctx context.Context, req syncEnqueueRequest, process func(context.Context, int64, bool, bool)) {
	defer w.unmarkEnqueued(req.episodeID)
	process(ctx, req.episodeID, req.manual, req.resync)
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

	if !w.cfg.AutoScanEnabled {
		return
	}

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
		if err := w.persistPendingSyncLog(ctx, id, false, false); err != nil {
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
	reqs, err := w.findPendingSyncLogEpisodes(ctx)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to find queued sync logs: %v", err)
		return
	}
	for _, req := range reqs {
		w.dispatchPersistedJob(ctx, req)
	}
}

func (w *SyncWorker) dispatchPersistedJob(ctx context.Context, req syncEnqueueRequest) {
	if !w.tryMarkEnqueued(req.episodeID) {
		return
	}
	w.dispatchJob(ctx, req)
}

func (w *SyncWorker) findPendingSyncLogEpisodes(ctx context.Context) ([]syncEnqueueRequest, error) {
	var rows []struct {
		EpisodeID   int64 `db:"episode_id"`
		CloudSynced bool  `db:"cloud_synced"`
	}
	if err := w.db.SelectContext(ctx, &rows, `
		SELECT latest_log.episode_id, e.cloud_synced
		FROM sync_logs latest_log
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) latest ON latest_log.episode_id = latest.episode_id AND latest_log.id = latest.latest_id
		INNER JOIN episodes e ON e.id = latest_log.episode_id
		WHERE latest_log.status = 'pending'
		  AND e.deleted_at IS NULL
		ORDER BY latest_log.started_at ASC, latest_log.id ASC
		LIMIT ?
	`, w.cfg.BatchSize); err != nil {
		return nil, fmt.Errorf("query pending sync logs: %w", err)
	}
	reqs := make([]syncEnqueueRequest, len(rows))
	for i, row := range rows {
		reqs[i] = syncEnqueueRequest{episodeID: row.EpisodeID, resync: row.CloudSynced}
	}
	return reqs, nil
}

func (w *SyncWorker) findPendingEpisodes(ctx context.Context, includeExhaustedFailures bool) ([]int64, error) {
	var ids []int64
	var err error
	query := `
		SELECT e.id
		FROM episodes e
		WHERE e.qa_status = 'approved'
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
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl
		    INNER JOIN (
		      SELECT episode_id, MAX(id) AS latest_id
		      FROM sync_logs
		      GROUP BY episode_id
		    ) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		    WHERE sl.episode_id = e.id
		      AND sl.status = 'failed'
		      AND sl.next_retry_at IS NULL
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
	var rows []struct {
		EpisodeID   int64 `db:"episode_id"`
		CloudSynced bool  `db:"cloud_synced"`
	}
	now := time.Now().UTC()
	err := w.db.SelectContext(ctx, &rows, `
		SELECT sl.episode_id, e.cloud_synced
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		INNER JOIN episodes e ON e.id = sl.episode_id
		WHERE sl.status = 'failed'
		  AND e.deleted_at IS NULL
		  AND sl.attempt_count < ?
		  AND sl.next_retry_at IS NOT NULL
		  AND sl.next_retry_at <= ?
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

	if len(rows) == 0 {
		return
	}

	for _, row := range rows {
		if err := w.persistPendingSyncLog(ctx, row.EpisodeID, false, row.CloudSynced); err != nil {
			if isSkippablePendingError(err) {
				continue
			}
			logger.Printf("[SYNC-WORKER] Failed to queue retry for episode %d: %v", row.EpisodeID, err)
			continue
		}
		w.dispatchPersistedJob(ctx, syncEnqueueRequest{episodeID: row.EpisodeID, manual: false, resync: row.CloudSynced})
	}
}

func (w *SyncWorker) processEpisodeWithMode(ctx context.Context, episodeID int64, manual bool, resync bool) {
	var ep syncEpisodeUploadRow
	err := w.db.GetContext(ctx, &ep, `
		SELECT
			e.id,
			e.episode_id,
			e.dc_plan_id,
			e.local_dc_plan_id,
			dp.id AS projected_dc_plan_id,
			dp.workspace_id,
			dp.name AS dc_plan_name,
			dp.dc_type,
			e.mcap_path,
			e.sidecar_path,
			e.cloud_synced,
			e.metadata,
			e.workstation_id,
			e.duration_sec,
			e.created_at,
			COALESCE(NULLIF(dc.operator_id, ''), NULLIF(ws.collector_operator_id, '')) AS data_collector_operator_id,
			COALESCE(NULLIF(dc.name, ''), NULLIF(ws.collector_name, '')) AS data_collector_name
		FROM episodes e
		LEFT JOIN dc_plan dp ON dp.id = e.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL
	`, episodeID)
	if err == sql.ErrNoRows {
		logger.Printf("[SYNC-WORKER] Episode %d not found, skipping", episodeID)
		return
	}
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to query episode %d: %v", episodeID, err)
		return
	}

	if ep.CloudSynced && !resync {
		//logger.Printf("[SYNC-WORKER] Episode %d already synced, skipping", episodeID)
		return
	}

	syncLogID, attemptCount, err := w.acquireSyncLogWithMode(ctx, episodeID, ep.McapPath, manual)
	if err != nil {
		//logger.Printf("[SYNC-WORKER] Failed to acquire sync log for episode %d: %v", episodeID, err)
		return
	}

	startTime := time.Now()

	result, err := w.uploadEpisodeDirect(ctx, syncLogID, ep)
	if err != nil {
		duration := int64(time.Since(startTime).Seconds())
		w.markSyncFailed(ctx, syncLogID, episodeID, duration, err, attemptCount)
		w.finishEpisodeProgress(episodeID)
		return
	}

	// Success: update episode and sync_log
	duration := int64(time.Since(startTime).Seconds())
	w.markSyncCompleted(ctx, syncLogID, episodeID, result, duration)
	w.finishEpisodeProgress(episodeID)
}

func (w *SyncWorker) uploadEpisodeDirect(ctx context.Context, syncLogID int64, ep syncEpisodeUploadRow) (*cloud.UploadResult, error) {
	uploadContext, err := hilbertUploadContext(ep)
	if err != nil {
		return nil, err
	}
	if w.hilbert == nil {
		return nil, newNonRetryableSyncError("Hilbert raw-data client is not configured")
	}
	source := w.sourceReader()
	if source == nil {
		return nil, fmt.Errorf("source object reader not available")
	}

	mcapKey := objectKeyFromStoredPath(ep.McapPath, w.minioBucket)
	if mcapKey == "" {
		return nil, newNonRetryableSyncError("episode %d has empty mcap_path", ep.ID)
	}

	objectSize, err := source.StatObject(ctx, w.minioBucket, mcapKey)
	if err != nil {
		return nil, fmt.Errorf("stat mcap object %s: %w", mcapKey, err)
	}
	if objectSize <= 0 {
		return nil, newNonRetryableSyncError("episode %d has zero-byte mcap object %s", ep.ID, mcapKey)
	}
	mcapMD5Hex, err := episodeMCAPMD5Hex(ctx, ep, source, w.minioBucket, mcapKey)
	if err != nil {
		return nil, fmt.Errorf("resolve mcap md5 %s: %w", mcapKey, err)
	}

	rawDataID, err := w.hilbertRawDataIDFromSyncLog(ctx, syncLogID)
	if err != nil {
		return nil, err
	}
	if rawDataID > 0 {
		logger.Printf("[SYNC-WORKER] Episode %d reusing Hilbert raw-data registration: raw_data_id=%d sync_log_id=%d",
			ep.ID, rawDataID, syncLogID)
	} else {
		rawDataID, err = w.hilbert.RegisterRawData(ctx, auth.HilbertRawDataRegisterRequest{
			WorkspaceID:  uploadContext.WorkspaceID,
			DCPlanID:     uploadContext.DCPlanID,
			BagName:      hilbertBagName(ep, mcapKey),
			BagStartTime: ep.bagStartTime(),
			BagEndTime:   ep.bagEndTime(),
			BagSize:      objectSize,
			BagDigest:    mcapMD5Hex,
		})
		if err != nil {
			return nil, fmt.Errorf("register Hilbert raw data: %w", err)
		}
		if err := w.persistHilbertRawDataID(ctx, syncLogID, rawDataID); err != nil {
			return nil, err
		}
		logger.Printf("[SYNC-WORKER] Episode %d Hilbert raw-data registered: raw_data_id=%d workspace_id=%d dc_plan_id=%d size=%d object_key=%s",
			ep.ID, rawDataID, uploadContext.WorkspaceID, uploadContext.DCPlanID, objectSize, mcapKey)
	}
	uploadCredentials, err := w.hilbert.GetRawDataUploadCredentials(ctx, uploadContext.WorkspaceID, rawDataID)
	if err != nil {
		return nil, fmt.Errorf("get Hilbert raw-data upload credentials: %w", err)
	}
	if !strings.EqualFold(uploadCredentials.Provider, "TOS") {
		return nil, fmt.Errorf("unsupported Hilbert raw-data provider %q", uploadCredentials.Provider)
	}

	obj, err := source.OpenObject(ctx, w.minioBucket, mcapKey)
	if err != nil {
		return nil, fmt.Errorf("get mcap object %s: %w", mcapKey, err)
	}
	defer func() {
		_ = obj.Close()
	}()

	tosUploader := w.tosUploader
	if tosUploader == nil {
		tosUploader = cloud.NewTOSS3Uploader(w.syncOSSTimeout())
	}
	logger.Printf("[SYNC-WORKER] Episode %d Hilbert raw-data upload start: raw_data_id=%d endpoint=%s bucket=%s object_key=%s size=%d",
		ep.ID, rawDataID, uploadCredentials.Endpoint, uploadCredentials.Bucket, uploadCredentials.Key, objectSize)
	objectETag, err := tosUploader.PutObject(ctx, hilbertUploadTarget(uploadCredentials), obj, objectSize, func(uploadedBytes int64, totalBytes int64) {
		w.setEpisodeProgress(ep.ID, uploadedBytes, totalBytes)
	})
	if err != nil {
		return nil, fmt.Errorf("upload Hilbert raw-data object: %w", err)
	}
	if err := w.hilbert.FinishRawDataUpload(ctx, uploadContext.WorkspaceID, rawDataID); err != nil {
		return nil, fmt.Errorf("finish Hilbert raw-data upload: %w", err)
	}

	rawID := strconv.FormatInt(rawDataID, 10)
	logger.Printf("[SYNC-WORKER] Episode %d Hilbert raw-data upload complete: raw_data_id=%s object_key=%s size=%d",
		ep.ID, rawID, uploadCredentials.Key, objectSize)
	return &cloud.UploadResult{
		LogicalUploadID: rawID,
		UploadID:        rawID,
		Bucket:          uploadCredentials.Bucket,
		ObjectKey:       uploadCredentials.Key,
		FileSize:        objectSize,
		OSSObjectETag:   objectETag,
	}, nil
}

//nolint:unused // Reserved for direct DP upload mode.
func directCloudUploadRequest(
	ep syncEpisodeUploadRow,
	mcapKey string,
	assetID string,
	rawTags map[string]string,
	uploadContext hilbertEpisodeUploadContext,
	progress cloud.UploadProgressFunc,
) cloud.UploadRequest {
	return cloud.UploadRequest{
		EpisodeID:   ep.EpisodeUUID,
		McapKey:     mcapKey,
		AssetID:     assetID,
		RawTags:     rawTags,
		ClientHints: uploadContext.clientHints(),
		Progress:    progress,
	}
}

func (ep syncEpisodeUploadRow) bagStartTime() time.Time {
	if !ep.CreatedAt.IsZero() {
		return ep.CreatedAt.UTC()
	}
	return time.Now().UTC()
}

func (ep syncEpisodeUploadRow) bagEndTime() time.Time {
	start := ep.bagStartTime()
	if ep.DurationSec.Valid && ep.DurationSec.Float64 > 0 {
		return start.Add(time.Duration(ep.DurationSec.Float64 * float64(time.Second))).UTC()
	}
	return start.Add(time.Second).UTC()
}

func hilbertBagName(ep syncEpisodeUploadRow, mcapKey string) string {
	key := strings.Trim(strings.TrimSpace(mcapKey), "/")
	ext := path.Ext(key)
	stem := sanitizeHilbertBagNamePart(ep.EpisodeUUID)
	if stem == "" {
		stem = "episode_" + strconv.FormatInt(ep.ID, 10)
	}
	if ext == "" {
		ext = ".mcap"
	}
	name := stem + ext
	if len(name) <= 180 {
		return name
	}
	return name[:180-len(ext)] + ext
}

func sanitizeHilbertBagNamePart(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_'
		if valid {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func (w *SyncWorker) hilbertRawDataIDFromSyncLog(ctx context.Context, syncLogID int64) (int64, error) {
	if w == nil || w.db == nil || syncLogID <= 0 {
		return 0, nil
	}
	var destination sql.NullString
	if err := w.db.GetContext(ctx, &destination, "SELECT destination_path FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("load Hilbert raw-data id from sync_log %d: %w", syncLogID, err)
	}
	value := strings.TrimSpace(destination.String)
	if !destination.Valid || !strings.HasPrefix(value, hilbertRawDataIDDestinationPrefix) {
		return 0, nil
	}
	rawID, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(value, hilbertRawDataIDDestinationPrefix)), 10, 64)
	if err != nil || rawID <= 0 {
		return 0, fmt.Errorf("invalid Hilbert raw-data id in sync_log %d: %q", syncLogID, value)
	}
	return rawID, nil
}

func (w *SyncWorker) persistHilbertRawDataID(ctx context.Context, syncLogID int64, rawDataID int64) error {
	if w == nil || w.db == nil || syncLogID <= 0 || rawDataID <= 0 {
		return nil
	}
	value := hilbertRawDataIDDestinationPrefix + strconv.FormatInt(rawDataID, 10)
	if _, err := w.db.ExecContext(ctx, "UPDATE sync_logs SET destination_path = ? WHERE id = ?", value, syncLogID); err != nil {
		return fmt.Errorf("persist Hilbert raw-data id %d to sync_log %d: %w", rawDataID, syncLogID, err)
	}
	return nil
}

func objectMD5Hex(ctx context.Context, source SourceObjectReader, bucket, key string) (string, error) {
	if source == nil {
		return "", fmt.Errorf("source object reader not available")
	}
	obj, err := source.OpenObject(ctx, bucket, key)
	if err != nil {
		return "", fmt.Errorf("open object %s: %w", key, err)
	}
	defer func() {
		_ = obj.Close()
	}()
	hash := md5.New() // #nosec G401 -- Hilbert raw-data API requires MD5.
	if _, err := io.Copy(hash, obj); err != nil {
		return "", fmt.Errorf("read object %s: %w", key, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func episodeMCAPMD5Hex(
	ctx context.Context,
	ep syncEpisodeUploadRow,
	source SourceObjectReader,
	bucket string,
	key string,
) (string, error) {
	if ep.Metadata.Valid {
		var metadata struct {
			Source      string `json:"source"`
			Product     string `json:"product"`
			ChecksumMD5 string `json:"checksum_md5"`
			ClientHints struct {
				Product string `json:"product"`
			} `json:"client_hints"`
		}
		if err := json.Unmarshal([]byte(ep.Metadata.String), &metadata); err == nil &&
			metadata.Source == "dgwcompat" &&
			(metadata.Product == "ego_portal_lite" || metadata.ClientHints.Product == "ego_portal_lite") {
			checksumMD5 := strings.ToLower(strings.TrimSpace(metadata.ChecksumMD5))
			if !isMD5HexDigest(checksumMD5) {
				return "", newNonRetryableSyncError("episode %d missing valid dgwcompat checksum_md5", ep.ID)
			}
			return checksumMD5, nil
		}
	}
	return objectMD5Hex(ctx, source, bucket, key)
}

func isMD5HexDigest(value string) bool {
	if len(value) != md5.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hilbertUploadTarget(credentials *auth.HilbertRawDataUploadCredentials) cloud.TOSS3UploadTarget {
	if credentials == nil {
		return cloud.TOSS3UploadTarget{}
	}
	return cloud.TOSS3UploadTarget{
		Endpoint:        credentials.Endpoint,
		Region:          credentials.Region,
		Bucket:          credentials.Bucket,
		Key:             credentials.Key,
		AccessKeyID:     credentials.Credentials.AccessKeyID,
		SecretAccessKey: credentials.Credentials.SecretAccessKey,
		TemporaryToken:  credentials.Credentials.TemporaryToken,
	}
}

func (w *SyncWorker) newDirectUploader(dpConfig *DPDeviceUploadConfig) (*cloud.Uploader, func(), error) { //nolint:unused // Reserved for direct DP upload mode.
	if dpConfig == nil {
		return nil, func() {}, fmt.Errorf("missing DP upload config")
	}
	authClient := cloud.NewAuthClient(cloud.AuthClientConfig{
		Endpoint:      dpConfig.Auth.Target,
		UseTLS:        dpConfig.Auth.UseTLS,
		TLSServerName: dpConfig.Auth.ServerName,
		APIKey:        dpConfig.Profile.APIKey,
		RefreshBefore: 60 * time.Second,
	})
	gatewayClient := cloud.NewGatewayClient(cloud.GatewayClientConfig{
		Endpoint:       dpConfig.Gateway.Target,
		UseTLS:         dpConfig.Gateway.UseTLS,
		TLSServerName:  dpConfig.Gateway.ServerName,
		RequestTimeout: w.syncRequestTimeout(),
	}, authClient)
	cleanup := func() {
		if err := gatewayClient.Close(); err != nil {
			logger.Printf("[SYNC-WORKER] Failed to close direct gateway client: %v", err)
		}
		if err := authClient.Close(); err != nil {
			logger.Printf("[SYNC-WORKER] Failed to close direct auth client: %v", err)
		}
	}

	uploader, err := cloud.NewUploader(gatewayClient, w.minioClient, w.minioBucket, cloud.UploaderConfig{
		RequestTimeout:  w.syncRequestTimeout(),
		OSSTimeout:      w.syncOSSTimeout(),
		PersistRootDir:  w.syncPersistRootDir(),
		MaxRestartCount: uint32(w.syncMaxRestartCount()), //nolint:gosec // non-negative by helper
	})
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return uploader, cleanup, nil
}

func (w *SyncWorker) syncRequestTimeout() time.Duration { //nolint:unused // Reserved for direct DP upload mode.
	if w.syncCfg != nil && w.syncCfg.RequestTimeoutSec > 0 {
		return time.Duration(w.syncCfg.RequestTimeoutSec) * time.Second
	}
	return 30 * time.Second
}

func (w *SyncWorker) syncOSSTimeout() time.Duration {
	if w.syncCfg != nil && w.syncCfg.OSSTimeoutSec > 0 {
		return time.Duration(w.syncCfg.OSSTimeoutSec) * time.Second
	}
	return 300 * time.Second
}

func (w *SyncWorker) syncPersistRootDir() string { //nolint:unused // Reserved for direct DP upload mode.
	if w.syncCfg == nil {
		return ""
	}
	return w.syncCfg.PersistRootDir
}

func (w *SyncWorker) syncMaxRestartCount() int { //nolint:unused // Reserved for direct DP upload mode.
	if w.syncCfg != nil && w.syncCfg.MaxRestartCount >= 0 {
		return w.syncCfg.MaxRestartCount
	}
	return 3
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
			retryDue := latest.NextRetry.Valid && !latest.NextRetry.Time.After(now)
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

			if !manual && !latest.NextRetry.Valid {
				return 0, 0, fmt.Errorf("%w for episode %d", errSyncNonRetryableFailed, episodeID)
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

	var nextRetry sql.NullTime
	if !isNonRetryableSyncError(uploadErr) {
		backoff := w.nextRetryDelay(attemptCount)
		nextRetry = sql.NullTime{Time: now.Add(backoff), Valid: true}
	}

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

	if nextRetry.Valid {
		logger.Printf("[SYNC-WORKER] Episode %d sync failed: %v (attempt=%d, next_retry=%v)",
			episodeID, uploadErr, attemptCount, nextRetry.Time.Format(time.RFC3339))
		return
	}
	logger.Printf("[SYNC-WORKER] Episode %d sync failed non-retryable: %v (attempt=%d)",
		episodeID, uploadErr, attemptCount)
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

func (w *SyncWorker) directTagsFromSidecar(ctx context.Context, sidecarPath string) (map[string]string, error) { //nolint:unused // Reserved for direct DP upload mode.
	key := objectKeyFromStoredPath(sidecarPath, w.minioBucket)
	if key == "" {
		return map[string]string{}, nil
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
		return nil, wrapNonRetryableSyncError(err, "flatten sidecar %s", key)
	}
	return tags, nil
}

// objectKeyFromStoredPath removes the leading "bucket/" only when the stored
// path explicitly includes this worker's bucket name. TOS object keys such as
// "device-uploads/..." must keep their first path segment.
func objectKeyFromStoredPath(storedPath, bucket string) string {
	key := strings.TrimPrefix(strings.TrimSpace(storedPath), "/")
	bucket = strings.Trim(strings.TrimSpace(bucket), "/")
	if key == "" || bucket == "" {
		return key
	}
	if key == bucket {
		return ""
	}
	prefix := bucket + "/"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return key
}
