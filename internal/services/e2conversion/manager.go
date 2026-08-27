// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package e2conversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"github.com/jmoiron/sqlx"
)

var objectBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Orbit is the external execution seam used by the reconciler.
type Orbit interface {
	Submit(ctx context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error)
	Get(ctx context.Context, id string) (orbitapi.Job, error)
	Logs(ctx context.Context, id string) (string, error)
	Stop(ctx context.Context, id string) (orbitapi.Job, error)
	Delete(ctx context.Context, id string) error
}

// ObjectStore is the external object identity and content seam used by the
// reconciler. Its methods are added as lifecycle behavior is implemented.
type ObjectStore interface {
	StatObject(ctx context.Context, bucket, objectName string) (size int64, etag string, err error)
	OpenObject(ctx context.Context, bucket, objectName string) (io.ReadCloser, error)
	OpenObjectRange(ctx context.Context, bucket, objectName string, offset, length, totalSize int64, etag string) (io.ReadCloser, error)
}

// Manager exposes the small business interface for the durable lifecycle.
type Manager struct {
	db      *sqlx.DB
	orbit   Orbit
	objects ObjectStore
	cfg     Config
	now     func() time.Time

	runnerMu     sync.Mutex
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}
	wake         chan struct{}
	dispatchMu   sync.RWMutex

	verificationMu     sync.Mutex
	verificationCancel context.CancelFunc
	verificationDone   chan struct{}
	verificationClaim  sync.Mutex
}

// NewManager constructs the e2-multimodal-conversion module.
func NewManager(db *sqlx.DB, orbit Orbit, objects ObjectStore, cfg Config) *Manager {
	return &Manager{
		db:      db,
		orbit:   orbit,
		objects: objects,
		cfg:     cfg,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
	}
}

type episodeAdmissionRow struct {
	ID                 int64          `db:"id"`
	IngestionChannel   string         `db:"ingestion_channel"`
	StorageBackend     string         `db:"storage_backend"`
	McapPath           string         `db:"mcap_path"`
	Metadata           sql.NullString `db:"metadata"`
	QAStatus           string         `db:"qa_status"`
	DeviceType         string         `db:"device_type"`
	CloudPublishSource sql.NullString `db:"cloud_publish_source"`
}

type sourceMetadata struct {
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"object_key"`
}

// Start creates generation 1 or returns the existing active derivative. The
// Episode row is locked so original sync and processing cannot both win.
func (m *Manager) Start(ctx context.Context, episodeID int64, actor string) (Derivative, bool, error) {
	if m == nil || m.db == nil {
		return Derivative{}, false, fmt.Errorf("start E2 conversion: database is not configured")
	}
	if !m.cfg.Enabled {
		return Derivative{}, false, ErrDisabled
	}
	if episodeID <= 0 {
		return Derivative{}, false, ErrEpisodeNotFound
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Derivative{}, false, fmt.Errorf("begin E2 conversion admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var episode episodeAdmissionRow
	query := `
		SELECT e.id, e.ingestion_channel, e.storage_backend, e.mcap_path, e.metadata,
		       COALESCE(e.qa_status, '') AS qa_status,
		       COALESCE(r.device_type, '') AS device_type,
		       e.cloud_publish_source
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL` + forUpdateClause(m.db)
	if err := tx.GetContext(ctx, &episode, query, episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, false, ErrEpisodeNotFound
		}
		return Derivative{}, false, fmt.Errorf("lock episode %d: %w", episodeID, err)
	}
	if err := validateE2EpisodeAdmission(episode); err != nil {
		return Derivative{}, false, err
	}
	if strings.EqualFold(strings.TrimSpace(episode.CloudPublishSource.String), CloudSourceOriginal) {
		return Derivative{}, false, ErrCloudSourceLocked
	}
	var syncEvidence int
	if err := tx.GetContext(ctx, &syncEvidence, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil {
		return Derivative{}, false, fmt.Errorf("check episode %d sync evidence: %w", episodeID, err)
	}
	if syncEvidence > 0 && !strings.EqualFold(strings.TrimSpace(episode.CloudPublishSource.String), CloudSourceE2Conversion) {
		return Derivative{}, false, ErrCloudSourceLocked
	}
	if _, _, err := normalizeEpisodeSource(episode); err != nil {
		return Derivative{}, false, err
	}

	existing, err := getDerivativeTx(ctx, tx, episodeID)
	if err == nil {
		switch existing.ProcessingStatus {
		case ProcessingSucceeded:
			return existing, false, ErrAlreadyDerived
		case ProcessingFailed, ProcessingCanceled:
			return existing, false, ErrRetryRequired
		default:
			if err := tx.Commit(); err != nil {
				return Derivative{}, false, fmt.Errorf("commit idempotent E2 conversion admission: %w", err)
			}
			m.wakeReconciler()
			return existing, false, nil
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Derivative{}, false, fmt.Errorf("load E2 conversion derivative: %w", err)
	}

	now := m.now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, orbit_delete_status,
			qa_status, created_by, updated_by, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
	`, episodeID, Kind, ProcessingQueued, DeleteNotRequired, QANotStarted,
		strings.TrimSpace(actor), strings.TrimSpace(actor), now, now)
	if err != nil {
		return Derivative{}, false, fmt.Errorf("insert E2 conversion derivative: %w", err)
	}
	derivativeID, err := result.LastInsertId()
	if err != nil {
		return Derivative{}, false, fmt.Errorf("read E2 conversion derivative id: %w", err)
	}
	created, err := getDerivativeByIDTx(ctx, tx, derivativeID)
	if err != nil {
		return Derivative{}, false, fmt.Errorf("load created E2 conversion derivative: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Derivative{}, false, fmt.Errorf("commit E2 conversion admission: %w", err)
	}
	m.wakeReconciler()
	return created, true, nil
}

