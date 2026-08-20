// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"crypto/sha256"
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
	"archebase.com/keystone-edge/internal/storage/objectrange"
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

type syncEpisodeUploadRow struct {
	ID                      int64               `db:"id"`
	EpisodeUUID             string              `db:"episode_id"`
	DCPlanID                sql.NullInt64       `db:"dc_plan_id"`
	LocalDCPlanID           sql.NullInt64       `db:"local_dc_plan_id"`
	ProjectedDCPlanID       sql.NullInt64       `db:"projected_dc_plan_id"`
	WorkspaceID             sql.NullInt64       `db:"workspace_id"`
	DCPlanName              sql.NullString      `db:"dc_plan_name"`
	DCType                  sql.NullString      `db:"dc_type"`
	McapPath                string              `db:"mcap_path"`
	StorageBackend          string              `db:"storage_backend"`
	SidecarPath             string              `db:"sidecar_path"`
	CloudSynced             bool                `db:"cloud_synced"`
	QAStatus                string              `db:"qa_status"`
	CloudPublishSource      sql.NullString      `db:"cloud_publish_source"`
	Checksum                sql.NullString      `db:"checksum"`
	FileSizeBytes           sql.NullInt64       `db:"file_size_bytes"`
	WorkstationID           sql.NullInt64       `db:"workstation_id"`
	DataCollectorOperatorID sql.NullString      `db:"data_collector_operator_id"`
	DataCollectorName       sql.NullString      `db:"data_collector_name"`
	DurationSec             sql.NullFloat64     `db:"duration_sec"`
	DeviceType              string              `db:"device_type"`
	Metadata                sql.NullString      `db:"metadata"`
	HilbertRawDataID        sql.NullInt64       `db:"hilbert_raw_data_id"`
	CreatedAt               time.Time           `db:"created_at"`
	SourceSnapshot          *SyncSourceSnapshot `db:"-"`
}

type hilbertRawDataClient interface {
	RegisterRawData(ctx context.Context, request auth.HilbertRawDataRegisterRequest) (int64, error)
	FindRawDataByBagName(ctx context.Context, workspaceID int64, bagName string) (*auth.HilbertRawData, error)
	GetRawDataUploadCredentials(ctx context.Context, workspaceID, rawDataID int64) (*auth.HilbertRawDataUploadCredentials, error)
	FinishRawDataUpload(ctx context.Context, workspaceID, rawDataID int64) error
}

type tosObjectUploader interface {
	PutObject(ctx context.Context, target cloud.TOSS3UploadTarget, reader io.Reader, size int64, payloadHash string, progress cloud.UploadProgressFunc) (string, error)
}

