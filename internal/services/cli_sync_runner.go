// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/jmoiron/sqlx"
	"github.com/minio/minio-go/v7"
)

const (
	cliSyncStatusPending    = "pending"
	cliSyncStatusInProgress = "in_progress"
	cliSyncStatusCompleted  = "completed"
	cliSyncStatusFailed     = "failed"

	cliSyncPollInterval = 30 * time.Second
)

var (
	ErrCLISyncDisabled         = errors.New("CLI sync is disabled")
	ErrCLISyncNotRunning       = errors.New("CLI sync runner is not running")
	ErrCLISyncQueueFull        = errors.New("CLI sync queue is full")
	ErrCLISyncAlreadyActive    = errors.New("CLI sync already active for episode")
	ErrCLISyncNormalSyncActive = errors.New("normal cloud sync already active for episode")
	ErrCLISyncEpisodeNotFound  = errors.New("episode not found")
	ErrCLISyncAlreadySynced    = errors.New("episode already synced to cloud")
	ErrCLISyncNotEligible      = errors.New("episode is not eligible for CLI sync")
)

// CLISyncRunnerConfig controls the dp CLI cloud sync sidepath.
type CLISyncRunnerConfig struct {
	Enabled       bool
	DPBin         string
	DPConfigPath  string
	TempDir       string
	MaxConcurrent int
	QueueSize     int
	TimeoutSec    int
	KeepTemp      bool
	MaxTags       int
	MaxTagBytes   int
}

// CLISyncRun is the API-facing representation of one CLI sync run.
type CLISyncRun struct {
	ID              int64          `db:"id"`
	EpisodeID       int64          `db:"episode_id"`
	Status          string         `db:"status"`
	SourcePath      sql.NullString `db:"source_path"`
	TempPath        sql.NullString `db:"temp_path"`
	DPConfigPath    sql.NullString `db:"dp_config_path"`
	FileID          sql.NullString `db:"file_id"`
	LogicalUploadID sql.NullString `db:"logical_upload_id"`
	UploadID        sql.NullString `db:"upload_id"`
	Bucket          sql.NullString `db:"bucket"`
	ObjectKey       sql.NullString `db:"object_key"`
	FileSize        sql.NullInt64  `db:"file_size"`
	OSSObjectETag   sql.NullString `db:"oss_object_etag"`
	DurationSec     sql.NullInt64  `db:"duration_sec"`
	ErrorMessage    sql.NullString `db:"error_message"`
	StartedAt       sql.NullTime   `db:"started_at"`
	CompletedAt     sql.NullTime   `db:"completed_at"`
}

type cliSyncEpisode struct {
	ID              int64          `db:"id"`
	EpisodePublicID string         `db:"episode_id"`
	QAStatus        string         `db:"qa_status"`
	McapPath        string         `db:"mcap_path"`
	SidecarPath     string         `db:"sidecar_path"`
	CloudSynced     bool           `db:"cloud_synced"`
	RobotDeviceID   sql.NullString `db:"robot_device_id"`
	TaskID          sql.NullInt64  `db:"task_id"`
	FactoryID       sql.NullInt64  `db:"factory_id"`
	OrganizationID  sql.NullInt64  `db:"organization_id"`
}

type cliUploadResult struct {
	LogicalUploadID string `json:"logicalUploadId"`
	UploadID        string `json:"uploadId"`
	FileID          string `json:"fileId"`
	Bucket          string `json:"bucket"`
	ObjectKey       string `json:"objectKey"`
	FileSize        int64  `json:"fileSize"`
	OSSObjectETag   string `json:"ossObjectEtag"`
}

// CLISyncRunner owns the emergency dp CLI upload sidepath.
type CLISyncRunner struct {
	db          *sqlx.DB
	minioClient *s3.Client
	minioBucket string
	cfg         CLISyncRunnerConfig

	runCh     chan int64
	running   atomic.Bool
	stopping  atomic.Bool
	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
}

