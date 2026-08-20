// SPDX-FileCopyrightText: 2026 ArcheBase
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
	ErrLocalCleanupNotSynced         = errors.New("episode is not cloud-synced")
	ErrLocalCleanupSyncActive        = errors.New("episode cloud sync is active")
	ErrLocalCleanupUnsupportedSource = errors.New("episode source is not local MinIO")
	ErrLocalCleanupSourceUnavailable = errors.New("local source object identity is unavailable")
)

type LocalObjectDeleter interface {
	DeleteObject(ctx context.Context, bucket, objectKey string) error
}

type LocalCleanupResult struct {
	JobID     int64  `json:"cleanup_job_id"`
	EpisodeID int64  `json:"episode_id"`
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"object_key"`
	Status    string `json:"status"`
}

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

type cleanupObject struct {
	Bucket string `db:"bucket"`
	Key    string `db:"object_key"`
}

type LocalCleanupService struct {
	db     *sqlx.DB
	store  LocalObjectDeleter
	bucket string
}

func NewLocalCleanupService(db *sqlx.DB, store LocalObjectDeleter, bucket string) *LocalCleanupService {
	return &LocalCleanupService{db: db, store: store, bucket: strings.TrimSpace(bucket)}
}

type LocalCleanupWorker struct {
	service  *LocalCleanupService
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

func NewLocalCleanupWorker(service *LocalCleanupService, interval time.Duration) *LocalCleanupWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &LocalCleanupWorker{service: service, interval: interval}
}

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
	var jobs []LocalCleanupJob
	if err := w.service.db.SelectContext(ctx, &jobs, `SELECT id, episode_id, bucket, object_key, status, requested_by, requested_at, started_at, completed_at, retry_count, error_message FROM local_cleanup_jobs WHERE status IN ('pending', 'in_progress', 'failed') ORDER BY requested_at, id LIMIT 50`); err != nil {
		return
	}
	for _, job := range jobs {
		_ = w.service.processJob(ctx, job)
	}
}

func parseMinIOURI(raw string) (string, string, bool) {
	value := strings.TrimSpace(raw)
	if !strings.HasPrefix(value, "minio://") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "minio://"), "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], strings.TrimLeft(parts[1], "/"), true
}

func (s *LocalCleanupService) cleanupObjects(ctx context.Context, tx *sqlx.Tx, episodeID int64, source SyncSourceSnapshot) ([]cleanupObject, error) {
	if source.Backend != SyncBackendMinIO || s.bucket == "" || !strings.EqualFold(source.Bucket, s.bucket) || strings.TrimSpace(source.ObjectKey) == "" {
		return nil, ErrLocalCleanupUnsupportedSource
	}
	if source.SourceType == SyncSourceOriginal {
		return []cleanupObject{{Bucket: source.Bucket, Key: source.ObjectKey}}, nil
	}
	if source.SourceType != SyncSourceDepthNormalization {
		return nil, ErrLocalCleanupUnsupportedSource
	}
	var derivative struct {
		Generation       int            `db:"generation"`
		SourceURI        sql.NullString `db:"source_uri"`
		McapPath         sql.NullString `db:"mcap_path"`
		ProcessingStatus string         `db:"processing_status"`
		QAStatus         string         `db:"qa_status"`
	}
	if err := tx.GetContext(ctx, &derivative, `SELECT generation, source_uri, mcap_path, processing_status, qa_status FROM episode_derivatives WHERE id=? AND episode_id=? AND kind='depth_normalization'`, source.DerivativeID, episodeID); err != nil {
		return nil, ErrLocalCleanupSourceUnavailable
	}
	if derivative.Generation != source.Generation || derivative.ProcessingStatus != "succeeded" || derivative.QAStatus != "approved" || !derivative.McapPath.Valid || derivative.McapPath.String != source.ObjectKey {
		return nil, ErrLocalCleanupSourceUnavailable
	}
	bucket, key, ok := parseMinIOURI(derivative.SourceURI.String)
	if !ok || !strings.EqualFold(bucket, s.bucket) {
		return nil, ErrLocalCleanupUnsupportedSource
	}
	return []cleanupObject{{Bucket: bucket, Key: key}, {Bucket: source.Bucket, Key: source.ObjectKey}}, nil
}

func (s *LocalCleanupService) sourceAndObjects(ctx context.Context, tx *sqlx.Tx, episodeID int64) ([]cleanupObject, error) {
	var raw sql.NullString
	if err := tx.GetContext(ctx, &raw, `SELECT source_snapshot FROM sync_logs WHERE episode_id=? AND status='completed' ORDER BY id DESC LIMIT 1`, episodeID); err != nil || !raw.Valid {
		return nil, ErrLocalCleanupSourceUnavailable
	}
	var source SyncSourceSnapshot
	if json.Unmarshal([]byte(raw.String), &source) != nil {
		return nil, ErrLocalCleanupUnsupportedSource
	}
	return s.cleanupObjects(ctx, tx, episodeID, source)
}

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
	if err := tx.GetContext(ctx, &episode, `SELECT cloud_synced, local_storage_status FROM episodes WHERE id=? AND deleted_at IS NULL`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	if !episode.CloudSynced {
		return LocalCleanupJob{}, ErrLocalCleanupNotSynced
	}
	if episode.LocalStorageStatus == "deleted" {
		return s.GetCleanupJob(ctx, episodeID)
	}
	var active int
	if err := tx.GetContext(ctx, &active, `SELECT COUNT(*) FROM sync_logs WHERE episode_id=? AND status IN ('pending','in_progress')`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	if active != 0 {
		return LocalCleanupJob{}, ErrLocalCleanupSyncActive
	}
	objects, err := s.sourceAndObjects(ctx, tx, episodeID)
	if err != nil {
		return LocalCleanupJob{}, err
	}
	now := time.Now().UTC()
	var jobID int64
	if err := tx.GetContext(ctx, &jobID, `SELECT id FROM local_cleanup_jobs WHERE episode_id=?`, episodeID); errors.Is(err, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `INSERT INTO local_cleanup_jobs (episode_id,bucket,object_key,status,requested_by,requested_at,retry_count) VALUES (?,?,?,'pending',?,?,0)`, episodeID, objects[0].Bucket, objects[0].Key, strings.TrimSpace(requestedBy), now)
		if err != nil {
			return LocalCleanupJob{}, err
		}
		jobID, err = result.LastInsertId()
		if err != nil {
			return LocalCleanupJob{}, err
		}
	} else if err != nil {
		return LocalCleanupJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_cleanup_job_objects WHERE job_id=?`, jobID); err != nil {
		return LocalCleanupJob{}, err
	}
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_cleanup_job_objects (job_id,bucket,object_key,status) VALUES (?,?,?,'pending')`, jobID, object.Bucket, object.Key); err != nil {
			return LocalCleanupJob{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_cleanup_jobs SET bucket=?,object_key=?,status='pending',requested_by=?,requested_at=?,error_message=NULL,completed_at=NULL WHERE id=?`, objects[0].Bucket, objects[0].Key, strings.TrimSpace(requestedBy), now, jobID); err != nil {
		return LocalCleanupJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE episodes SET local_storage_status='deleting',local_storage_delete_error=NULL WHERE id=?`, episodeID); err != nil {
		return LocalCleanupJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalCleanupJob{}, err
	}
	return s.GetCleanupJob(ctx, episodeID)
}

func (s *LocalCleanupService) GetCleanupJob(ctx context.Context, episodeID int64) (LocalCleanupJob, error) {
	var job LocalCleanupJob
	err := s.db.GetContext(ctx, &job, `SELECT id,episode_id,bucket,object_key,status,requested_by,requested_at,started_at,completed_at,retry_count,error_message FROM local_cleanup_jobs WHERE episode_id=?`, episodeID)
	return job, err
}

func (s *LocalCleanupService) processJob(ctx context.Context, job LocalCleanupJob) error {
	var objects []cleanupObject
	if err := s.db.SelectContext(ctx, &objects, `SELECT bucket,object_key FROM local_cleanup_job_objects WHERE job_id=? AND status<>'completed' ORDER BY id`, job.JobID); err != nil {
		return err
	}
	if len(objects) == 0 {
		objects = []cleanupObject{{Bucket: job.Bucket, Key: job.ObjectKey}}
	}
	for _, object := range objects {
		if err := s.store.DeleteObject(ctx, object.Bucket, object.Key); err != nil {
			return s.recordFailure(ctx, job, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE local_cleanup_job_objects SET status='completed',error_message=NULL WHERE job_id=? AND bucket=? AND object_key=?`, job.JobID, object.Bucket, object.Key); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE episodes SET local_storage_status='deleted',local_storage_deleted_at=?,local_storage_delete_error=NULL WHERE id=?`, now, job.EpisodeID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE local_cleanup_jobs SET status='completed',completed_at=?,error_message=NULL WHERE id=?`, now, job.JobID)
	return err
}