// SourceObjectReader reads source MCAP objects for Hilbert sync.
type SourceObjectReader interface {
	StatObject(ctx context.Context, bucket, objectName string) (size int64, etag string, err error)
	OpenObjectRange(ctx context.Context, bucket, objectName string, offset, length, totalSize int64, etag string) (io.ReadCloser, error)
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

func (r minioSourceObjectReader) StatObject(ctx context.Context, bucket, objectName string) (int64, string, error) {
	if r.client == nil {
		return 0, "", fmt.Errorf("minio client not available")
	}
	objInfo, err := r.client.StatObject(ctx, bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		return 0, "", err
	}
	etag := objectrange.NormalizeETag(objInfo.ETag)
	if etag == "" {
		return 0, "", fmt.Errorf("minio object metadata missing ETag")
	}
	return objInfo.Size, etag, nil
}

func (r minioSourceObjectReader) OpenObjectRange(ctx context.Context, bucket, objectName string, offset, length, totalSize int64, etag string) (io.ReadCloser, error) {
	if r.client == nil {
		return nil, fmt.Errorf("minio client not available")
	}
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("invalid minio object range offset=%d length=%d", offset, length)
	}
	end := offset + length - 1
	if end < offset {
		return nil, fmt.Errorf("minio object range overflows offset=%d length=%d", offset, length)
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, end); err != nil {
		return nil, fmt.Errorf("set minio object range offset=%d length=%d: %w", offset, length, err)
	}
	if err := opts.SetMatchETag(etag); err != nil {
		return nil, fmt.Errorf("set minio object ETag precondition: %w", err)
	}
	core := minio.Core{Client: r.client.Client}
	body, objectInfo, header, err := core.GetObject(ctx, bucket, objectName, opts)
	if err != nil {
		return nil, err
	}
	if err := objectrange.ValidateResponse(header, offset, length, totalSize, etag); err != nil {
		_ = body.Close()
		return nil, fmt.Errorf("validate minio object range: %w", err)
	}
	if objectInfo.Size != length {
		_ = body.Close()
		return nil, fmt.Errorf("minio object range size %d, want %d", objectInfo.Size, length)
	}
	return body, nil
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
	db               *sqlx.DB
	uploader         *cloud.Uploader
	minioClient      *s3.Client
	minioBucket      string
	hilbert          hilbertRawDataClient
	tosUploader      tosObjectUploader
	source           SourceObjectReader
	tosSource        SourceObjectReader
	tosBucket        string
	derivativeBucket string
	cfg              SyncWorkerConfig
	syncCfg          *config.SyncConfig

	// sourceObjectRangeSize is overridden only by small deterministic tests.
	// Production reads use defaultSourceObjectRangeSize.
	sourceObjectRangeSize int64

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
	// ErrEpisodeAlreadySynced is returned when an already-synced episode is submitted again.
	ErrEpisodeAlreadySynced = errors.New("episode already synced to cloud")
	// ErrSyncWorkerNotRunning is returned when Start has not been called or after Stop.
	ErrSyncWorkerNotRunning = errors.New("sync worker is not running")

	// DeviceTypeZJWA1D identifies the device family requiring depth normalization.
	DeviceTypeZJWA1D = "ZJ-WA1-D"

	errSyncRetryBackoffActive = errors.New("sync retry backoff active")
	errSyncRetryExhausted     = errors.New("sync retry max retries exceeded")
	errSyncAlreadyCompleted   = errors.New("sync already completed")
	errSyncNonRetryableFailed = errors.New("sync latest failure is non-retryable")
	errSyncCanceled           = errors.New("sync was canceled")

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
// It must be called before Start.
func (w *SyncWorker) SetSourceObjectReader(reader SourceObjectReader) {
	if w == nil {
		return
	}
	w.source = reader
}

// SetTOSSourceObjectReader configures how the worker reads DGW compatibility
// uploads stored in the configured TOS bucket. It must be called before Start.
func (w *SyncWorker) SetTOSSourceObjectReader(bucket string, reader SourceObjectReader) {
	if w == nil {
		return
	}
	w.tosBucket = strings.TrimSpace(bucket)
	w.tosSource = reader
}

// SetStereoSplitSourceBucket configures the TOS bucket containing verified
// stereo-split outputs. The same TOS reader is used for original and derived
// objects, but their buckets may differ.
func (w *SyncWorker) SetStereoSplitSourceBucket(bucket string) {
	if w == nil {
		return
	}
	w.derivativeBucket = strings.TrimSpace(bucket)
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

type episodeSourceMetadata struct {
	Source             string `json:"source"`
	ObjectStoreBackend string `json:"object_store_backend"`
	Bucket             string `json:"bucket"`
	ObjectKey          string `json:"object_key"`
}

func (m episodeSourceMetadata) usesTOS(configuredBucket string) bool {
	if strings.EqualFold(strings.TrimSpace(m.Source), "dgwcompat") ||
		strings.EqualFold(strings.TrimSpace(m.ObjectStoreBackend), "volcengine_tos") {
		return true
	}
	bucket := strings.TrimSpace(m.Bucket)
	return bucket != "" && strings.TrimSpace(configuredBucket) != "" &&
		strings.EqualFold(bucket, strings.TrimSpace(configuredBucket))
}

func (w *SyncWorker) mcapSourceObject(ep syncEpisodeUploadRow) (SourceObjectReader, string, string, error) {
	if ep.SourceSnapshot != nil {
		return w.sourceForSnapshot(*ep.SourceSnapshot)
	}
	var metadata episodeSourceMetadata
	if ep.Metadata.Valid && json.Unmarshal([]byte(ep.Metadata.String), &metadata) == nil && metadata.usesTOS(w.tosBucket) {
		bucket := strings.TrimSpace(metadata.Bucket)
		if bucket == "" {
			bucket = strings.TrimSpace(w.tosBucket)
		}
		if configuredBucket := strings.TrimSpace(w.tosBucket); configuredBucket != "" &&
			!strings.EqualFold(bucket, configuredBucket) {
			return nil, "", "", newNonRetryableSyncError(
				"episode %d uses unconfigured TOS bucket %q", ep.ID, bucket,
			)
		}
		key := strings.TrimSpace(metadata.ObjectKey)
		if key == "" {
			key = objectKeyFromStoredPath(ep.McapPath, bucket)
		}
		if bucket == "" || key == "" {
			return nil, "", "", newNonRetryableSyncError("episode %d has invalid TOS object location", ep.ID)
		}
		if w.tosSource == nil {
			return nil, "", "", fmt.Errorf("TOS source object reader not available for episode %d", ep.ID)
		}
		return w.tosSource, bucket, key, nil
	}

	key := objectKeyFromStoredPath(ep.McapPath, w.minioBucket)
	if key == "" {
		return nil, "", "", newNonRetryableSyncError("episode %d has empty mcap_path", ep.ID)
	}
	reader := w.sourceReader()
	if reader == nil {
		return nil, "", "", fmt.Errorf("source object reader not available")
	}
	return reader, w.minioBucket, key, nil
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

// EnqueueEpisode adds a specific episode ID for immediate sync processing.
func (w *SyncWorker) EnqueueEpisode(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisode(ctx, episodeID, false)
}

// EnqueueEpisodeManual adds a specific episode ID for immediate sync processing.
// It selects an approved stereo-split derivative when available, otherwise the
// original Episode object, and preserves the claimed source for retries.
func (w *SyncWorker) EnqueueEpisodeManual(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisodeManual(ctx, episodeID, "", syncSourceAuto)
}

// EnqueueOriginalAutomatic persists an approved original Episode as the
// canonical cloud source and dispatches it through the existing worker pool.
func (w *SyncWorker) EnqueueOriginalAutomatic(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisodeManual(ctx, episodeID, "", SyncSourceOriginal)
}

// EnqueueStereoSplitManual claims the Episode's canonical cloud source as the
// approved stereo-split generation and queues that frozen object for upload.
func (w *SyncWorker) EnqueueStereoSplitManual(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisodeManual(ctx, episodeID, "", SyncSourceStereoSplit)
}

// EnqueueDepthNormalizationAutomatic claims the approved local depth-normalized
// MCAP generation as the Episode's canonical Hilbert upload source.
func (w *SyncWorker) EnqueueDepthNormalizationAutomatic(ctx context.Context, episodeID int64) error {
	return w.enqueueEpisodeManual(ctx, episodeID, "", SyncSourceDepthNormalization)
}

// EnqueueEpisodeManualForBulkRun persists an automatically sourced manual sync
// request with its originating bulk run.
func (w *SyncWorker) EnqueueEpisodeManualForBulkRun(ctx context.Context, episodeID int64, bulkRunID string) error {
	bulkRunID = strings.TrimSpace(bulkRunID)
	if bulkRunID == "" {
		return fmt.Errorf("bulk run ID is required")
	}
	return w.enqueueEpisodeManual(ctx, episodeID, bulkRunID, syncSourceAuto)
}

func (w *SyncWorker) enqueueEpisodeManual(ctx context.Context, episodeID int64, bulkRunID, sourceType string) error {
	if !w.running.Load() {
		return ErrSyncWorkerNotRunning
	}
	if err := w.persistPendingSyncLogForSource(ctx, episodeID, true, bulkRunID, sourceType); err != nil {
		return err
	}
	w.enqueuePersistedEpisode(ctx, syncEnqueueRequest{episodeID: episodeID, manual: true})
	return nil
}

// CancelBulkRun cancels durable sync work that has not started uploading yet.
func (w *SyncWorker) CancelBulkRun(ctx context.Context, bulkRunID string) (int64, error) {
	bulkRunID = strings.TrimSpace(bulkRunID)
	if bulkRunID == "" {
		return 0, fmt.Errorf("bulk run ID is required")
	}
	res, err := w.db.ExecContext(ctx, `
		UPDATE sync_logs
		SET status = 'canceled',
		    error_message = COALESCE(error_message, 'bulk run canceled before upload started'),
		    next_retry_at = NULL,
		    completed_at = COALESCE(completed_at, ?)
		WHERE bulk_run_id = ?
		  AND (
		    status = 'pending'
		    OR (status = 'failed' AND next_retry_at IS NOT NULL AND attempt_count < ?)
		  )
	`, time.Now().UTC(), bulkRunID, w.cfg.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("cancel bulk sync logs: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read canceled bulk sync log count: %w", err)
	}
	return count, nil
}

// EnqueueEpisodeResync rejects already-synced episodes because Hilbert does not
// issue upload credentials after a raw-data record reaches uploaded status.
func (w *SyncWorker) EnqueueEpisodeResync(_ context.Context, episodeID int64) error {
	return fmt.Errorf("%w: episode %d", ErrEpisodeAlreadySynced, episodeID)
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

func (w *SyncWorker) persistPendingSyncLog(ctx context.Context, episodeID int64, manual bool, bulkRunID string) error {
	return w.persistPendingSyncLogForSource(ctx, episodeID, manual, bulkRunID, SyncSourceOriginal)
}

func (w *SyncWorker) persistPendingSyncLogForSource(ctx context.Context, episodeID int64, manual bool, bulkRunID, sourceType string) error {
	if w.db == nil {
		return nil
	}
	if sourceType != syncSourceAuto && sourceType != SyncSourceOriginal && sourceType != SyncSourceStereoSplit &&
		sourceType != SyncSourceDepthNormalization {
		return fmt.Errorf("unsupported sync source type %q", sourceType)
	}
	automaticSource := sourceType == syncSourceAuto

	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pending sync_log transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)
	var episode syncEpisodeUploadRow
	if err := tx.GetContext(ctx, &episode, `
		SELECT e.id, e.episode_id, COALESCE(e.storage_backend, '') AS storage_backend,
		       COALESCE(e.mcap_path, '') AS mcap_path, e.checksum, e.file_size_bytes,
		       e.metadata, e.cloud_synced, COALESCE(e.qa_status, '') AS qa_status,
		       e.cloud_publish_source, e.created_at, e.duration_sec,
		       COALESCE(current_ws_robot.device_type, task_ws_robot.device_type, '') AS device_type
		FROM episodes e
		LEFT JOIN tasks t ON t.id=e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations current_ws ON current_ws.id=e.workstation_id AND current_ws.deleted_at IS NULL
		LEFT JOIN robots current_ws_robot ON current_ws_robot.id=current_ws.robot_id AND current_ws_robot.deleted_at IS NULL
		LEFT JOIN workstations task_ws ON task_ws.id=t.workstation_id AND task_ws.deleted_at IS NULL
		LEFT JOIN robots task_ws_robot ON task_ws_robot.id=task_ws.robot_id AND task_ws_robot.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("episode %d not found", episodeID)
		}
		return fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	if episode.CloudSynced {
		return fmt.Errorf("%w: episode %d", ErrEpisodeAlreadySynced, episodeID)
	}
	if automaticSource && episode.QAStatus != "approved" {
		return fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, episode.QAStatus)
	}

	var latest struct {
		ID             int64          `db:"id"`
		Status         string         `db:"status"`
		NextRetry      sql.NullTime   `db:"next_retry_at"`
		AttemptCount   int            `db:"attempt_count"`
		SourceSnapshot sql.NullString `db:"source_snapshot"`
	}
	err = tx.GetContext(ctx, &latest, `
		SELECT id, status, next_retry_at, attempt_count, source_snapshot
		FROM sync_logs
		WHERE episode_id = ?
		ORDER BY id DESC
		LIMIT 1
	`+lockClause, episodeID)
	var snapshot SyncSourceSnapshot
	if errors.Is(err, sql.ErrNoRows) {
		if sourceType == syncSourceAuto {
			sourceType, err = w.resolveManualSyncSourceTx(ctx, tx, episode)
			if err != nil {
				return err
			}
		}
		switch sourceType {
		case SyncSourceOriginal:
			if episode.QAStatus != "approved" {
				return fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, episode.QAStatus)
			}
			if !automaticSource {
				var derivativeConflict int
				if err := tx.GetContext(ctx, &derivativeConflict, `
					SELECT COUNT(*) FROM episode_derivatives
					WHERE episode_id = ? AND kind = 'stereo_split'
					  AND processing_status IN ('queued', 'submitting', 'pending', 'running', 'verifying', 'succeeded')
				`, episodeID); err != nil {
					return fmt.Errorf("check stereo split sync conflict: %w", err)
				}
				if derivativeConflict > 0 {
					return fmt.Errorf("%w: stereo split processing or output already exists", ErrCloudPublishSourceLocked)
				}
			}
			snapshot, err = w.buildOriginalSourceSnapshot(episode)
		case SyncSourceDepthNormalization:
			snapshot, err = w.buildDepthNormalizationSourceSnapshot(ctx, tx, episode)
		default:
			snapshot, err = w.buildStereoSplitSourceSnapshot(ctx, tx, episode)
		}
		if err != nil {
			return err
		}
		claimedSource := strings.TrimSpace(episode.CloudPublishSource.String)
		if claimedSource != "" && claimedSource != sourceType {
			return fmt.Errorf("%w: episode source is %q", ErrCloudPublishSourceLocked, claimedSource)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE episodes SET cloud_publish_source = ?,
			    cloud_publish_claimed_at = COALESCE(cloud_publish_claimed_at, ?)
			WHERE id = ? AND (cloud_publish_source IS NULL OR cloud_publish_source = ?)
		`, sourceType, time.Now().UTC(), episodeID, sourceType); err != nil {
			return fmt.Errorf("claim episode cloud publish source: %w", err)
		}
		encoded, err := encodeSyncSourceSnapshot(snapshot)
		if err != nil {
			return err
		}
		if err := insertPendingSyncLog(ctx, tx, episodeID, bulkRunID, time.Now().UTC(), 0, encoded, snapshot.ObjectKey); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("lock latest sync_log: %w", err)
	}
	if sourceType == syncSourceAuto {
		if latest.SourceSnapshot.Valid && strings.TrimSpace(latest.SourceSnapshot.String) != "" {
			existingSnapshot, decodeErr := decodeSyncSourceSnapshot(latest.SourceSnapshot.String)
			if decodeErr != nil {
				return newNonRetryableSyncError("latest sync_log %d has invalid source snapshot: %v", latest.ID, decodeErr)
			}
			sourceType = existingSnapshot.SourceType
		} else {
			sourceType = SyncSourceOriginal
		}
	}
	if !latest.SourceSnapshot.Valid || strings.TrimSpace(latest.SourceSnapshot.String) == "" {
		if sourceType != SyncSourceOriginal {
			return newNonRetryableSyncError("latest sync_log %d has no source snapshot", latest.ID)
		}
		snapshot, err = w.buildOriginalSourceSnapshot(episode)
		if err != nil {
			return err
		}
		encoded, encodeErr := encodeSyncSourceSnapshot(snapshot)
		if encodeErr != nil {
			return encodeErr
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sync_logs SET source_snapshot = ?, source_path = COALESCE(source_path, ?)
			WHERE id = ? AND source_snapshot IS NULL
		`, encoded, snapshot.ObjectKey, latest.ID); err != nil {
			return fmt.Errorf("backfill legacy sync source snapshot: %w", err)
		}
		latest.SourceSnapshot = sql.NullString{String: encoded, Valid: true}
		if !episode.CloudPublishSource.Valid || strings.TrimSpace(episode.CloudPublishSource.String) == "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE episodes SET cloud_publish_source = 'original',
				    cloud_publish_claimed_at = COALESCE(cloud_publish_claimed_at, ?)
				WHERE id = ? AND cloud_publish_source IS NULL
			`, time.Now().UTC(), episodeID); err != nil {
				return fmt.Errorf("claim legacy original sync source: %w", err)
			}
			episode.CloudPublishSource = sql.NullString{String: SyncSourceOriginal, Valid: true}
		}
	} else {
		snapshot, err = decodeSyncSourceSnapshot(latest.SourceSnapshot.String)
		if err != nil {
			return newNonRetryableSyncError("latest sync_log %d has invalid source snapshot: %v", latest.ID, err)
		}
	}
	if snapshot.SourceType != sourceType {
		return fmt.Errorf("%w: existing sync source is %q", ErrCloudPublishSourceLocked, snapshot.SourceType)
	}
	if err := w.validateSyncSourceGateTx(ctx, tx, episodeID, episode.QAStatus, strings.TrimSpace(episode.CloudPublishSource.String), snapshot); err != nil {
		return err
	}
	encodedSnapshot := latest.SourceSnapshot.String

	now := time.Now().UTC()
	switch latest.Status {
	case "pending", "in_progress":
		return fmt.Errorf("%w for episode %d", ErrSyncAlreadyInProgress, episodeID)
	case "completed":
		return fmt.Errorf("%w for episode %d", errSyncAlreadyCompleted, episodeID)
	case "failed":
		retryDue := latest.NextRetry.Valid && !latest.NextRetry.Time.After(now)
		if bulkRunID == "" && latest.AttemptCount < w.cfg.MaxRetries && retryDue {
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
		if err := insertPendingSyncLog(ctx, tx, episodeID, bulkRunID, now, 0, encodedSnapshot, snapshot.ObjectKey); err != nil {
			return err
		}
		return tx.Commit()
	case "canceled":
		if !manual {
			return fmt.Errorf("%w for episode %d", errSyncCanceled, episodeID)
		}
		if err := insertPendingSyncLog(ctx, tx, episodeID, bulkRunID, now, 0, encodedSnapshot, snapshot.ObjectKey); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("unknown sync status %q for episode %d", latest.Status, episodeID)
	}
}

func insertPendingSyncLog(ctx context.Context, tx *sqlx.Tx, episodeID int64, bulkRunID string, queuedAt time.Time, attemptCount int, sourceSnapshot, sourcePath string) error {
	var bulkRunValue interface{}
	if strings.TrimSpace(bulkRunID) != "" {
		bulkRunValue = strings.TrimSpace(bulkRunID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_logs (
			episode_id, bulk_run_id, source_path, source_snapshot,
			status, attempt_count, started_at
		) VALUES (?, ?, ?, ?, 'pending', ?, ?)
	`, episodeID, bulkRunValue, sourcePath, sourceSnapshot, attemptCount, queuedAt); err != nil {
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
		errors.Is(err, ErrEpisodeAlreadySynced) ||
		errors.Is(err, errSyncRetryBackoffActive) ||
		errors.Is(err, errSyncRetryExhausted) ||
		errors.Is(err, errSyncAlreadyCompleted) ||
		errors.Is(err, errSyncNonRetryableFailed) ||
		errors.Is(err, errSyncCanceled)
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
		if err := w.persistPendingSyncLog(ctx, id, false, ""); err != nil {
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
	w.processEnqueuedEpisodeWith(ctx, req, w.processEpisode)
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
		EpisodeID int64 `db:"episode_id"`
	}
	if err := w.db.SelectContext(ctx, &rows, `
		SELECT latest_log.episode_id
		FROM sync_logs latest_log
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) latest ON latest_log.episode_id = latest.episode_id AND latest_log.id = latest.latest_id
		INNER JOIN episodes e ON e.id = latest_log.episode_id
		WHERE latest_log.status = 'pending'
		  AND e.deleted_at IS NULL
		  AND e.cloud_synced = FALSE
		ORDER BY latest_log.started_at ASC, latest_log.id ASC
		LIMIT ?
	`, w.cfg.BatchSize); err != nil {
		return nil, fmt.Errorf("query pending sync logs: %w", err)
	}
	reqs := make([]syncEnqueueRequest, len(rows))
	for i, row := range rows {
		reqs[i] = syncEnqueueRequest{episodeID: row.EpisodeID}
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
		  AND (e.cloud_publish_source IS NULL OR e.cloud_publish_source = 'original')
		  AND NOT EXISTS (
		    SELECT 1 FROM episode_derivatives ed
		    WHERE ed.episode_id = e.id AND ed.kind = 'stereo_split'
		      AND ed.processing_status IN ('queued', 'submitting', 'pending', 'running', 'verifying', 'succeeded')
		  )
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
		  AND NOT EXISTS (
		    SELECT 1 FROM sync_logs sl
		    INNER JOIN (
		      SELECT episode_id, MAX(id) AS latest_id
		      FROM sync_logs
		      GROUP BY episode_id
		    ) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		    WHERE sl.episode_id = e.id
		      AND sl.status = 'canceled'
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
		EpisodeID int64 `db:"episode_id"`
	}
	now := time.Now().UTC()
	err := w.db.SelectContext(ctx, &rows, `
		SELECT sl.episode_id
		FROM sync_logs sl
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) t ON sl.episode_id = t.episode_id AND sl.id = t.latest_id
		INNER JOIN episodes e ON e.id = sl.episode_id
		WHERE sl.status = 'failed'
		  AND e.deleted_at IS NULL
		  AND e.cloud_synced = FALSE
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
		if err := w.persistPendingSyncLogForSource(ctx, row.EpisodeID, false, "", syncSourceAuto); err != nil {
			if isSkippablePendingError(err) {
				continue
			}
			logger.Printf("[SYNC-WORKER] Failed to queue retry for episode %d: %v", row.EpisodeID, err)
			continue
		}
		w.dispatchPersistedJob(ctx, syncEnqueueRequest{episodeID: row.EpisodeID, manual: false})
	}
}

func (w *SyncWorker) processEpisode(ctx context.Context, episodeID int64, manual bool) {
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
			e.storage_backend,
			e.sidecar_path,
			e.cloud_synced,
			COALESCE(e.qa_status, '') AS qa_status,
			e.cloud_publish_source,
			e.checksum,
			e.file_size_bytes,
			e.workstation_id,
			e.duration_sec,
			COALESCE(current_ws_robot.device_type, task_ws_robot.device_type, '') AS device_type,
			e.metadata,
			e.hilbert_raw_data_id,
			e.created_at,
			COALESCE(NULLIF(dc.operator_id, ''), NULLIF(ws.collector_operator_id, '')) AS data_collector_operator_id,
			COALESCE(NULLIF(dc.name, ''), NULLIF(ws.collector_name, '')) AS data_collector_name
		FROM episodes e
		LEFT JOIN dc_plan dp ON dp.id = e.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots current_ws_robot ON current_ws_robot.id=ws.robot_id AND current_ws_robot.deleted_at IS NULL
		LEFT JOIN robots task_ws_robot ON task_ws_robot.id=ws.robot_id AND task_ws_robot.deleted_at IS NULL
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

	if ep.CloudSynced {
		//logger.Printf("[SYNC-WORKER] Episode %d already synced, skipping", episodeID)
		return
	}

	syncLogID, attemptCount, err := w.acquireSyncLogWithMode(ctx, episodeID, ep.McapPath, manual)
	if err != nil {
		//logger.Printf("[SYNC-WORKER] Failed to acquire sync log for episode %d: %v", episodeID, err)
		return
	}
	snapshot, found, err := loadSyncSourceSnapshot(ctx, w.db, syncLogID)
	if err != nil {
		w.markSyncFailed(ctx, syncLogID, episodeID, 0, err, attemptCount)
		return
	}
	if found {
		ep.SourceSnapshot = &snapshot
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
	source, sourceBucket, mcapKey, err := w.mcapSourceObject(ep)
	if err != nil {
		return nil, err
	}

	objectSize, sourceETag, err := source.StatObject(ctx, sourceBucket, mcapKey)
	if err != nil {
		return nil, fmt.Errorf("stat mcap object %s: %w", mcapKey, err)
	}
	if objectSize <= 0 {
		return nil, newNonRetryableSyncError("episode %d has zero-byte mcap object %s", ep.ID, mcapKey)
	}
	if ep.SourceSnapshot != nil && ep.SourceSnapshot.SizeBytes != objectSize {
		return nil, newNonRetryableSyncError(
			"persisted source size changed for episode %d: got %d want %d",
			ep.ID, objectSize, ep.SourceSnapshot.SizeBytes,
		)
	}
	bagDigest := ""
	if ep.SourceSnapshot != nil {
		bagDigest = strings.ToLower(strings.TrimSpace(ep.SourceSnapshot.SHA256))
	}
	if bagDigest == "" {
		bagDigest, err = w.resolveEpisodeSHA256Hex(ctx, ep, source, sourceBucket, mcapKey, objectSize, sourceETag)
	} else if len(bagDigest) != 64 {
		err = newNonRetryableSyncError("persisted source snapshot has invalid SHA-256")
	}
	if err != nil {
		return nil, err
	}
	rawDataID := int64(0)
	if ep.HilbertRawDataID.Valid {
		if ep.HilbertRawDataID.Int64 <= 0 {
			return nil, newNonRetryableSyncError(
				"episode %d has invalid Hilbert raw-data id %d",
				ep.ID,
				ep.HilbertRawDataID.Int64,
			)
		}
		rawDataID = ep.HilbertRawDataID.Int64
	} else {
		rawDataID, err = w.resolveHilbertRawDataIDFromSyncLogs(ctx, syncLogID)
		if err != nil {
			return nil, err
		}
	}
	if rawDataID > 0 {
		if !ep.HilbertRawDataID.Valid {
			if err := w.persistEpisodeHilbertRawDataID(ctx, syncLogID, ep.ID, rawDataID); err != nil {
				return nil, err
			}
		}
		logger.Printf("[SYNC-WORKER] Episode %d reusing Hilbert raw-data registration: raw_data_id=%d sync_log_id=%d",
			ep.ID, rawDataID, syncLogID)
	} else {
		bagName := hilbertBagName(ep, mcapKey)
		if ep.SourceSnapshot != nil {
			bagName = ep.SourceSnapshot.BagName
		}
		registerRequest := auth.HilbertRawDataRegisterRequest{
			WorkspaceID:  uploadContext.WorkspaceID,
			DCPlanID:     uploadContext.DCPlanID,
			BagName:      bagName,
			BagStartTime: ep.bagStartTime(),
			BagEndTime:   ep.bagEndTime(),
			BagSize:      objectSize,
			BagDigest:    bagDigest,
		}
		rawDataID, err = w.registerOrRecoverHilbertRawData(ctx, registerRequest)
		if err != nil {
			return nil, err
		}
		logger.Printf("[SYNC-WORKER] Episode %d Hilbert raw-data registration resolved: raw_data_id=%d workspace_id=%d dc_plan_id=%d size=%d object_key=%s",
			ep.ID, rawDataID, uploadContext.WorkspaceID, uploadContext.DCPlanID, objectSize, mcapKey)
		if err := w.persistEpisodeHilbertRawDataID(ctx, syncLogID, ep.ID, rawDataID); err != nil {
			logger.Printf("[SYNC-WORKER] Episode %d failed to persist Hilbert raw-data registration: raw_data_id=%d sync_log_id=%d err=%v",
				ep.ID, rawDataID, syncLogID, err)
			return nil, err
		}
	}
	uploadCredentials, err := w.hilbert.GetRawDataUploadCredentials(ctx, uploadContext.WorkspaceID, rawDataID)
	if err != nil {
		return nil, fmt.Errorf("get Hilbert raw-data upload credentials: %w", err)
	}
	if !strings.EqualFold(uploadCredentials.Provider, "TOS") {
		return nil, fmt.Errorf("unsupported Hilbert raw-data provider %q", uploadCredentials.Provider)
	}

	obj, err := w.openSourceObjectRangeStream(ctx, source, sourceBucket, mcapKey, objectSize, sourceETag)
	if err != nil {
		return nil, fmt.Errorf("open ranged mcap object %s: %w", mcapKey, err)
	}
	defer func() {
		_ = obj.Close()
	}()

	tosUploader := w.tosUploader
	if tosUploader == nil {
		tosUploader = cloud.NewTOSS3Uploader(w.syncOSSTimeout(), config.ModeEdge)
	}
	logger.Printf("[SYNC-WORKER] Episode %d Hilbert raw-data upload start: raw_data_id=%d endpoint=%s bucket=%s object_key=%s size=%d",
		ep.ID, rawDataID, uploadCredentials.Endpoint, uploadCredentials.Bucket, uploadCredentials.Key, objectSize)
	objectETag, err := tosUploader.PutObject(ctx, hilbertUploadTarget(uploadCredentials), obj, objectSize, bagDigest, func(uploadedBytes int64, totalBytes int64) {
		w.setEpisodeProgress(ep.ID, uploadedBytes, totalBytes)
	})
	if err != nil {
		if errors.Is(err, cloud.ErrTOSPayloadChecksumMismatch) {
			return nil, wrapNonRetryableSyncError(err, "upload Hilbert raw-data object")
		}
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

func (w *SyncWorker) registerOrRecoverHilbertRawData(
	ctx context.Context,
	request auth.HilbertRawDataRegisterRequest,
) (int64, error) {
	rawDataID, registerErr := w.hilbert.RegisterRawData(ctx, request)
	if registerErr == nil {
		return rawDataID, nil
	}

	existing, lookupErr := w.hilbert.FindRawDataByBagName(ctx, request.WorkspaceID, request.BagName)
	if lookupErr != nil {
		return 0, fmt.Errorf("register Hilbert raw data: %v; recover by bag name: %w", registerErr, lookupErr)
	}
	if existing == nil {
		return 0, fmt.Errorf("register Hilbert raw data: %w", registerErr)
	}
	if mismatch := hilbertRawDataRegistrationMismatch(existing, request); mismatch != "" {
		return 0, newNonRetryableSyncError(
			"Hilbert raw data bagName %q conflicts on %s after registration error: %v",
			request.BagName,
			mismatch,
			registerErr,
		)
	}

	logger.Printf("[SYNC-WORKER] Recovered Hilbert raw-data registration after ambiguous response: raw_data_id=%d bag_name=%s",
		existing.ID, existing.BagName)
	return existing.ID, nil
}

func hilbertRawDataRegistrationMismatch(
	existing *auth.HilbertRawData,
	request auth.HilbertRawDataRegisterRequest,
) string {
	if existing == nil || existing.ID <= 0 {
		return "id"
	}
	if existing.WorkspaceID != request.WorkspaceID {
		return "workspace_id"
	}
	if existing.DCPlanID != request.DCPlanID {
		return "dc_plan_id"
	}
	if existing.BagName != request.BagName {
		return "bag_name"
	}
	if !hilbertRegistrationTimeEqual(existing.BagStartTime, request.BagStartTime) {
		return "bag_start_time"
	}
	if !hilbertRegistrationTimeEqual(existing.BagEndTime, request.BagEndTime) {
		return "bag_end_time"
	}
	if existing.BagSize != request.BagSize {
		return "bag_size"
	}
	if !strings.EqualFold(strings.TrimSpace(existing.BagDigest), strings.TrimSpace(request.BagDigest)) {
		return "bag_digest"
	}
	return ""
}

func hilbertRegistrationTimeEqual(left, right time.Time) bool {
	return left.UTC().Truncate(time.Microsecond).Equal(right.UTC().Truncate(time.Microsecond))
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

func (w *SyncWorker) resolveHilbertRawDataIDFromSyncLogs(ctx context.Context, syncLogID int64) (int64, error) {
	if w == nil || w.db == nil || syncLogID <= 0 {
		return 0, nil
	}
	var current struct {
		EpisodeID       int64          `db:"episode_id"`
		DestinationPath sql.NullString `db:"destination_path"`
	}
	if err := w.db.GetContext(ctx, &current, "SELECT episode_id, destination_path FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("load Hilbert raw-data id from sync_log %d: %w", syncLogID, err)
	}
	recoveredID, found, err := parseHilbertRawDataIDDestination(syncLogID, current.DestinationPath)
	if err != nil {
		return 0, err
	}
	recoveredSyncLogID := syncLogID
	if !found {
		recoveredID = 0
		recoveredSyncLogID = 0
	}

	var historical []struct {
		ID              int64          `db:"id"`
		DestinationPath sql.NullString `db:"destination_path"`
	}
	if err := w.db.SelectContext(ctx, &historical, `
		SELECT id, destination_path
		FROM sync_logs
		WHERE episode_id = ?
		  AND id <> ?
		  AND destination_path IS NOT NULL
		ORDER BY id DESC
	`, current.EpisodeID, syncLogID); err != nil {
		return 0, fmt.Errorf("load historical Hilbert raw-data ids for episode %d: %w", current.EpisodeID, err)
	}

	for _, candidate := range historical {
		rawDataID, candidateFound, err := parseHilbertRawDataIDDestination(candidate.ID, candidate.DestinationPath)
		if err != nil {
			return 0, err
		}
		if !candidateFound {
			continue
		}
		if recoveredID > 0 && recoveredID != rawDataID {
			return 0, newNonRetryableSyncError(
				"episode %d has conflicting historical Hilbert raw-data ids %d and %d",
				current.EpisodeID,
				recoveredID,
				rawDataID,
			)
		}
		recoveredID = rawDataID
		recoveredSyncLogID = candidate.ID
	}

	if recoveredID > 0 && recoveredSyncLogID != syncLogID {
		logger.Printf("[SYNC-WORKER] Recovered Hilbert raw-data registration for episode %d: raw_data_id=%d historical_sync_log_id=%d sync_log_id=%d",
			current.EpisodeID, recoveredID, recoveredSyncLogID, syncLogID)
	}

	return recoveredID, nil
}

func parseHilbertRawDataIDDestination(syncLogID int64, destination sql.NullString) (int64, bool, error) {
	if !destination.Valid {
		return 0, false, nil
	}
	value := strings.TrimSpace(destination.String)
	if !strings.HasPrefix(value, hilbertRawDataIDDestinationPrefix) {
		return 0, false, nil
	}
	rawDataID, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(value, hilbertRawDataIDDestinationPrefix)), 10, 64)
	if err != nil || rawDataID <= 0 {
		return 0, false, fmt.Errorf("invalid Hilbert raw-data id in sync_log %d: %q", syncLogID, value)
	}
	return rawDataID, true, nil
}

func (w *SyncWorker) persistEpisodeHilbertRawDataID(ctx context.Context, syncLogID, episodeID, rawDataID int64) error {
	if w == nil || w.db == nil {
		return fmt.Errorf("persist Hilbert raw-data id: database is not configured")
	}
	if episodeID <= 0 || rawDataID <= 0 {
		return fmt.Errorf("persist Hilbert raw-data id: invalid episode_id=%d raw_data_id=%d", episodeID, rawDataID)
	}

	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Hilbert raw-data id transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE episodes
		SET hilbert_raw_data_id = ?
		WHERE id = ?
		  AND hilbert_raw_data_id IS NULL
		  AND deleted_at IS NULL
	`, rawDataID, episodeID)
	if err != nil {
		return fmt.Errorf("persist Hilbert raw-data id %d for episode %d: %w", rawDataID, episodeID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Hilbert raw-data id update result for episode %d: %w", episodeID, err)
	}
	if rowsAffected == 0 {
		var existing sql.NullInt64
		if err := tx.GetContext(ctx, &existing, `
			SELECT hilbert_raw_data_id
			FROM episodes
			WHERE id = ? AND deleted_at IS NULL
		`, episodeID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("persist Hilbert raw-data id: episode %d not found", episodeID)
			}
			return fmt.Errorf("load Hilbert raw-data id for episode %d: %w", episodeID, err)
		}
		if !existing.Valid || existing.Int64 <= 0 {
			return fmt.Errorf("episode %d has invalid persisted Hilbert raw-data id", episodeID)
		}
		if existing.Int64 != rawDataID {
			return newNonRetryableSyncError(
				"episode %d Hilbert raw-data id conflict: existing=%d incoming=%d",
				episodeID,
				existing.Int64,
				rawDataID,
			)
		}
	}

	value := hilbertRawDataIDDestinationPrefix + strconv.FormatInt(rawDataID, 10)
	if syncLogID > 0 {
		result, err := tx.ExecContext(ctx, `
			UPDATE sync_logs
			SET destination_path = ?
			WHERE id = ? AND episode_id = ?
		`, value, syncLogID, episodeID)
		if err != nil {
			return fmt.Errorf("persist Hilbert raw-data id %d to sync_log %d: %w", rawDataID, syncLogID, err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read Hilbert raw-data sync_log update result for sync_log %d: %w", syncLogID, err)
		}
		if rowsAffected == 0 {
			var existing struct {
				EpisodeID       int64          `db:"episode_id"`
				DestinationPath sql.NullString `db:"destination_path"`
			}
			if err := tx.GetContext(ctx, &existing, `
				SELECT episode_id, destination_path
				FROM sync_logs
				WHERE id = ?
			`, syncLogID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("persist Hilbert raw-data id: sync_log %d not found", syncLogID)
				}
				return fmt.Errorf("load sync_log %d after Hilbert raw-data id update: %w", syncLogID, err)
			}
			if existing.EpisodeID != episodeID || !existing.DestinationPath.Valid ||
				strings.TrimSpace(existing.DestinationPath.String) != value {
				return fmt.Errorf("persist Hilbert raw-data id: sync_log %d does not belong to episode %d", syncLogID, episodeID)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Hilbert raw-data id %d for episode %d: %w", rawDataID, episodeID, err)
	}
	return nil
}

func episodeSHA256Hex(ep syncEpisodeUploadRow) (string, error) {
	checksum := strings.ToLower(strings.TrimSpace(ep.Checksum.String))
	if !ep.Checksum.Valid || len(checksum) != 64 {
		return "", newNonRetryableSyncError("episode %d missing valid SHA-256 checksum", ep.ID)
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return "", newNonRetryableSyncError("episode %d missing valid SHA-256 checksum", ep.ID)
	}
	return checksum, nil
}

func (w *SyncWorker) resolveEpisodeSHA256Hex(
	ctx context.Context,
	ep syncEpisodeUploadRow,
	source SourceObjectReader,
	sourceBucket string,
	mcapKey string,
	objectSize int64,
	sourceETag string,
) (string, error) {
	if ep.SourceSnapshot == nil && ep.Checksum.Valid && strings.TrimSpace(ep.Checksum.String) != "" {
		return episodeSHA256Hex(ep)
	}

	obj, err := w.openSourceObjectRangeStream(ctx, source, sourceBucket, mcapKey, objectSize, sourceETag)
	if err != nil {
		return "", fmt.Errorf("open ranged mcap object %s to calculate SHA-256: %w", mcapKey, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, obj)
	closeErr := obj.Close()
	if copyErr != nil {
		if closeErr != nil {
			copyErr = errors.Join(copyErr, fmt.Errorf("close mcap object %s: %w", mcapKey, closeErr))
		}
		return "", fmt.Errorf("calculate SHA-256 for mcap object %s: %w", mcapKey, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close mcap object %s after calculating SHA-256: %w", mcapKey, closeErr)
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	if ep.SourceSnapshot == nil && w != nil && w.db != nil {
		if _, err := w.db.ExecContext(ctx, "UPDATE episodes SET checksum = ? WHERE id = ?", checksum, ep.ID); err != nil {
			return "", fmt.Errorf("persist calculated SHA-256 for episode %d: %w", ep.ID, err)
		}
	}
	logger.Printf("[SYNC-WORKER] Episode %d calculated missing SHA-256 checksum from %s/%s", ep.ID, sourceBucket, mcapKey)
	return checksum, nil
}

func (w *SyncWorker) openSourceObjectRangeStream(
	ctx context.Context,
	source SourceObjectReader,
	bucket string,
	objectName string,
	objectSize int64,
	sourceETag string,
) (io.ReadCloser, error) {
	rangeSize := defaultSourceObjectRangeSize
	timeout := 300 * time.Second
	if w != nil {
		if w.sourceObjectRangeSize > 0 {
			rangeSize = w.sourceObjectRangeSize
		}
		timeout = w.syncOSSTimeout()
	}
	return newSourceObjectRangeReader(ctx, source, bucket, objectName, objectSize, sourceETag, rangeSize, timeout)
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
	var lockedEpisode struct {
		ID                 int64          `db:"id"`
		QAStatus           string         `db:"qa_status"`
		CloudPublishSource sql.NullString `db:"cloud_publish_source"`
	}
	if err := tx.GetContext(ctx, &lockedEpisode, `
		SELECT id, COALESCE(qa_status, '') AS qa_status, cloud_publish_source
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("episode %d not found", episodeID)
		}
		return 0, 0, fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	var latest struct {
		ID             int64          `db:"id"`
		Status         string         `db:"status"`
		NextRetry      sql.NullTime   `db:"next_retry_at"`
		AttemptCount   int            `db:"attempt_count"`
		SourceSnapshot sql.NullString `db:"source_snapshot"`
	}
	latestQuery := `
			SELECT sl.id, sl.status, sl.next_retry_at, sl.attempt_count, sl.source_snapshot
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
		if latest.SourceSnapshot.Valid && strings.TrimSpace(latest.SourceSnapshot.String) != "" {
			snapshot, snapshotErr := decodeSyncSourceSnapshot(latest.SourceSnapshot.String)
			if snapshotErr != nil {
				return 0, 0, newNonRetryableSyncError("sync_log %d has invalid source snapshot: %v", latest.ID, snapshotErr)
			}
			if gateErr := w.validateSyncSourceGateTx(
				ctx,
				tx,
				episodeID,
				lockedEpisode.QAStatus,
				strings.TrimSpace(lockedEpisode.CloudPublishSource.String),
				snapshot,
			); gateErr != nil {
				return 0, 0, gateErr
			}
		} else if lockedEpisode.QAStatus != "approved" {
			// Compatibility for legacy rows created before source_snapshot existed.
			return 0, 0, fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, lockedEpisode.QAStatus)
		}
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
		case "canceled":
			return 0, 0, fmt.Errorf("%w for episode %d", errSyncCanceled, episodeID)
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
	if lockedEpisode.QAStatus != "approved" {
		return 0, 0, fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, lockedEpisode.QAStatus)
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

	snapshot, hasSnapshot, err := loadSyncSourceSnapshot(ctx, tx, syncLogID)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to load source snapshot for episode %d completion: %v", episodeID, err)
		return
	}
	updateQuery := `
		UPDATE episodes
		SET cloud_synced = TRUE,
		    cloud_synced_at = ?,
		    cloud_mcap_path = ?,
		    cloud_processed = FALSE
		WHERE id = ?
		  AND deleted_at IS NULL
		  AND qa_status = 'approved'`
	args := []any{now, result.ObjectKey, episodeID}
	if hasSnapshot && snapshot.SourceType == SyncSourceOriginal {
		updateQuery += " AND cloud_publish_source = 'original'"
	} else if hasSnapshot && snapshot.SourceType == SyncSourceStereoSplit {
		updateQuery = `
			UPDATE episodes
			SET cloud_synced = TRUE,
			    cloud_synced_at = ?,
			    cloud_mcap_path = ?,
			    cloud_processed = FALSE
			WHERE id = ?
			  AND deleted_at IS NULL
			  AND cloud_publish_source = 'stereo_split'
			  AND EXISTS (
			    SELECT 1 FROM episode_derivatives ed
			    WHERE ed.id = ? AND ed.episode_id = episodes.id
			      AND ed.kind = 'stereo_split' AND ed.generation = ?
			      AND ed.processing_status = 'succeeded' AND ed.qa_status = 'approved'
			  )`
		args = append(args, snapshot.DerivativeID, snapshot.Generation)
	}
	// Keep cloud_synced and the source-specific QA gate mutually consistent even
	// if another writer bypasses the normal claim guards.
	episodeResult, err := tx.ExecContext(ctx, updateQuery, args...)
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to update episode %d cloud status: %v", episodeID, err)
		return
	}
	affected, err := episodeResult.RowsAffected()
	if err != nil {
		logger.Printf("[SYNC-WORKER] Failed to read episode %d cloud status rows affected: %v", episodeID, err)
		return
	}
	if affected != 1 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sync_logs
			SET status = 'failed',
			    error_message = 'sync source eligibility changed before completion',
			    duration_sec = ?,
			    completed_at = ?,
			    next_retry_at = NULL
			WHERE id = ? AND status = 'in_progress'
		`, durationSec, now, syncLogID); err != nil {
			logger.Printf("[SYNC-WORKER] Failed to reject sync completion for episode %d: %v", episodeID, err)
			return
		}
		if err := tx.Commit(); err != nil {
			logger.Printf("[SYNC-WORKER] Failed to commit rejected sync completion for episode %d: %v", episodeID, err)
			return
		}
		logger.Printf("[SYNC-WORKER] Rejected sync completion for episode %d because source eligibility changed", episodeID)
		return
	}

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