// NewCLISyncRunner creates a runner. Call Start before accepting enqueue requests.
func NewCLISyncRunner(db *sqlx.DB, minioClient *s3.Client, minioBucket string, cfg CLISyncRunnerConfig) (*CLISyncRunner, error) {
	if !cfg.Enabled {
		return &CLISyncRunner{db: db, minioClient: minioClient, minioBucket: minioBucket, cfg: cfg}, nil
	}
	if db == nil {
		return nil, fmt.Errorf("CLI sync requires database")
	}
	if minioClient == nil {
		return nil, fmt.Errorf("CLI sync requires MinIO client")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 16
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 7200
	}
	if cfg.MaxTags <= 0 {
		cfg.MaxTags = 128
	}
	if cfg.MaxTagBytes <= 0 {
		cfg.MaxTagBytes = 65536
	}
	if strings.TrimSpace(cfg.TempDir) == "" {
		cfg.TempDir = "/var/lib/keystone/cli-sync"
	}
	if err := os.MkdirAll(cfg.TempDir, 0o750); err != nil {
		return nil, fmt.Errorf("create CLI sync temp dir: %w", err)
	}
	probe, err := os.CreateTemp(cfg.TempDir, ".write-probe-*")
	if err != nil {
		return nil, fmt.Errorf("CLI sync temp dir is not writable: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return nil, fmt.Errorf("close CLI sync temp probe: %w", err)
	}
	_ = os.Remove(probePath)

	return &CLISyncRunner{
		db:          db,
		minioClient: minioClient,
		minioBucket: minioBucket,
		cfg:         cfg,
		runCh:       make(chan int64, cfg.QueueSize),
	}, nil
}

// IsEnabled reports whether the sidepath is configured.
func (r *CLISyncRunner) IsEnabled() bool {
	return r != nil && r.cfg.Enabled
}

// IsRunning reports whether background workers are accepting runs.
func (r *CLISyncRunner) IsRunning() bool {
	return r != nil && r.running.Load()
}

// Start starts background CLI sync workers.
func (r *CLISyncRunner) Start() {
	if r == nil || !r.cfg.Enabled {
		return
	}
	r.mu.Lock()
	if !r.running.CompareAndSwap(false, true) {
		r.mu.Unlock()
		return
	}
	r.runCtx, r.runCancel = context.WithCancel(context.Background())
	runCtx := r.runCtx
	r.mu.Unlock()

	for i := 0; i < r.cfg.MaxConcurrent; i++ {
		r.wg.Add(1)
		go r.worker(runCtx)
	}
	r.wg.Add(1)
	go r.dispatcher(runCtx)

	logger.Printf("[CLI-SYNC] Started (dp=%s concurrency=%d queue=%d)", r.cfg.DPBin, r.cfg.MaxConcurrent, r.cfg.QueueSize)
}

// Stop gracefully stops the runner.
func (r *CLISyncRunner) Stop(ctx context.Context) error {
	if r == nil || !r.cfg.Enabled {
		return nil
	}
	r.mu.Lock()
	if !r.running.Load() {
		r.mu.Unlock()
		return nil
	}
	r.running.Store(false)
	r.stopping.Store(true)
	cancel := r.runCancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Printf("[CLI-SYNC] Stopped")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("CLI sync runner shutdown: %w", ctx.Err())
	}
}

// EnqueueEpisode creates a CLI sync run and schedules it for background processing.
func (r *CLISyncRunner) EnqueueEpisode(ctx context.Context, episodeID int64) (int64, error) {
	if r == nil || !r.cfg.Enabled {
		return 0, ErrCLISyncDisabled
	}
	if !r.running.Load() {
		return 0, ErrCLISyncNotRunning
	}
	runID, err := r.persistPendingRun(ctx, episodeID)
	if err != nil {
		return 0, err
	}

	select {
	case r.runCh <- runID:
		return runID, nil
	case <-ctx.Done():
		r.markRunFailed(context.Background(), runID, time.Now(), ctx.Err())
		return 0, ctx.Err()
	default:
		r.markRunFailed(context.Background(), runID, time.Now(), ErrCLISyncQueueFull)
		return 0, ErrCLISyncQueueFull
	}
}