// Retry replaces a failed/canceled execution snapshot with a fresh queued
// generation after the Orbit API delete phase has completed.
func (m *Manager) Retry(ctx context.Context, episodeID int64, actor string) (Derivative, error) {
	if m == nil || m.db == nil {
		return Derivative{}, fmt.Errorf("retry E2 conversion: database is not configured")
	}
	if !m.cfg.Enabled {
		return Derivative{}, ErrDisabled
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Derivative{}, fmt.Errorf("begin E2 conversion retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var episode episodeAdmissionRow
	if err := tx.GetContext(ctx, &episode, `
		SELECT e.id, e.ingestion_channel, e.storage_backend, e.mcap_path, e.metadata,
		       COALESCE(e.qa_status, '') AS qa_status,
		       COALESCE(r.device_type, '') AS device_type,
		       e.cloud_publish_source
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL`+forUpdateClause(m.db), episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, ErrEpisodeNotFound
		}
		return Derivative{}, fmt.Errorf("lock retry episode: %w", err)
	}
	if err := validateE2EpisodeAdmission(episode); err != nil {
		return Derivative{}, err
	}
	if strings.EqualFold(strings.TrimSpace(episode.CloudPublishSource.String), CloudSourceOriginal) {
		return Derivative{}, ErrCloudSourceLocked
	}
	if _, _, err := normalizeEpisodeSource(episode); err != nil {
		return Derivative{}, err
	}
	current, err := getDerivativeTx(ctx, tx, episodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, ErrNotFound
		}
		return Derivative{}, fmt.Errorf("load retry derivative: %w", err)
	}
	if current.ProcessingStatus == ProcessingSucceeded {
		return Derivative{}, ErrAlreadyDerived
	}
	if current.ProcessingStatus != ProcessingFailed && current.ProcessingStatus != ProcessingCanceled {
		return Derivative{}, ErrProcessingActive
	}
	if current.OrbitDeleteStatus != DeleteCompleted && current.OrbitDeleteStatus != DeleteNotRequired {
		return Derivative{}, ErrCleanupPending
	}
	now := m.now().UTC()
	if err := resetDerivativeGenerationTx(ctx, tx, current.ID, actor, now); err != nil {
		return Derivative{}, err
	}
	retried, err := getDerivativeByIDTx(ctx, tx, current.ID)
	if err != nil {
		return Derivative{}, fmt.Errorf("load retried E2 conversion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Derivative{}, fmt.Errorf("commit E2 conversion retry: %w", err)
	}
	m.wakeReconciler()
	return retried, nil
}

// Cancel persists the desired cancellation. Queued work ends immediately;
// active work is stopped by the reconciler so a concurrent submit cannot race
// a one-shot handler call.
func (m *Manager) Cancel(ctx context.Context, episodeID int64, actor string) (Derivative, error) {
	if m == nil || m.db == nil {
		return Derivative{}, fmt.Errorf("cancel E2 conversion: database is not configured")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Derivative{}, fmt.Errorf("begin E2 conversion cancel: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := getDerivativeTx(ctx, tx, episodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, ErrNotFound
		}
		return Derivative{}, fmt.Errorf("load cancel derivative: %w", err)
	}
	now := m.now().UTC()
	switch current.ProcessingStatus {
	case ProcessingQueued:
		_, err = tx.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, processing_finished_at = ?,
			    orbit_delete_status = ?, updated_by = NULLIF(?, ''), updated_at = ?
			WHERE id = ? AND processing_status = ?
		`, ProcessingCanceled, now, DeleteNotRequired, strings.TrimSpace(actor), now, current.ID, ProcessingQueued)
	case ProcessingSubmitting, ProcessingPending, ProcessingRunning:
		_, err = tx.ExecContext(ctx, `
			UPDATE episode_derivatives SET cancel_requested_at = COALESCE(cancel_requested_at, ?),
			    reconcile_after = NULL, updated_by = NULLIF(?, ''), updated_at = ?
			WHERE id = ?
		`, now, strings.TrimSpace(actor), now, current.ID)
	case ProcessingFailed, ProcessingCanceled:
		// Idempotent terminal cancellation.
	case ProcessingVerifying, ProcessingSucceeded:
		return Derivative{}, ErrAlreadyDerived
	default:
		return Derivative{}, fmt.Errorf("cancel E2 conversion with unknown status %q", current.ProcessingStatus)
	}
	if err != nil {
		return Derivative{}, fmt.Errorf("persist E2 conversion cancellation: %w", err)
	}
	updated, err := getDerivativeByIDTx(ctx, tx, current.ID)
	if err != nil {
		return Derivative{}, fmt.Errorf("load canceled E2 conversion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Derivative{}, fmt.Errorf("commit E2 conversion cancellation: %w", err)
	}
	m.wakeReconciler()
	return updated, nil
}

// RetryQA requeues only the fixed QA phase. A successful processing result is
// immutable and is never regenerated because QA failed.
func (m *Manager) RetryQA(ctx context.Context, episodeID int64, actor string) (Derivative, error) {
	if m == nil || m.db == nil {
		return Derivative{}, fmt.Errorf("retry E2 conversion QA: database is not configured")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Derivative{}, fmt.Errorf("begin E2 conversion QA retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := getDerivativeTx(ctx, tx, episodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, ErrNotFound
		}
		return Derivative{}, fmt.Errorf("load E2 conversion QA retry: %w", err)
	}
	if current.ProcessingStatus != ProcessingSucceeded {
		return Derivative{}, ErrQAUnavailable
	}
	if current.QAStatus == QAApproved || current.QAStatus == QAPending || current.QAStatus == QARunning {
		if err := tx.Commit(); err != nil {
			return Derivative{}, fmt.Errorf("commit idempotent E2 conversion QA retry: %w", err)
		}
		return current, nil
	}
	if current.QAStatus != QAFailed {
		return Derivative{}, ErrQAUnavailable
	}
	var syncEvidence int
	if err := tx.GetContext(ctx, &syncEvidence, `
		SELECT COUNT(*) FROM sync_logs
		WHERE episode_id = ? AND status IN ('pending', 'in_progress', 'completed')
	`, episodeID); err != nil {
		return Derivative{}, fmt.Errorf("check E2 conversion QA sync evidence: %w", err)
	}
	if syncEvidence > 0 {
		return Derivative{}, ErrCloudSyncActive
	}
	now := m.now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE episode_derivatives SET qa_status = ?, qa_attempt_count = 0,
		    qa_next_retry_at = NULL, qa_score = NULL, quality_flag = NULL,
		    qa_result = NULL, qa_error = NULL, qa_started_at = NULL,
		    qa_finished_at = NULL, reconcile_after = NULL,
		    updated_by = NULLIF(?, ''), updated_at = ?
		WHERE id = ? AND processing_status = ? AND qa_status = ?
	`, QAPending, strings.TrimSpace(actor), now, current.ID, ProcessingSucceeded, QAFailed); err != nil {
		return Derivative{}, fmt.Errorf("persist E2 conversion QA retry: %w", err)
	}
	updated, err := getDerivativeByIDTx(ctx, tx, current.ID)
	if err != nil {
		return Derivative{}, fmt.Errorf("load E2 conversion QA retry result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Derivative{}, fmt.Errorf("commit E2 conversion QA retry: %w", err)
	}
	m.wakeReconciler()
	return updated, nil
}

// Logs returns live Orbit logs while a Job exists and otherwise the persisted
// tail captured at terminal state.
func (m *Manager) Logs(ctx context.Context, episodeID int64) (string, error) {
	derivative, err := m.Get(ctx, episodeID)
	if err != nil {
		return "", err
	}
	if m.orbit != nil && strings.TrimSpace(derivative.OrbitJobID) != "" {
		logs, err := m.orbit.Logs(ctx, derivative.OrbitJobID)
		if err == nil {
			if limit := m.cfg.LogTailBytes; limit > 0 && len(logs) > limit {
				logs = logs[len(logs)-limit:]
			}
			return logs, nil
		}
		if derivative.OrbitLogTail == "" {
			return "", fmt.Errorf("load Orbit logs: %w", err)
		}
	}
	return derivative.OrbitLogTail, nil
}

func validateE2EpisodeAdmission(episode episodeAdmissionRow) error {
	if strings.TrimSpace(episode.DeviceType) != "Ego Portal E2" {
		return fmt.Errorf("%w: device type %q is not Ego Portal E2", ErrSourceUnavailable, episode.DeviceType)
	}
	if strings.TrimSpace(episode.IngestionChannel) != "data_gateway" ||
		strings.TrimSpace(episode.StorageBackend) != "keystone_tos" {
		return fmt.Errorf("%w: episode provenance must be data_gateway/keystone_tos", ErrSourceUnavailable)
	}
	if !strings.EqualFold(path.Ext(strings.TrimSpace(episode.McapPath)), ".tar") {
		return fmt.Errorf("%w: episode source is not a tar object", ErrSourceUnavailable)
	}
	if strings.TrimSpace(episode.QAStatus) != QAApproved {
		return fmt.Errorf("%w: episode QA status %q is not approved", ErrQANotApproved, episode.QAStatus)
	}
	return nil
}
func normalizeEpisodeSource(episode episodeAdmissionRow) (string, string, error) {
	if !strings.EqualFold(strings.TrimSpace(episode.StorageBackend), "keystone_tos") {
		return "", "", fmt.Errorf("%w: storage backend %q is not TOS", ErrSourceUnavailable, episode.StorageBackend)
	}
	var metadata sourceMetadata
	if !episode.Metadata.Valid || json.Unmarshal([]byte(episode.Metadata.String), &metadata) != nil {
		return "", "", fmt.Errorf("%w: missing TOS metadata", ErrSourceUnavailable)
	}
	bucket := strings.ToLower(strings.TrimSpace(metadata.Bucket))
	objectKey := strings.TrimSpace(metadata.ObjectKey)
	if !objectBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return "", "", fmt.Errorf("%w: invalid TOS bucket", ErrSourceUnavailable)
	}
	cleaned := path.Clean("/" + objectKey)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if objectKey == "" || cleaned == "." || cleaned != strings.TrimPrefix(objectKey, "/") || strings.HasSuffix(cleaned, "/") {
		return "", "", fmt.Errorf("%w: invalid TOS object key", ErrSourceUnavailable)
	}
	return bucket, cleaned, nil
}

// ValidateSource reports whether Episode storage metadata can be normalized
// into the TOS source URI required by the fixed processor.
func ValidateSource(storageBackend, metadata string) error {
	_, _, err := normalizeEpisodeSource(episodeAdmissionRow{
		StorageBackend: storageBackend,
		Metadata: sql.NullString{
			String: metadata,
			Valid:  strings.TrimSpace(metadata) != "",
		},
	})
	return err
}

func forUpdateClause(db *sqlx.DB) string {
	if db != nil && db.DriverName() == "mysql" {
		return " FOR UPDATE"
	}
	return ""
}

const derivativeSelect = `
	SELECT id, episode_id, kind, generation,
	       processor_config_revision_id,
	       COALESCE(processor_image, '') AS processor_image,
	       COALESCE(source_uri, '') AS source_uri,
	       COALESCE(source_etag, '') AS source_etag,
	       COALESCE(source_checksum, '') AS source_checksum,
	       source_size_bytes, processing_status, cancel_requested_at,
	       COALESCE(orbit_submission_id, '') AS orbit_submission_id,
	       COALESCE(orbit_job_id, '') AS orbit_job_id,
	       COALESCE(output_prefix, '') AS output_prefix,
	       COALESCE(mcap_path, '') AS mcap_path,
	       COALESCE(metadata_path, '') AS metadata_path,
	       COALESCE(manifest_path, '') AS manifest_path,
	       COALESCE(checksum, '') AS checksum, file_size_bytes, duration_sec,
	       processing_duration_sec,
	       COALESCE(processing_error, '') AS processing_error,
	       COALESCE(orbit_log_tail, '') AS orbit_log_tail,
	       orbit_delete_status,
	       COALESCE(orbit_delete_error, '') AS orbit_delete_error,
	       qa_status, qa_score,
	       COALESCE(quality_flag, '') AS quality_flag,
	       COALESCE(qa_error, '') AS qa_error,
	       created_at, updated_at
	FROM episode_derivatives`

func getDerivativeTx(ctx context.Context, tx *sqlx.Tx, episodeID int64) (Derivative, error) {
	var derivative Derivative
	err := tx.GetContext(ctx, &derivative, derivativeSelect+" WHERE episode_id = ? AND kind = ?", episodeID, Kind)
	return derivative, err
}

func getDerivativeByIDTx(ctx context.Context, tx *sqlx.Tx, derivativeID int64) (Derivative, error) {
	var derivative Derivative
	err := tx.GetContext(ctx, &derivative, derivativeSelect+" WHERE id = ?", derivativeID)
	return derivative, err
}
