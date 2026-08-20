// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	// ErrLocalCleanupNotSynced indicates that cloud durability has not been confirmed.
	ErrLocalCleanupNotSynced = errors.New("episode is not cloud-synced")
	// ErrLocalCleanupSyncActive prevents removal while an upload can read the source.
	ErrLocalCleanupSyncActive = errors.New("episode cloud sync is active")
	// ErrLocalCleanupUnsupportedSource prevents deletion of non-MinIO objects.
	ErrLocalCleanupUnsupportedSource = errors.New("episode source is not local MinIO")
	// ErrLocalCleanupSourceUnavailable means no immutable source location was retained.
	ErrLocalCleanupSourceUnavailable = errors.New("local source object identity is unavailable")
)

// LocalObjectDeleter deletes a local object. Its operation must be idempotent.
type LocalObjectDeleter interface {
	DeleteObject(ctx context.Context, bucket, objectKey string) error
}

// LocalCleanupResult describes the terminal state of one local-object cleanup.
type LocalCleanupResult struct {
	JobID     int64  `json:"cleanup_job_id"`
	EpisodeID int64  `json:"episode_id"`
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"object_key"`
	Status    string `json:"status"`
}

// LocalCleanupJob describes the persisted state of a cleanup request.
type LocalCleanupJob struct {
	JobID        int64          `json:"cleanup_job_id" db:"id"`
	EpisodeID    int64          `json:"episode_id" db:"episode_id"`
	Bucket       string         `json:"bucket" db:"bucket"`
	ObjectKey    string         `json:"object_key" db:"object_key"`
	Status       string         `json:"status" db:"status"`
	RequestedBy  sql.NullString `json:"-" db:"requested_by"`
	RequestedAt  time.Time      `json:"requested_at" db:"requested_at"`
	StartedAt    sql.NullTime   `json:"started_at" db:"started_at"`
	CompletedAt  sql.NullTime   `json:"completed_at" db:"completed_at"`
	RetryCount   int            `json:"retry_count" db:"retry_count"`
	ErrorMessage sql.NullString `json:"error_message" db:"error_message"`
}

// LocalCleanupWorker processes persisted cleanup jobs and can be safely
// restarted because object deletion is idempotent.
type LocalCleanupWorker struct {
	service  *LocalCleanupService
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewLocalCleanupWorker creates a worker for pending and interrupted jobs.
func NewLocalCleanupWorker(service *LocalCleanupService, interval time.Duration) *LocalCleanupWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &LocalCleanupWorker{service: service, interval: interval}
}

// Start begins cleanup processing. It is safe to call once.
func (w *LocalCleanupWorker) Start() {
	if w == nil || w.service == nil || w.stop != nil {
		return
	}
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		w.process(context.Background())
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.process(context.Background())
			case <-w.stop:
				return
			}
		}
	}()
}

// Stop waits for the worker to finish its current database/object operation.
func (w *LocalCleanupWorker) Stop(ctx context.Context) error {
	if w == nil || w.stop == nil {
		return nil
	}
	close(w.stop)
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *LocalCleanupWorker) process(ctx context.Context) {
	jobs, err := w.service.pendingJobs(ctx)
	if err != nil {
		return
	}
	for _, job := range jobs {
		_ = w.service.processJob(ctx, job)
	}
}

func (s *LocalCleanupService) pendingJobs(ctx context.Context) ([]LocalCleanupJob, error) {
	var jobs []LocalCleanupJob
	err := s.db.SelectContext(ctx, &jobs, `
		SELECT id, episode_id, bucket, object_key, status, requested_by, requested_at,
		       started_at, completed_at, retry_count, error_message
		FROM local_cleanup_jobs
		WHERE status IN ('pending', 'in_progress', 'failed')
		ORDER BY requested_at, id LIMIT 50`)
	return jobs, err
}

func (s *LocalCleanupService) processJob(ctx context.Context, job LocalCleanupJob) error {
	if err := s.store.DeleteObject(ctx, job.Bucket, job.ObjectKey); err != nil {
		s.recordFailure(ctx, job.EpisodeID, job.JobID, err)
		return err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE episodes SET local_storage_status = 'deleted', local_storage_deleted_at = ?, local_storage_delete_error = NULL WHERE id = ?`, now, job.EpisodeID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE local_cleanup_jobs SET status = 'completed', completed_at = ?, error_message = NULL WHERE id = ?`, now, job.JobID)
	return err
}

// RequestCleanupEpisode validates and queues a cleanup without touching MinIO.
func (s *LocalCleanupService) RequestCleanupEpisode(ctx context.Context, episodeID int64, requestedBy string) (LocalCleanupJob, error) {
	if s == nil || s.db == nil || s.store == nil {
		return LocalCleanupJob{}, fmt.Errorf("local cleanup is not configured")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return LocalCleanupJob{}, fmt.Errorf("begin local cleanup request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var episode struct {
		CloudSynced        bool   `db:"cloud_synced"`
		LocalStorageStatus string `db:"local_storage_status"`
	}
	if err := tx.GetContext(ctx, &episode, `SELECT cloud_synced, local_storage_status FROM episodes WHERE id = ? AND deleted_at IS NULL`, episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LocalCleanupJob{}, sql.ErrNoRows
		}
		return LocalCleanupJob{}, fmt.Errorf("load cleanup episode: %w", err)
	}
	if !episode.CloudSynced {
		return LocalCleanupJob{}, ErrLocalCleanupNotSynced
	}
	if episode.LocalStorageStatus == "deleted" {
		return s.GetCleanupJob(ctx, episodeID)
	}
	var active int
	if err := tx.GetContext(ctx, &active, `SELECT COUNT(*) FROM sync_logs WHERE episode_id = ? AND status IN ('pending', 'in_progress')`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	if active != 0 {
		return LocalCleanupJob{}, ErrLocalCleanupSyncActive
	}
	var raw sql.NullString
	if err := tx.GetContext(ctx, &raw, `SELECT source_snapshot FROM sync_logs WHERE episode_id = ? AND status = 'completed' ORDER BY id DESC LIMIT 1`, episodeID); err != nil || !raw.Valid {
		if errors.Is(err, sql.ErrNoRows) || !raw.Valid {
			return LocalCleanupJob{}, ErrLocalCleanupSourceUnavailable
		}
		return LocalCleanupJob{}, err
	}
	var source SyncSourceSnapshot
	if json.Unmarshal([]byte(raw.String), &source) != nil || source.Backend != SyncBackendMinIO || source.SourceType != SyncSourceOriginal || strings.TrimSpace(source.Bucket) == "" || strings.TrimSpace(source.ObjectKey) == "" || s.bucket == "" || !strings.EqualFold(source.Bucket, s.bucket) {
		return LocalCleanupJob{}, ErrLocalCleanupUnsupportedSource
	}
	now := time.Now().UTC()
	var jobID int64
	if err := tx.GetContext(ctx, &jobID, `SELECT id FROM local_cleanup_jobs WHERE episode_id = ?`, episodeID); errors.Is(err, sql.ErrNoRows) {
		res, insertErr := tx.ExecContext(ctx, `INSERT INTO local_cleanup_jobs (episode_id, bucket, object_key, status, requested_by, requested_at, retry_count) VALUES (?, ?, ?, 'pending', ?, ?, 0)`, episodeID, source.Bucket, source.ObjectKey, strings.TrimSpace(requestedBy), now)
		if insertErr != nil {
			return LocalCleanupJob{}, insertErr
		}
		newJobID, err := res.LastInsertId()
		if err != nil {
			return LocalCleanupJob{}, fmt.Errorf("load new local cleanup job id: %w", err)
		}
		jobID = newJobID
	} else if err != nil {
		return LocalCleanupJob{}, err
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE local_cleanup_jobs SET bucket=?, object_key=?, status='pending', requested_by=?, requested_at=?, error_message=NULL, completed_at=NULL WHERE id=?`, source.Bucket, source.ObjectKey, strings.TrimSpace(requestedBy), now, jobID); err != nil {
			return LocalCleanupJob{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE episodes SET local_storage_status='deleting', local_storage_delete_error=NULL WHERE id=?`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalCleanupJob{}, err
	}
	return s.GetCleanupJob(ctx, episodeID)
}

// GetCleanupJob returns the latest persisted cleanup job for an episode.
func (s *LocalCleanupService) GetCleanupJob(ctx context.Context, episodeID int64) (LocalCleanupJob, error) {
	var job LocalCleanupJob
	if err := s.db.GetContext(ctx, &job, `SELECT id, episode_id, bucket, object_key, status, requested_by, requested_at, started_at, completed_at, retry_count, error_message FROM local_cleanup_jobs WHERE episode_id = ?`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	return job, nil
}

// LocalCleanupService owns validation, auditing, and deletion of an episode's
// original MinIO source object. Callers need only supply its numeric ID.
type LocalCleanupService struct {
	db     *sqlx.DB
	store  LocalObjectDeleter
	bucket string
}

// NewLocalCleanupService creates the local cleanup module. A nil store makes
// cleanup unavailable instead of risking a database-only deletion marker.
func NewLocalCleanupService(db *sqlx.DB, store LocalObjectDeleter, bucket string) *LocalCleanupService {
	return &LocalCleanupService{db: db, store: store, bucket: strings.TrimSpace(bucket)}
}

// CleanupEpisode deletes the immutable MinIO source of a cloud-synced episode.
// It is idempotent: a previously deleted object remains a successful result.
func (s *LocalCleanupService) CleanupEpisode(ctx context.Context, episodeID int64, requestedBy string) (LocalCleanupResult, error) {
	if s == nil || s.db == nil || s.store == nil {
		return LocalCleanupResult{}, fmt.Errorf("local cleanup is not configured")
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return LocalCleanupResult{}, fmt.Errorf("begin local cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var episode struct {
		CloudSynced        bool   `db:"cloud_synced"`
		LocalStorageStatus string `db:"local_storage_status"`
	}
	if err := tx.GetContext(ctx, &episode, `
		SELECT cloud_synced, local_storage_status
		FROM episodes WHERE id = ? AND deleted_at IS NULL`, episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LocalCleanupResult{}, sql.ErrNoRows
		}
		return LocalCleanupResult{}, fmt.Errorf("load cleanup episode: %w", err)
	}
	if !episode.CloudSynced {
		return LocalCleanupResult{}, ErrLocalCleanupNotSynced
	}
	if episode.LocalStorageStatus == "deleted" {
		return LocalCleanupResult{EpisodeID: episodeID, Status: "deleted"}, nil
	}

	var active int
	if err := tx.GetContext(ctx, &active, `SELECT COUNT(*) FROM sync_logs WHERE episode_id = ? AND status IN ('pending', 'in_progress')`, episodeID); err != nil {
		return LocalCleanupResult{}, fmt.Errorf("check active cloud sync: %w", err)
	}
	if active != 0 {
		return LocalCleanupResult{}, ErrLocalCleanupSyncActive
	}

	var rawSnapshot sql.NullString
	if err := tx.GetContext(ctx, &rawSnapshot, `
		SELECT source_snapshot FROM sync_logs
		WHERE episode_id = ? AND status = 'completed'
		ORDER BY id DESC LIMIT 1`, episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) || !rawSnapshot.Valid {
			return LocalCleanupResult{}, ErrLocalCleanupSourceUnavailable
		}
		return LocalCleanupResult{}, fmt.Errorf("load cleanup source snapshot: %w", err)
	}
	var source SyncSourceSnapshot
	if err := json.Unmarshal([]byte(rawSnapshot.String), &source); err != nil || source.Backend != SyncBackendMinIO || source.SourceType != SyncSourceOriginal ||
		strings.TrimSpace(source.Bucket) == "" || strings.TrimSpace(source.ObjectKey) == "" {
		return LocalCleanupResult{}, ErrLocalCleanupUnsupportedSource
	}
	if s.bucket == "" || !strings.EqualFold(source.Bucket, s.bucket) {
		return LocalCleanupResult{}, ErrLocalCleanupUnsupportedSource
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE episodes SET local_storage_status = 'deleting', local_storage_delete_error = NULL
		WHERE id = ?`, episodeID); err != nil {
		return LocalCleanupResult{}, fmt.Errorf("mark local cleanup deleting: %w", err)
	}
	var jobID int64
	if err := tx.GetContext(ctx, &jobID, `SELECT id FROM local_cleanup_jobs WHERE episode_id = ?`, episodeID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return LocalCleanupResult{}, fmt.Errorf("load existing local cleanup job: %w", err)
		}
		insertResult, insertErr := tx.ExecContext(ctx, `
			INSERT INTO local_cleanup_jobs (episode_id, bucket, object_key, status, requested_by, requested_at, started_at, retry_count)
			VALUES (?, ?, ?, 'in_progress', ?, ?, ?, 1)`,
			episodeID, source.Bucket, source.ObjectKey, strings.TrimSpace(requestedBy), now, now)
		if insertErr != nil {
			return LocalCleanupResult{}, fmt.Errorf("create local cleanup job: %w", insertErr)
		}
		jobID, err = insertResult.LastInsertId()
		if err != nil {
			return LocalCleanupResult{}, fmt.Errorf("load new local cleanup job id: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `
		UPDATE local_cleanup_jobs
		SET bucket = ?, object_key = ?, status = 'in_progress', requested_by = ?, requested_at = ?, started_at = ?,
			retry_count = retry_count + 1, error_message = NULL, completed_at = NULL
		WHERE id = ?`, source.Bucket, source.ObjectKey, strings.TrimSpace(requestedBy), now, now, jobID); err != nil {
		return LocalCleanupResult{}, fmt.Errorf("restart local cleanup job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LocalCleanupResult{}, fmt.Errorf("commit local cleanup request: %w", err)
	}

	result := LocalCleanupResult{JobID: jobID, EpisodeID: episodeID, Bucket: source.Bucket, ObjectKey: source.ObjectKey, Status: "deleted"}
	if err := s.store.DeleteObject(ctx, source.Bucket, source.ObjectKey); err != nil {
		s.recordFailure(ctx, episodeID, jobID, err)
		return result, fmt.Errorf("delete local object: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE episodes SET local_storage_status = 'deleted', local_storage_deleted_at = ?, local_storage_delete_error = NULL
		WHERE id = ?`, time.Now().UTC(), episodeID); err != nil {
		return result, fmt.Errorf("mark local cleanup complete: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE local_cleanup_jobs SET status = 'completed', completed_at = ?, error_message = NULL WHERE id = ?`, time.Now().UTC(), jobID); err != nil {
		return result, fmt.Errorf("complete local cleanup job: %w", err)
	}
	return result, nil
}

func (s *LocalCleanupService) recordFailure(ctx context.Context, episodeID, jobID int64, cleanupErr error) {
	message := cleanupErr.Error()
	if _, err := s.db.ExecContext(ctx, `UPDATE episodes SET local_storage_status = 'delete_failed', local_storage_delete_error = ? WHERE id = ?`, message, episodeID); err != nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE local_cleanup_jobs SET status = 'failed', error_message = ?, completed_at = ? WHERE id = ?`, message, time.Now().UTC(), jobID)
}