// LatestRun returns the most recent CLI sync run for an episode.
func (r *CLISyncRunner) LatestRun(ctx context.Context, episodeID int64) (*CLISyncRun, error) {
	if r == nil || !r.cfg.Enabled {
		return nil, ErrCLISyncDisabled
	}
	var row CLISyncRun
	err := r.db.GetContext(ctx, &row, `
		SELECT
			id,
			episode_id,
			status,
			source_path,
			temp_path,
			dp_config_path,
			file_id,
			logical_upload_id,
			upload_id,
			bucket,
			object_key,
			file_size,
			oss_object_etag,
			duration_sec,
			error_message,
			started_at,
			completed_at
		FROM cli_sync_runs
		WHERE episode_id = ?
		ORDER BY id DESC
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("query latest CLI sync run: %w", err)
	}
	return &row, nil
}

func (r *CLISyncRunner) persistPendingRun(ctx context.Context, episodeID int64) (int64, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin CLI sync transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)
	var ep cliSyncEpisode
	if err := tx.GetContext(ctx, &ep, `
		SELECT
			e.id,
			e.episode_id,
			e.qa_status,
			e.mcap_path,
			e.sidecar_path,
			e.cloud_synced,
			COALESCE(NULLIF(TRIM(r.device_id), ''), NULLIF(TRIM(ws.robot_serial), '')) AS robot_device_id,
			e.task_id,
			e.factory_id,
			e.organization_id
		FROM episodes e
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL
	`+lockClause, episodeID); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("%w: %d", ErrCLISyncEpisodeNotFound, episodeID)
		}
		return 0, fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	if ep.CloudSynced {
		return 0, fmt.Errorf("%w: %d", ErrCLISyncAlreadySynced, episodeID)
	}
	if ep.QAStatus != "approved" && ep.QAStatus != "inspector_approved" {
		return 0, fmt.Errorf("%w: qa_status=%s", ErrCLISyncNotEligible, ep.QAStatus)
	}
	if strings.TrimSpace(ep.McapPath) == "" {
		return 0, fmt.Errorf("%w: empty mcap_path", ErrCLISyncNotEligible)
	}
	if strings.TrimSpace(ep.SidecarPath) == "" {
		return 0, fmt.Errorf("%w: empty sidecar_path", ErrCLISyncNotEligible)
	}
	if strings.TrimSpace(ep.RobotDeviceID.String) == "" {
		return 0, fmt.Errorf("%w: empty robot_device_id", ErrCLISyncNotEligible)
	}

	var normalActive int
	if err := tx.GetContext(ctx, &normalActive, `
		SELECT COUNT(*)
		FROM sync_logs
		WHERE episode_id = ?
		  AND status IN ('pending', 'in_progress')
	`, episodeID); err != nil {
		return 0, fmt.Errorf("query active normal sync count: %w", err)
	}
	if normalActive > 0 {
		return 0, fmt.Errorf("%w: %d", ErrCLISyncNormalSyncActive, episodeID)
	}

	var cliActive int
	if err := tx.GetContext(ctx, &cliActive, `
		SELECT COUNT(*)
		FROM cli_sync_runs
		WHERE episode_id = ?
		  AND status IN ('pending', 'in_progress')
	`, episodeID); err != nil {
		return 0, fmt.Errorf("query active CLI sync count: %w", err)
	}
	if cliActive > 0 {
		return 0, fmt.Errorf("%w: %d", ErrCLISyncAlreadyActive, episodeID)
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO cli_sync_runs (episode_id, status, source_path, dp_config_path, created_at, updated_at)
		VALUES (?, 'pending', ?, ?, ?, ?)
	`, episodeID, ep.McapPath, r.cfg.DPConfigPath, now, now)
	if err != nil {
		return 0, fmt.Errorf("insert CLI sync run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CLI sync run last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit CLI sync run: %w", err)
	}
	return runID, nil
}

func (r *CLISyncRunner) dispatcher(ctx context.Context) {
	defer r.wg.Done()
	r.dispatchPendingRuns(ctx)
	ticker := time.NewTicker(cliSyncPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.dispatchPendingRuns(ctx)
		}
	}
}