func (s *LocalCleanupService) CleanupEpisode(ctx context.Context, episodeID int64, requestedBy string) (LocalCleanupResult, error) {
	job, err := s.RequestCleanupEpisode(ctx, episodeID, requestedBy)
	if err != nil {
		return LocalCleanupResult{}, err
	}
	if err := s.processJob(ctx, job); err != nil {
		return LocalCleanupResult{JobID: job.JobID, EpisodeID: episodeID, Bucket: job.Bucket, ObjectKey: job.ObjectKey, Status: "failed"}, err
	}
	return LocalCleanupResult{JobID: job.JobID, EpisodeID: episodeID, Bucket: job.Bucket, ObjectKey: job.ObjectKey, Status: "deleted"}, nil
}

func (s *LocalCleanupService) recordFailure(ctx context.Context, job LocalCleanupJob, cleanupErr error) error {
	message := cleanupErr.Error()
	_, _ = s.db.ExecContext(ctx, `UPDATE episodes SET local_storage_status='delete_failed',local_storage_delete_error=? WHERE id=?`, message, job.EpisodeID)
	_, _ = s.db.ExecContext(ctx, `UPDATE local_cleanup_jobs SET status='failed',error_message=?,completed_at=? WHERE id=?`, message, time.Now().UTC(), job.JobID)
	return cleanupErr
}