func (r *CLISyncRunner) dispatchPendingRuns(ctx context.Context) {
	var ids []int64
	if err := r.db.SelectContext(ctx, &ids, `
		SELECT id
		FROM cli_sync_runs
		WHERE status = 'pending'
		ORDER BY id ASC
		LIMIT ?
	`, r.cfg.QueueSize); err != nil {
		if ctx.Err() == nil {
			logger.Printf("[CLI-SYNC] Failed to query pending runs: %v", err)
		}
		return
	}
	for _, id := range ids {
		select {
		case r.runCh <- id:
		default:
			return
		}
	}
}

func (r *CLISyncRunner) worker(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case runID := <-r.runCh:
			r.processRun(ctx, runID)
		}
	}
}

func (r *CLISyncRunner) processRun(parent context.Context, runID int64) {
	startedAt := time.Now().UTC()
	claimed, err := r.claimRun(parent, runID, startedAt)
	if err != nil {
		logger.Printf("[CLI-SYNC] Failed to claim run %d: %v", runID, err)
		return
	}
	if !claimed {
		return
	}
	logger.Printf("[CLI-SYNC] Run %d claimed", runID)

	ctx, cancel := context.WithTimeout(parent, time.Duration(r.cfg.TimeoutSec)*time.Second)
	defer cancel()

	var ep cliSyncEpisode
	if err := r.loadEpisodeForRun(ctx, runID, &ep); err != nil {
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}
	deviceID := strings.TrimSpace(ep.RobotDeviceID.String)
	if deviceID == "" {
		r.markRunFailed(context.Background(), runID, startedAt, fmt.Errorf("%w: empty robot_device_id", ErrCLISyncNotEligible))
		return
	}
	logger.Printf("[CLI-SYNC] Run %d loaded episode: episode_id=%d public_id=%s qa_status=%s device_id=%s mcap_path=%s sidecar_path=%s",
		runID, ep.ID, ep.EpisodePublicID, ep.QAStatus, deviceID, ep.McapPath, ep.SidecarPath)

	tags, err := r.buildTagsFromEpisode(ctx, ep)
	if err != nil {
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}
	logger.Printf("[CLI-SYNC] Run %d built upload tags: episode_id=%d tag_count=%d", runID, ep.ID, len(tags))

	mcapKey := stripBucketPrefix(ep.McapPath)
	if mcapKey == "" {
		r.markRunFailed(context.Background(), runID, startedAt, fmt.Errorf("empty mcap_path"))
		return
	}
	logger.Printf("[CLI-SYNC] Run %d staging MCAP from MinIO: episode_id=%d bucket=%s key=%s temp_dir=%s",
		runID, ep.ID, r.minioBucket, mcapKey, r.cfg.TempDir)

	tempPath, fileSize, err := r.stageMcap(ctx, ep.ID, mcapKey)
	if err != nil {
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}
	logger.Printf("[CLI-SYNC] Run %d staged MCAP: episode_id=%d temp_path=%s size=%d bytes",
		runID, ep.ID, tempPath, fileSize)
	if !r.cfg.KeepTemp {
		defer func() { _ = os.Remove(tempPath) }()
	}
	if err := r.setRunTempPath(context.Background(), runID, tempPath); err != nil {
		logger.Printf("[CLI-SYNC] Failed to update temp path for run %d: %v", runID, err)
	}

	uploadStartedAt := time.Now()
	logger.Printf("[CLI-SYNC] Run %d starting dp upload: episode_id=%d dp_bin=%s device_id=%s tag_count=%d file_size=%d",
		runID, ep.ID, r.cfg.DPBin, deviceID, len(tags), fileSize)
	result, stdoutJSON, err := r.runDPUpload(ctx, tempPath, tags, deviceID)
	if err != nil {
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}
	logger.Printf("[CLI-SYNC] Run %d dp upload finished: episode_id=%d elapsed=%s file_id=%s logical_upload_id=%s object_key=%s",
		runID, ep.ID, time.Since(uploadStartedAt).Round(time.Millisecond), result.FileID, result.LogicalUploadID, result.ObjectKey)
	if result.FileSize <= 0 {
		result.FileSize = fileSize
	}
	if err := validateCLIUploadResult(result); err != nil {
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}

	if err := r.markRunCompleted(context.Background(), runID, ep, result, stdoutJSON, startedAt); err != nil {
		logger.Printf("[CLI-SYNC] Failed to mark run %d completed: %v", runID, err)
		r.markRunFailed(context.Background(), runID, startedAt, err)
		return
	}
	logger.Printf("[CLI-SYNC] Episode %d CLI synced: run_id=%d file_id=%s logical_upload_id=%s object_key=%s",
		ep.ID, runID, result.FileID, result.LogicalUploadID, result.ObjectKey)
}

func (r *CLISyncRunner) loadEpisodeForRun(ctx context.Context, runID int64, ep *cliSyncEpisode) error {
	if err := r.db.GetContext(ctx, ep, `
		SELECT
			e.id,
			e.episode_id,
			e.qa_status,
			e.mcap_path,
			e.sidecar_path,
			e.cloud_synced,
			COALESCE(NULLIF(TRIM(r.device_id), ''), NULLIF(TRIM(ws.robot_serial), '')) AS robot_device_id,
			e.task_id,
			e.factory_id,
			e.organization_id
		FROM cli_sync_runs csr
		INNER JOIN episodes e ON e.id = csr.episode_id AND e.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE csr.id = ?
	`, runID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w for CLI sync run %d", ErrCLISyncEpisodeNotFound, runID)
		}
		return fmt.Errorf("load episode for CLI sync run %d: %w", runID, err)
	}
	return nil
}

func (r *CLISyncRunner) claimRun(ctx context.Context, runID int64, startedAt time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE cli_sync_runs
		SET status = 'in_progress',
		    started_at = ?,
		    error_message = NULL,
		    updated_at = ?
		WHERE id = ?
		  AND status = 'pending'
	`, startedAt, startedAt, runID)
	if err != nil {
		return false, fmt.Errorf("claim CLI sync run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim CLI sync rows affected: %w", err)
	}
	return n == 1, nil
}

func (r *CLISyncRunner) buildTagsFromEpisode(ctx context.Context, ep cliSyncEpisode) (map[string]string, error) {
	sidecarTags, err := r.tagsFromSidecar(ctx, ep.SidecarPath)
	if err != nil {
		return nil, err
	}

	tags := make(map[string]string, len(sidecarTags)+6)
	for k, v := range sidecarTags {
		tags[k] = v
	}
	tags["episode_id"] = ep.EpisodePublicID
	tags["keystone_episode_id"] = strconv.FormatInt(ep.ID, 10)
	tags["sync_channel"] = "keystone_cli"
	if deviceID := strings.TrimSpace(ep.RobotDeviceID.String); deviceID != "" {
		tags["device_id"] = deviceID
	}
	if ep.TaskID.Valid {
		tags["task_id"] = strconv.FormatInt(ep.TaskID.Int64, 10)
	}
	if ep.FactoryID.Valid {
		tags["factory_id"] = strconv.FormatInt(ep.FactoryID.Int64, 10)
	}
	if ep.OrganizationID.Valid {
		tags["organization_id"] = strconv.FormatInt(ep.OrganizationID.Int64, 10)
	}

	if err := r.validateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func (r *CLISyncRunner) tagsFromSidecar(ctx context.Context, sidecarPath string) (map[string]string, error) {
	key := stripBucketPrefix(sidecarPath)
	if key == "" {
		return nil, fmt.Errorf("empty sidecar_path")
	}
	startedAt := time.Now()
	logger.Printf("[CLI-SYNC] Reading sidecar from MinIO: bucket=%s key=%s", r.minioBucket, key)
	obj, err := r.minioClient.GetObject(ctx, r.minioBucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get sidecar object %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read sidecar object %s: %w", key, err)
	}
	tags, err := flattenSidecarScalars(data)
	if err != nil {
		return nil, fmt.Errorf("flatten sidecar %s: %w", key, err)
	}
	logger.Printf("[CLI-SYNC] Read sidecar complete: bucket=%s key=%s bytes=%d scalar_tag_count=%d elapsed=%s",
		r.minioBucket, key, len(data), len(tags), time.Since(startedAt).Round(time.Millisecond))
	return tags, nil
}

func (r *CLISyncRunner) validateTags(tags map[string]string) error {
	if len(tags) > r.cfg.MaxTags {
		return fmt.Errorf("too many CLI sync tags: %d > %d", len(tags), r.cfg.MaxTags)
	}
	totalBytes := 0
	for key, value := range tags {
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("CLI sync tag key is empty")
		}
		if strings.ContainsAny(key, ",=") {
			return fmt.Errorf("CLI sync tag key %q contains unsupported characters", key)
		}
		totalBytes += len(key) + 1 + len(encodeDPTagValue(value))
	}
	if totalBytes > r.cfg.MaxTagBytes {
		return fmt.Errorf("CLI sync tags too large: %d > %d bytes", totalBytes, r.cfg.MaxTagBytes)
	}
	return nil
}

func (r *CLISyncRunner) stageMcap(ctx context.Context, episodeID int64, mcapKey string) (string, int64, error) {
	startedAt := time.Now()
	obj, err := r.minioClient.GetObject(ctx, r.minioBucket, mcapKey, minio.GetObjectOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("get MCAP object %s: %w", mcapKey, err)
	}
	defer func() { _ = obj.Close() }()

	tmp, err := os.CreateTemp(r.cfg.TempDir, fmt.Sprintf("episode-%d-*.mcap", episodeID))
	if err != nil {
		return "", 0, fmt.Errorf("create CLI sync temp file: %w", err)
	}
	tempPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	size, err := io.Copy(tmp, obj)
	if err != nil {
		return "", 0, fmt.Errorf("write CLI sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close CLI sync temp file: %w", err)
	}
	if size <= 0 {
		return "", 0, fmt.Errorf("zero-byte MCAP cannot be CLI synced")
	}
	cleanup = false
	logger.Printf("[CLI-SYNC] MCAP download complete: episode_id=%d bucket=%s key=%s temp_path=%s size=%d elapsed=%s",
		episodeID, r.minioBucket, mcapKey, tempPath, size, time.Since(startedAt).Round(time.Millisecond))
	return tempPath, size, nil
}

func (r *CLISyncRunner) runDPUpload(ctx context.Context, tempPath string, tags map[string]string, deviceID string) (*cliUploadResult, string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, "", fmt.Errorf("dp device id is required")
	}
	args := []string{
		"--config", r.cfg.DPConfigPath,
		"--json",
		"data", "upload", tempPath,
		"--device", deviceID,
		"--hint", "source=keystone_cli_sync",
	}

	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--tag", key+"="+encodeDPTagValue(tags[key]))
	}
	logger.Printf("[CLI-SYNC] Prepared dp command: dp_bin=%s file=%s device_id=%s tag_count=%d hint_count=1",
		r.cfg.DPBin, tempPath, deviceID, len(tags))

	cmd := exec.CommandContext(ctx, r.cfg.DPBin, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(stderr.String())
		if output == "" {
			output = strings.TrimSpace(stdout.String())
		}
		return nil, "", fmt.Errorf("dp data upload failed: %s", sanitizeCLIOutput(output, err))
	}

	stdoutText := strings.TrimSpace(stdout.String())
	var result cliUploadResult
	if err := json.Unmarshal([]byte(stdoutText), &result); err != nil {
		return nil, "", fmt.Errorf("parse dp upload JSON: %w", err)
	}
	return &result, stdoutText, nil
}

func validateCLIUploadResult(result *cliUploadResult) error {
	if result == nil {
		return fmt.Errorf("dp upload result is empty")
	}
	if strings.TrimSpace(result.FileID) == "" {
		return fmt.Errorf("dp upload result missing fileId")
	}
	if strings.TrimSpace(result.LogicalUploadID) == "" {
		return fmt.Errorf("dp upload result missing logicalUploadId")
	}
	if strings.TrimSpace(result.ObjectKey) == "" {
		return fmt.Errorf("dp upload result missing objectKey")
	}
	if result.FileSize <= 0 {
		return fmt.Errorf("dp upload result has invalid fileSize")
	}
	return nil
}

func (r *CLISyncRunner) setRunTempPath(ctx context.Context, runID int64, tempPath string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE cli_sync_runs
		SET temp_path = ?, updated_at = ?
		WHERE id = ?
	`, tempPath, time.Now().UTC(), runID)
	return err
}

func (r *CLISyncRunner) markRunCompleted(ctx context.Context, runID int64, ep cliSyncEpisode, result *cliUploadResult, stdoutJSON string, startedAt time.Time) error {
	now := time.Now().UTC()
	durationSec := int64(now.Sub(startedAt).Seconds())

	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin CLI sync completion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := txLockClause(tx)
	var cloudSynced bool
	if err := tx.GetContext(ctx, &cloudSynced, `
		SELECT cloud_synced
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
	`+lockClause, ep.ID); err != nil {
		return fmt.Errorf("lock episode for CLI sync completion: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE cli_sync_runs
		SET status = 'completed',
		    file_id = ?,
		    logical_upload_id = ?,
		    upload_id = ?,
		    bucket = ?,
		    object_key = ?,
		    file_size = ?,
		    oss_object_etag = ?,
		    duration_sec = ?,
		    error_message = NULL,
		    stdout_json = ?,
		    completed_at = ?,
		    updated_at = ?
		WHERE id = ?
	`, result.FileID, result.LogicalUploadID, nullableStringValue(result.UploadID), result.Bucket, result.ObjectKey,
		result.FileSize, result.OSSObjectETag, durationSec, stdoutJSON, now, now, runID); err != nil {
		return fmt.Errorf("update CLI sync run completed: %w", err)
	}

	if cloudSynced {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_logs (episode_id, source_path, destination_path, status, bytes_transferred, duration_sec, attempt_count, started_at, completed_at)
		VALUES (?, ?, ?, 'completed', ?, ?, 1, ?, ?)
	`, ep.ID, ep.McapPath, result.ObjectKey, result.FileSize, durationSec, startedAt, now); err != nil {
		return fmt.Errorf("insert CLI sync completed log: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE episodes
		SET cloud_synced = TRUE,
		    cloud_synced_at = ?,
		    cloud_mcap_path = ?,
		    cloud_processed = FALSE
		WHERE id = ? AND deleted_at IS NULL
	`, now, result.ObjectKey, ep.ID); err != nil {
		return fmt.Errorf("update episode CLI sync cloud state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit CLI sync completion: %w", err)
	}
	return nil
}

func (r *CLISyncRunner) markRunFailed(ctx context.Context, runID int64, startedAt time.Time, runErr error) {
	now := time.Now().UTC()
	durationSec := int64(now.Sub(startedAt).Seconds())
	msg := sanitizeCLIOutput("", runErr)
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	logger.Printf("[CLI-SYNC] Run %d failed: duration=%ds error=%s", runID, durationSec, msg)
	if _, err := r.db.ExecContext(ctx, `
		UPDATE cli_sync_runs
		SET status = 'failed',
		    duration_sec = ?,
		    error_message = ?,
		    completed_at = ?,
		    updated_at = ?
		WHERE id = ?
	`, durationSec, msg, now, now, runID); err != nil {
		logger.Printf("[CLI-SYNC] Failed to mark run %d failed: %v", runID, err)
	}
}

func nullableStringValue(value string) interface{} {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

var cliSecretPattern = regexp.MustCompile(`(?i)(authorization|access[_-]?key|secret|token|password|api[_-]?key)(["'=:\s]+)([^,\s"}]+)`)

func encodeDPTagValue(value string) string {
	value = strings.ReplaceAll(value, `%`, `%25`)
	value = strings.ReplaceAll(value, `,`, `%2C`)
	return value
}

func sanitizeCLIOutput(output string, err error) string {
	text := strings.TrimSpace(output)
	if text == "" && err != nil {
		text = err.Error()
	}
	text = cliSecretPattern.ReplaceAllString(text, `$1$2<redacted>`)
	if len(text) > 4096 {
		text = text[:4096] + "...<truncated>"
	}
	return text
}
