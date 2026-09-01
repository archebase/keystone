// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"github.com/foxglove/mcap/go/mcap"
)

const (
	processingCommand       = "/app/run_processing.py"
	manifestName            = "processing_manifest.json"
	outputMcapName          = "output_bag.mcap"
	outputMetadataName      = "metadata.yaml"
	maxManifestBytes        = 1024 * 1024
	maxVerificationAttempts = 5
	manifestSchemaV1        = 1
	manifestSchemaV2        = 2
	manifestSchemaV3        = 3
	stereoH264OutputFormat  = "stereo_h264"
	calibrationAttachment   = "calibration.json"
	calibrationMediaType    = "application/json"

	leftImageTopic  = "/decxin/left_rgb/compressed"
	rightImageTopic = "/decxin/right_rgb/compressed"
	leftVideoTopic  = "/decxin/left_rgb/h264"
	rightVideoTopic = "/decxin/right_rgb/h264"
	imuTopic        = "/decxin/imu"

	compressedImageSchema = "sensor_msgs/msg/CompressedImage"
	compressedVideoSchema = "foxglove.CompressedVideo"
	imuSchema             = "sensor_msgs/msg/Imu"
)

var mcapMagic = []byte{0x89, 'M', 'C', 'A', 'P', '0', '\r', '\n'}

type reconcileEpisodeRow struct {
	ID                      int64          `db:"id"`
	StorageBackend          string         `db:"storage_backend"`
	McapPath                string         `db:"mcap_path"`
	Checksum                sql.NullString `db:"checksum"`
	Metadata                sql.NullString `db:"metadata"`
	CloudPublishSource      sql.NullString `db:"cloud_publish_source"`
	CameraSerial            sql.NullString `db:"camera_serial"`
	CalibrationCaptureID    sql.NullString `db:"calibration_capture_id"`
	CalibrationResultSHA256 sql.NullString `db:"calibration_result_sha256"`
}

type calibrationResultRow struct {
	SessionID    string `db:"calibration_session_id"`
	CameraSerial string `db:"camera_serial"`
	CaptureID    string `db:"capture_id"`
	Status       string `db:"status"`
	Bucket       string `db:"bucket"`
	ResultKey    string `db:"result_object_key"`
	ResultSize   int64  `db:"result_size_bytes"`
	ResultSHA256 string `db:"result_checksum_sha256"`
}

type frozenDerivativeRow struct {
	ID                   int64          `db:"id"`
	EpisodeID            int64          `db:"episode_id"`
	Generation           int            `db:"generation"`
	ProcessingStatus     string         `db:"processing_status"`
	CancelRequestedAt    sql.NullTime   `db:"cancel_requested_at"`
	OrbitSubmissionID    sql.NullString `db:"orbit_submission_id"`
	OrbitRequest         sql.NullString `db:"orbit_request"`
	OrbitJobID           sql.NullString `db:"orbit_job_id"`
	OrbitLogTail         sql.NullString `db:"orbit_log_tail"`
	SubmitAttemptCount   int            `db:"submit_attempt_count"`
	OrbitSubmitAbsentAt  sql.NullTime   `db:"orbit_submit_absent_at"`
	OrbitJobMissingSince sql.NullTime   `db:"orbit_job_missing_since"`
	OrbitDeleteStatus    string         `db:"orbit_delete_status"`
	QAStatus             string         `db:"qa_status"`
}

// Get returns the current stereo-split record for an Episode.
func (m *Manager) Get(ctx context.Context, episodeID int64) (Derivative, error) {
	if m == nil || m.db == nil {
		return Derivative{}, fmt.Errorf("get stereo split: database is not configured")
	}
	var derivative Derivative
	if err := m.db.GetContext(ctx, &derivative, derivativeSelect+" WHERE episode_id = ? AND kind = ?", episodeID, Kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Derivative{}, ErrNotFound
		}
		return Derivative{}, fmt.Errorf("get stereo split derivative: %w", err)
	}
	var payloads struct {
		ProcessingResult sql.NullString `db:"processing_result"`
		QAResult         sql.NullString `db:"qa_result"`
	}
	if err := m.db.GetContext(ctx, &payloads, `
		SELECT processing_result, qa_result
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivative.ID, Kind); err != nil {
		return Derivative{}, fmt.Errorf("get stereo split result payloads: %w", err)
	}
	if payloads.ProcessingResult.Valid && strings.TrimSpace(payloads.ProcessingResult.String) != "" {
		if err := json.Unmarshal([]byte(payloads.ProcessingResult.String), &derivative.ProcessingResult); err != nil {
			return Derivative{}, fmt.Errorf("decode stereo split processing result: %w", err)
		}
	}
	if payloads.QAResult.Valid && strings.TrimSpace(payloads.QAResult.String) != "" {
		if err := json.Unmarshal([]byte(payloads.QAResult.String), &derivative.QAResult); err != nil {
			return Derivative{}, fmt.Errorf("decode stereo split QA result: %w", err)
		}
	}
	derivative.OutputBucket = strings.TrimSpace(m.cfg.OutputBucket)
	return derivative, nil
}

// ReconcileOnce advances at most one durable record. It returns false when no
// record is due, allowing the background loop to sleep without busy polling.
func (m *Manager) ReconcileOnce(ctx context.Context) (bool, error) {
	if m == nil || m.db == nil {
		return false, fmt.Errorf("reconcile stereo split: database is not configured")
	}
	if !m.cfg.Enabled {
		return false, nil
	}
	preferQueued, err := m.shouldPreferQueuedDispatch(ctx)
	if err != nil {
		return true, err
	}
	verificationWorkersActive := m.verificationWorkersActive()
	statusSyncWorkersActive := m.statusSyncWorkersActive()
	dispatchWorkersActive := m.dispatchWorkersActive()
	var candidate frozenDerivativeRow
	err = m.db.GetContext(ctx, &candidate, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives
		WHERE kind = ?
		  AND (processing_status <> ? OR ? = 0)
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
		  AND NOT (? = 1 AND cancel_requested_at IS NULL AND processing_status IN (?, ?))
		  AND NOT (? = 1 AND cancel_requested_at IS NULL AND processing_status IN (?, ?))
		  AND (
		    processing_status IN (?, ?, ?, ?, ?)
		    OR (processing_status = ? AND qa_status IN (?, ?))
		    OR orbit_delete_status = ?
		  )
		ORDER BY CASE
		  WHEN cancel_requested_at IS NOT NULL
		       AND processing_status IN ('submitting', 'pending', 'running') THEN 0
		  WHEN ? = 1 AND processing_status = 'queued' THEN 1
		  WHEN processing_status IN ('submitting', 'pending', 'running', 'verifying') THEN CASE processing_status
		    WHEN 'submitting' THEN 2
		    WHEN 'pending' THEN 3
		    WHEN 'running' THEN 3
		    WHEN 'verifying' THEN 4
		    ELSE 5 END
		  WHEN processing_status = 'succeeded' AND qa_status IN ('pending', 'running') THEN 5
		  WHEN orbit_delete_status = 'pending' THEN 6
		  ELSE 7 END,
		  updated_at ASC, id ASC
		LIMIT 1
	`, Kind, ProcessingVerifying, boolInt(verificationWorkersActive), m.now().UTC(), boolInt(statusSyncWorkersActive), ProcessingPending, ProcessingRunning, boolInt(dispatchWorkersActive), ProcessingQueued, ProcessingSubmitting, ProcessingQueued, ProcessingSubmitting, ProcessingPending, ProcessingRunning, ProcessingVerifying,
		ProcessingSucceeded, QAPending, QARunning, DeletePending, boolInt(preferQueued))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("select stereo split reconcile candidate: %w", err)
	}
	if candidate.CancelRequestedAt.Valid {
		switch candidate.ProcessingStatus {
		case ProcessingSubmitting, ProcessingPending, ProcessingRunning:
			return true, m.reconcileCancellation(ctx, candidate.ID)
		}
	}

	switch candidate.ProcessingStatus {
	case ProcessingQueued:
		m.dispatchMu.RLock()
		defer m.dispatchMu.RUnlock()
		atCapacity, err := m.atOrbitCapacity(ctx)
		if err != nil {
			return true, err
		}
		if atCapacity {
			if err := m.preflightQueuedScratchStorage(ctx, candidate); err != nil {
				if errors.Is(err, errScratchStorageExceeded) {
					return true, m.failBeforeSubmission(ctx, candidate.ID, err)
				}
				return true, err
			}
			return false, nil
		}
		if err := m.freezeQueued(ctx, candidate); err != nil {
			if errors.Is(err, errScratchStorageExceeded) {
				return true, m.failBeforeSubmission(ctx, candidate.ID, err)
			}
			return true, err
		}
		return true, m.reconcileSubmitting(ctx, candidate.ID)
	case ProcessingSubmitting:
		m.dispatchMu.RLock()
		defer m.dispatchMu.RUnlock()
		return true, m.reconcileSubmitting(ctx, candidate.ID)
	case ProcessingPending, ProcessingRunning:
		return true, m.reconcileOrbitStatus(ctx, candidate.ID)
	case ProcessingVerifying:
		return true, m.verifySucceeded(ctx, candidate.ID)
	case ProcessingSucceeded:
		if candidate.QAStatus == QAPending || candidate.QAStatus == QARunning {
			return true, m.reconcileQA(ctx, candidate.ID)
		}
		if candidate.OrbitDeleteStatus == DeletePending {
			return true, m.reconcileDelete(ctx, candidate.ID)
		}
		return true, nil
	case ProcessingFailed, ProcessingCanceled:
		if candidate.OrbitDeleteStatus == DeletePending {
			return true, m.reconcileDelete(ctx, candidate.ID)
		}
		return true, nil
	default:
		return true, fmt.Errorf("unsupported reconcile status %q", candidate.ProcessingStatus)
	}
}

func (m *Manager) preflightQueuedScratchStorage(ctx context.Context, candidate frozenDerivativeRow) error {
	if m.objects == nil {
		return fmt.Errorf("preflight stereo split scratch storage: TOS object reader is not configured")
	}
	var episode episodeAdmissionRow
	if err := m.db.GetContext(ctx, &episode, `
		SELECT id, storage_backend, mcap_path, metadata, cloud_publish_source
		FROM episodes WHERE id = ? AND deleted_at IS NULL
	`, candidate.EpisodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEpisodeNotFound
		}
		return fmt.Errorf("load stereo split source for scratch preflight: %w", err)
	}
	bucket, objectKey, err := normalizeEpisodeSource(episode)
	if err != nil {
		return err
	}
	sourceSize, _, err := m.objects.StatObject(ctx, bucket, objectKey)
	if err != nil {
		return fmt.Errorf("stat stereo split source for scratch preflight: %w", err)
	}
	if sourceSize <= 0 {
		return fmt.Errorf("%w: source size %d is invalid", ErrSourceUnavailable, sourceSize)
	}
	_, err = scratchStorageRequest(sourceSize)
	return err
}

func (m *Manager) failBeforeSubmission(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status = ?, processing_error = ?, processing_finished_at = ?,
		    orbit_delete_status = ?, reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ? AND cancel_requested_at IS NULL
	`, ProcessingFailed, cause.Error(), now, DeleteNotRequired, now, derivativeID, ProcessingQueued)
	if err != nil {
		return fmt.Errorf("persist stereo split pre-submission failure after %w: %w", cause, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read stereo split pre-submission failure result after %w: %w", cause, err)
	}
	if rows != 1 {
		return nil
	}
	return cause
}

type dispatchCapacity struct {
	Active    int
	Limit     int
	QueuedDue bool
}

func (m *Manager) dispatchWorkersActive() bool {
	m.dispatchRunMu.Lock()
	defer m.dispatchRunMu.Unlock()
	return m.dispatchCancel != nil
}

func (m *Manager) statusSyncWorkersActive() bool {
	m.statusSyncMu.Lock()
	defer m.statusSyncMu.Unlock()
	return m.statusSyncCancel != nil
}

func (m *Manager) verificationWorkersActive() bool {
	m.verificationMu.Lock()
	defer m.verificationMu.Unlock()
	return m.verificationCancel != nil
}
func (m *Manager) atOrbitCapacity(ctx context.Context) (bool, error) {
	capacity, err := m.loadDispatchCapacity(ctx, false)
	if errors.Is(err, ErrImageNotConfigured) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return capacity.Active >= capacity.Limit, nil
}

func (m *Manager) shouldPreferQueuedDispatch(ctx context.Context) (bool, error) {
	capacity, err := m.loadDispatchCapacity(ctx, true)
	if err != nil {
		return false, err
	}
	return capacity.QueuedDue && capacity.Active < capacity.Limit, nil
}

// loadDispatchCapacity centralizes the durable concurrency calculation. The
// in-process dispatch lock serializes the final capacity recheck and freeze;
// the deployment must remain single-replica until admission is guarded by a
// database-backed lease or transaction that spans Keystone replicas.
func (m *Manager) loadDispatchCapacity(ctx context.Context, includeQueued bool) (dispatchCapacity, error) {
	capacity := dispatchCapacity{}
	if includeQueued {
		var queuedDue int
		if err := m.db.GetContext(ctx, &queuedDue, `
			SELECT CASE WHEN EXISTS (
				SELECT 1 FROM episode_derivatives
				WHERE kind = ? AND processing_status = ?
				  AND (reconcile_after IS NULL OR reconcile_after <= ?)
			) THEN 1 ELSE 0 END
		`, Kind, ProcessingQueued, m.now().UTC()); err != nil {
			return dispatchCapacity{}, fmt.Errorf("check queued stereo split work: %w", err)
		}
		capacity.QueuedDue = queuedDue == 1
		if !capacity.QueuedDue {
			return capacity, nil
		}
	}
	current, err := m.CurrentImageConfig(ctx)
	if err != nil {
		return dispatchCapacity{}, fmt.Errorf("load stereo split concurrency setting: %w", err)
	}
	capacity.Limit = configuredMaxConcurrent(current.MaxConcurrent)
	if err := m.db.GetContext(ctx, &capacity.Active, `
		SELECT COUNT(*) FROM episode_derivatives
		WHERE kind = ? AND processing_status IN (?, ?, ?)
	`, Kind, ProcessingSubmitting, ProcessingPending, ProcessingRunning); err != nil {
		return dispatchCapacity{}, fmt.Errorf("count active stereo split jobs: %w", err)
	}
	if !includeQueued {
		return capacity, nil
	}
	return capacity, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (m *Manager) cancelRequested(ctx context.Context, derivativeID int64) (bool, error) {
	var requested int
	if err := m.db.GetContext(ctx, &requested, `
		SELECT CASE WHEN cancel_requested_at IS NULL THEN 0 ELSE 1 END
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return false, fmt.Errorf("check stereo split cancellation: %w", err)
	}
	return requested == 1, nil
}

func (m *Manager) reconcileCancellationIfRequested(ctx context.Context, derivativeID int64) error {
	requested, err := m.cancelRequested(ctx, derivativeID)
	if err != nil || !requested {
		return err
	}
	return m.reconcileCancellation(ctx, derivativeID)
}

func (m *Manager) reconcileCancellation(ctx context.Context, derivativeID int64) error {
	var row frozenDerivativeRow
	if err := m.db.GetContext(ctx, &row, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id, orbit_log_tail,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load stereo split cancellation: %w", err)
	}
	if !row.CancelRequestedAt.Valid {
		return nil
	}
	switch row.ProcessingStatus {
	case ProcessingSubmitting, ProcessingPending, ProcessingRunning:
	default:
		return nil
	}

	lookupID := strings.TrimSpace(row.OrbitJobID.String)
	if lookupID == "" {
		lookupID = strings.TrimSpace(row.OrbitSubmissionID.String)
	}
	if lookupID == "" {
		return m.failInvariant(ctx, derivativeID, "canceled submitting derivative has no Orbit identity")
	}
	job, err := m.orbit.Get(ctx, lookupID)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			confirmed, confirmErr := m.confirmCanceledSubmissionAbsent(ctx, row)
			if confirmErr != nil || !confirmed {
				return confirmErr
			}
			return m.persistCanceledMissingJob(ctx, row)
		}
		return m.deferCancellation(ctx, derivativeID, fmt.Errorf("query Orbit Job for cancellation: %w", err))
	}
	if row.OrbitRequest.Valid && strings.TrimSpace(row.OrbitRequest.String) != "" {
		var request orbitapi.SubmitRequest
		if err := json.Unmarshal([]byte(row.OrbitRequest.String), &request); err != nil {
			return m.failInvariant(ctx, derivativeID, "canceled derivative has invalid Orbit request")
		}
		if err := validateAdoptedJob(job, request); err != nil {
			return m.failInvariant(ctx, derivativeID, err.Error())
		}
	}
	if handled, err := m.persistCancellationTerminalJob(ctx, row, job); handled || err != nil {
		return err
	}

	stopped, err := m.orbit.Stop(ctx, job.JobID)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			row.OrbitJobID = sql.NullString{String: job.JobID, Valid: true}
			return m.persistCanceledMissingJob(ctx, row)
		}
		return m.deferCancellation(ctx, derivativeID, fmt.Errorf("stop Orbit Job: %w", err))
	}
	if strings.TrimSpace(stopped.JobID) == "" {
		stopped.JobID = job.JobID
	}
	if handled, err := m.persistCancellationTerminalJob(ctx, row, stopped); handled || err != nil {
		return err
	}
	return m.deferCancellation(ctx, derivativeID, fmt.Errorf("Orbit stop has not reached a terminal status: %s", stopped.Status))
}

func (m *Manager) confirmCanceledSubmissionAbsent(ctx context.Context, row frozenDerivativeRow) (bool, error) {
	if row.SubmitAttemptCount == 0 {
		return true, nil
	}
	now := m.now().UTC()
	if !row.OrbitSubmitAbsentAt.Valid {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives
			SET orbit_submit_absent_at = ?, reconcile_after = ?, updated_at = ?
			WHERE id = ? AND cancel_requested_at IS NOT NULL
		`, now, now.Add(m.submissionAbsenceGrace()), now, row.ID); err != nil {
			return false, fmt.Errorf("persist canceled Orbit absence observation: %w", err)
		}
		return false, nil
	}
	if now.Before(row.OrbitSubmitAbsentAt.Time.Add(m.submissionAbsenceGrace())) {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET reconcile_after = ?, updated_at = ?
			WHERE id = ? AND cancel_requested_at IS NOT NULL
		`, row.OrbitSubmitAbsentAt.Time.Add(m.submissionAbsenceGrace()), now, row.ID); err != nil {
			return false, fmt.Errorf("defer canceled Orbit absence confirmation: %w", err)
		}
		return false, nil
	}
	return true, nil
}

func (m *Manager) persistCancellationTerminalJob(ctx context.Context, row frozenDerivativeRow, job orbitapi.Job) (bool, error) {
	status := strings.ToUpper(strings.TrimSpace(job.Status))
	if status != "SUCCEEDED" && status != "FAILED" && status != "STOPPED" {
		return false, nil
	}
	now := m.now().UTC()
	logs, logsErr := m.orbitLogTail(ctx, job.JobID)
	if logsErr != nil {
		logs = m.terminalOrbitLogTail(row.OrbitLogTail, job, logsErr)
		logsErr = nil
	}
	logTail := nullableLogTail(logs, logsErr)
	message := strings.TrimSpace(job.Message)
	if message == "" {
		message = "Orbit Job ended with status " + status
	}
	switch status {
	case "SUCCEEDED":
		_, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET orbit_job_id = ?, processing_status = ?,
			    cancel_requested_at = NULL, orbit_log_tail = ?, processing_error = NULL,
			    reconcile_after = NULL, updated_at = ?
			WHERE id = ? AND cancel_requested_at IS NOT NULL
		`, job.JobID, ProcessingVerifying, logTail, now, row.ID)
		if err != nil {
			return true, fmt.Errorf("persist Orbit success racing cancellation: %w", err)
		}
		return true, nil
	case "FAILED", "STOPPED":
		processingStatus := ProcessingFailed
		if status == "STOPPED" {
			processingStatus = ProcessingCanceled
		}
		_, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET orbit_job_id = ?, processing_status = ?,
			    processing_error = ?, orbit_log_tail = ?, processing_finished_at = ?,
			    orbit_delete_status = ?, reconcile_after = NULL, updated_at = ?
			WHERE id = ? AND cancel_requested_at IS NOT NULL
		`, job.JobID, processingStatus, message, logTail, now, DeletePending, now, row.ID)
		if err != nil {
			return true, fmt.Errorf("persist Orbit cancellation terminal status: %w", err)
		}
		return true, nil
	default:
		return false, nil
	}
}

func (m *Manager) persistCanceledMissingJob(ctx context.Context, row frozenDerivativeRow) error {
	now := m.now().UTC()
	deleteStatus := DeleteNotRequired
	var acceptedAt any
	if row.OrbitJobID.Valid && strings.TrimSpace(row.OrbitJobID.String) != "" {
		deleteStatus = DeleteCompleted
		acceptedAt = now
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET processing_status = ?,
		    processing_error = ?, processing_finished_at = ?, orbit_delete_status = ?,
		    orbit_delete_error = NULL, orbit_delete_accepted_at = ?,
		    orbit_submit_absent_at = NULL, orbit_job_missing_since = NULL,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND cancel_requested_at IS NOT NULL
	`, ProcessingCanceled, "canceled; Orbit Job is absent", now, deleteStatus, acceptedAt, now, row.ID)
	if err != nil {
		return fmt.Errorf("persist canceled absent Orbit Job: %w", err)
	}
	return nil
}

func (m *Manager) deferCancellation(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET processing_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND cancel_requested_at IS NOT NULL
	`, cause.Error(), now.Add(m.pollInterval()), now, derivativeID); err != nil {
		return fmt.Errorf("persist stereo split cancellation retry after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) freezeQueued(ctx context.Context, candidate frozenDerivativeRow) error {
	if m.orbit == nil {
		return fmt.Errorf("freeze stereo split: Orbit is not configured")
	}

	var episode reconcileEpisodeRow
	if err := m.db.GetContext(ctx, &episode, `
		SELECT id, storage_backend, mcap_path, checksum, metadata, cloud_publish_source,
		       camera_serial, calibration_capture_id, calibration_result_sha256
		FROM episodes WHERE id = ? AND deleted_at IS NULL
	`, candidate.EpisodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEpisodeNotFound
		}
		return fmt.Errorf("load stereo split source episode: %w", err)
	}
	bucket, objectKey, err := normalizeEpisodeSource(episodeAdmissionRow{
		ID:                 episode.ID,
		StorageBackend:     episode.StorageBackend,
		McapPath:           episode.McapPath,
		Metadata:           episode.Metadata,
		CloudPublishSource: episode.CloudPublishSource,
	})
	if err != nil {
		return err
	}
	calibrationInput, err := m.loadCalibrationInput(ctx, episode)
	if err != nil {
		return err
	}
	submissionID := fmt.Sprintf("derivative-%d-stereo-split-g%d", candidate.ID, candidate.Generation)
	execution, err := m.PrepareExecution(ctx, ExecutionInput{
		SourceBucket:    bucket,
		SourceObjectKey: objectKey,
		SourceChecksum:  episode.Checksum.String,
		OutputScope:     path.Join(fmt.Sprintf("%d", candidate.EpisodeID), "stereo-split"),
		SubmissionID:    submissionID,
		Generation:      candidate.Generation,
		Calibration:     calibrationInput,
	})
	if err != nil {
		return err
	}
	requestJSON, err := json.Marshal(execution.Request)
	if err != nil {
		return fmt.Errorf("encode frozen Orbit request: %w", err)
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin stereo split freeze: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var lockedStatus string
	if err := tx.GetContext(ctx, &lockedStatus, `
		SELECT processing_status FROM episode_derivatives WHERE id = ?`+forUpdateClause(m.db), candidate.ID); err != nil {
		return fmt.Errorf("lock stereo split derivative: %w", err)
	}
	if lockedStatus != ProcessingQueued {
		return nil
	}
	var lockedEpisode episodeAdmissionRow
	if err := tx.GetContext(ctx, &lockedEpisode, `
		SELECT id, storage_backend, mcap_path, metadata, cloud_publish_source,
		       camera_serial, calibration_capture_id, calibration_result_sha256
		FROM episodes WHERE id = ? AND deleted_at IS NULL`+forUpdateClause(m.db), candidate.EpisodeID); err != nil {
		return fmt.Errorf("lock stereo split source episode: %w", err)
	}
	lockedBucket, lockedKey, err := normalizeEpisodeSource(lockedEpisode)
	if err != nil {
		return err
	}
	if lockedBucket != bucket || lockedKey != objectKey ||
		lockedEpisode.CameraSerial != episode.CameraSerial ||
		lockedEpisode.CalibrationCaptureID != episode.CalibrationCaptureID ||
		lockedEpisode.CalibrationResultSHA256 != episode.CalibrationResultSHA256 ||
		strings.EqualFold(strings.TrimSpace(lockedEpisode.CloudPublishSource.String), CloudSourceOriginal) {
		return ErrCloudSourceLocked
	}
	var calibrationCameraSerial, calibrationSessionID, calibrationCaptureID any
	var calibrationResultURI, calibrationResultETag, calibrationResultSize, calibrationResultSHA256 any
	if execution.Calibration != nil {
		calibrationCameraSerial = execution.Calibration.CameraSerial
		calibrationSessionID = execution.Calibration.SessionID
		calibrationCaptureID = execution.Calibration.CaptureID
		calibrationResultURI = execution.Calibration.ResultURI
		calibrationResultETag = execution.Calibration.ResultETag
		calibrationResultSize = execution.Calibration.ResultSizeBytes
		calibrationResultSHA256 = execution.Calibration.ResultSHA256
	}
	now := m.now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processor_config_revision_id = ?, processor_image = ?,
		    source_uri = ?, source_etag = ?, source_checksum = NULLIF(?, ''),
		    source_size_bytes = ?,
		    calibration_camera_serial = ?, calibration_session_id = ?,
		    calibration_capture_id = ?, calibration_result_uri = ?,
		    calibration_result_etag = ?, calibration_result_size_bytes = ?,
		    calibration_result_sha256 = ?, processing_status = ?,
		    orbit_submission_id = ?, orbit_request = ?, orbit_snapshot_frozen_at = ?,
		    output_prefix = ?, submit_attempt_count = 0, reconcile_after = NULL,
		    processing_error = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, execution.ProcessorConfigRevisionID, execution.ProcessorImage, execution.SourceURI,
		execution.SourceETag, execution.SourceChecksum, execution.SourceSizeBytes,
		calibrationCameraSerial, calibrationSessionID, calibrationCaptureID,
		calibrationResultURI, calibrationResultETag, calibrationResultSize, calibrationResultSHA256,
		ProcessingSubmitting, submissionID, string(requestJSON), now,
		execution.OutputPrefix, now, candidate.ID, ProcessingQueued)
	if err != nil {
		return fmt.Errorf("freeze stereo split Orbit request: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read frozen stereo split update result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("freeze stereo split Orbit request affected %d rows", rows)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stereo split freeze: %w", err)
	}
	return nil
}

func (m *Manager) loadCalibrationInput(
	ctx context.Context,
	episode reconcileEpisodeRow,
) (*CalibrationInput, error) {
	if !episode.CameraSerial.Valid || strings.TrimSpace(episode.CameraSerial.String) == "" {
		return nil, nil
	}
	var result struct {
		CameraSerial string         `db:"camera_serial"`
		Bucket       string         `db:"bucket"`
		ObjectKey    string         `db:"object_key"`
		SizeBytes    int64          `db:"size_bytes"`
		SHA256       string         `db:"sha256"`
		SessionID    sql.NullString `db:"calibration_session_id"`
		CaptureID    sql.NullString `db:"capture_id"`
	}
	cameraSerialComparison := "camera_serial = ?"
	if m.db.DriverName() != "sqlite" {
		cameraSerialComparison = "BINARY camera_serial = BINARY ?"
	}
	if err := m.db.GetContext(ctx, &result, `
		SELECT camera_serial, bucket, object_key, size_bytes, sha256,
		       calibration_session_id, capture_id
		FROM camera_calibrations WHERE `+cameraSerialComparison+`
	`, episode.CameraSerial.String); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return m.loadLegacyCalibrationInput(ctx, episode)
		}
		return nil, fmt.Errorf("load frozen episode calibration result: %w", err)
	}
	if result.CameraSerial != episode.CameraSerial.String || strings.TrimSpace(result.Bucket) == "" ||
		strings.TrimSpace(result.ObjectKey) == "" || result.SizeBytes <= 0 || normalizedSHA256(result.SHA256) == "" {
		return nil, fmt.Errorf("current camera calibration identity is incomplete")
	}
	return &CalibrationInput{
		CameraSerial:    result.CameraSerial,
		SessionID:       result.SessionID.String,
		CaptureID:       result.CaptureID.String,
		ResultBucket:    result.Bucket,
		ResultObjectKey: result.ObjectKey,
		ResultSHA256:    result.SHA256,
	}, nil
}

func (m *Manager) loadLegacyCalibrationInput(ctx context.Context, episode reconcileEpisodeRow) (*CalibrationInput, error) {
	if !episode.CalibrationCaptureID.Valid || strings.TrimSpace(episode.CalibrationCaptureID.String) == "" {
		return nil, nil
	}
	var result calibrationResultRow
	if err := m.db.GetContext(ctx, &result, `
		SELECT c.calibration_session_id, s.camera_serial, c.capture_id, c.status,
		       c.bucket, c.result_object_key, c.result_size_bytes, c.result_checksum_sha256
		FROM calibration_captures c
		INNER JOIN calibration_sessions s ON s.session_id = c.calibration_session_id
		WHERE c.capture_id = ?
	`, episode.CalibrationCaptureID.String); err != nil {
		return nil, fmt.Errorf("load legacy episode calibration result: %w", err)
	}
	if result.Status != "succeeded" || strings.TrimSpace(result.ResultKey) == "" || result.ResultSize <= 0 {
		return nil, nil
	}
	return &CalibrationInput{CameraSerial: result.CameraSerial, SessionID: result.SessionID, CaptureID: result.CaptureID, ResultBucket: result.Bucket, ResultObjectKey: result.ResultKey, ResultSHA256: result.ResultSHA256}, nil
}

func (m *Manager) reconcileSubmitting(ctx context.Context, derivativeID int64) error {
	var frozen frozenDerivativeRow
	if err := m.db.GetContext(ctx, &frozen, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load submitting stereo split: %w", err)
	}
	if frozen.ProcessingStatus != ProcessingSubmitting {
		return nil
	}
	if frozen.CancelRequestedAt.Valid {
		return m.reconcileCancellation(ctx, derivativeID)
	}
	if !frozen.OrbitSubmissionID.Valid || strings.TrimSpace(frozen.OrbitSubmissionID.String) == "" ||
		!frozen.OrbitRequest.Valid || strings.TrimSpace(frozen.OrbitRequest.String) == "" {
		return m.failInvariant(ctx, derivativeID, "submitting derivative has incomplete Orbit snapshot")
	}
	var request orbitapi.SubmitRequest
	if err := json.Unmarshal([]byte(frozen.OrbitRequest.String), &request); err != nil {
		return m.failInvariant(ctx, derivativeID, "submitting derivative has invalid Orbit request")
	}
	if request.SubmissionID != frozen.OrbitSubmissionID.String {
		return m.failInvariant(ctx, derivativeID, "Orbit request submission ID does not match snapshot")
	}

	job, err := m.orbit.Get(ctx, request.SubmissionID)
	if err == nil {
		if err := validateAdoptedJob(job, request); err != nil {
			return m.failInvariant(ctx, derivativeID, err.Error())
		}
		if err := m.markOrbitAccepted(ctx, derivativeID, job.JobID); err != nil {
			return err
		}
		return m.reconcileCancellationIfRequested(ctx, derivativeID)
	}
	if !errors.Is(err, orbitapi.ErrNotFound) {
		return m.deferSubmission(ctx, derivativeID, fmt.Errorf("query Orbit submission: %w", err))
	}
	requested, err := m.cancelRequested(ctx, derivativeID)
	if err != nil {
		return err
	}
	if requested {
		return m.reconcileCancellation(ctx, derivativeID)
	}
	submitting, err := m.recordSubmissionAttempt(ctx, derivativeID)
	if err != nil {
		return err
	}
	if !submitting {
		return m.reconcileCancellation(ctx, derivativeID)
	}

	response, err := m.orbit.Submit(ctx, request)
	if err != nil {
		if errors.Is(err, orbitapi.ErrConflict) {
			job, getErr := m.orbit.Get(ctx, request.SubmissionID)
			if getErr == nil {
				if validateErr := validateAdoptedJob(job, request); validateErr != nil {
					return m.failInvariant(ctx, derivativeID, validateErr.Error())
				}
				if err := m.markOrbitAccepted(ctx, derivativeID, job.JobID); err != nil {
					return err
				}
				return m.reconcileCancellationIfRequested(ctx, derivativeID)
			}
			if errors.Is(getErr, orbitapi.ErrNotFound) {
				return m.deferSubmission(ctx, derivativeID, fmt.Errorf("Orbit binding conflict without Job"))
			}
			return m.deferSubmission(ctx, derivativeID, fmt.Errorf("query conflicting Orbit submission: %w", getErr))
		}
		return m.deferSubmission(ctx, derivativeID, fmt.Errorf("submit Orbit job: %w", err))
	}
	if response.SubmissionID != request.SubmissionID {
		return m.failInvariant(ctx, derivativeID, "Orbit submit response has mismatched submission ID")
	}
	if err := m.markOrbitAccepted(ctx, derivativeID, response.JobID); err != nil {
		return err
	}
	return m.reconcileCancellationIfRequested(ctx, derivativeID)
}

func (m *Manager) recordSubmissionAttempt(ctx context.Context, derivativeID int64) (bool, error) {
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET submit_attempt_count = submit_attempt_count + 1,
		    orbit_submit_absent_at = NULL, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND processing_status = ? AND cancel_requested_at IS NULL
	`, now.Add(m.pollInterval()), now, derivativeID, ProcessingSubmitting)
	if err != nil {
		return false, fmt.Errorf("record Orbit submission attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read Orbit submission attempt result: %w", err)
	}
	return rows == 1, nil
}

func validateAdoptedJob(job orbitapi.Job, request orbitapi.SubmitRequest) error {
	if strings.TrimSpace(job.JobID) == "" || job.SubmissionID != request.SubmissionID || job.Image != request.Image {
		return fmt.Errorf("Orbit Job identity or image does not match frozen request")
	}
	if len(job.DataBindings) > 0 && !equalBindings(job.DataBindings, request.DataBindings) {
		return fmt.Errorf("Orbit Job bindings do not match frozen request")
	}
	return nil
}

func equalBindings(left, right []orbitapi.DataBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (m *Manager) markOrbitAccepted(ctx context.Context, derivativeID int64, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return m.failInvariant(ctx, derivativeID, "Orbit Job ID is empty")
	}
	now := m.now().UTC()
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET orbit_job_id = ?, orbit_submit_absent_at = NULL, orbit_job_missing_since = NULL,
		    processing_status = ?, processing_started_at = COALESCE(processing_started_at, ?),
		    reconcile_after = ?, processing_error = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, jobID, ProcessingPending, now, now.Add(m.pollInterval()), now, derivativeID, ProcessingSubmitting)
	if err != nil {
		return fmt.Errorf("persist accepted Orbit Job: %w", err)
	}
	return nil
}

func (m *Manager) deferSubmission(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET reconcile_after = CASE WHEN cancel_requested_at IS NULL THEN ? ELSE NULL END,
		    processing_error = ?, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, now.Add(m.pollInterval()), cause.Error(), now, derivativeID, ProcessingSubmitting)
	if err != nil {
		return fmt.Errorf("persist Orbit submission retry after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) failInvariant(ctx context.Context, derivativeID int64, message string) error {
	now := m.now().UTC()
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status = ?, processing_error = ?, processing_finished_at = ?,
		    orbit_delete_status = CASE WHEN orbit_job_id IS NULL THEN ? ELSE ? END,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ?
	`, ProcessingFailed, message, now, DeleteNotRequired, DeletePending, now, derivativeID)
	if err != nil {
		return fmt.Errorf("persist stereo split invariant failure: %w", err)
	}
	return fmt.Errorf("stereo split invariant failed: %s", message)
}

func (m *Manager) reconcileOrbitStatus(ctx context.Context, derivativeID int64) error {
	var row frozenDerivativeRow
	if err := m.db.GetContext(ctx, &row, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id, orbit_log_tail,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load active stereo split: %w", err)
	}
	if row.CancelRequestedAt.Valid {
		return m.reconcileCancellation(ctx, derivativeID)
	}
	if !row.OrbitJobID.Valid || strings.TrimSpace(row.OrbitJobID.String) == "" ||
		!row.OrbitRequest.Valid || strings.TrimSpace(row.OrbitRequest.String) == "" {
		return m.failInvariant(ctx, derivativeID, "active derivative has incomplete Orbit identity")
	}
	var request orbitapi.SubmitRequest
	if err := json.Unmarshal([]byte(row.OrbitRequest.String), &request); err != nil {
		return m.failInvariant(ctx, derivativeID, "active derivative has invalid Orbit request")
	}
	job, err := m.orbit.Get(ctx, row.OrbitJobID.String)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			return m.reconcileMissingActiveJob(ctx, row)
		}
		return m.deferActivePoll(ctx, derivativeID, fmt.Errorf("query active Orbit Job: %w", err))
	}
	if err := validateAdoptedJob(job, request); err != nil {
		return m.failInvariant(ctx, derivativeID, err.Error())
	}
	requested, err := m.cancelRequested(ctx, derivativeID)
	if err != nil {
		return err
	}
	if requested {
		return m.reconcileCancellation(ctx, derivativeID)
	}
	now := m.now().UTC()
	switch strings.ToUpper(strings.TrimSpace(job.Status)) {
	case "PENDING":
		_, err = m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, reconcile_after = ?,
			    orbit_job_missing_since = NULL, processing_error = NULL, updated_at = ? WHERE id = ?
		`, ProcessingPending, now.Add(m.pollInterval()), now, derivativeID)
	case "RUNNING":
		_, err = m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, reconcile_after = ?,
			    processing_started_at = COALESCE(processing_started_at, ?),
			    orbit_job_missing_since = NULL, processing_error = NULL, updated_at = ? WHERE id = ?
		`, ProcessingRunning, now.Add(m.pollInterval()), now, now, derivativeID)
	case "SUCCEEDED":
		logs, logsErr := m.orbitLogTail(ctx, row.OrbitJobID.String)
		if logsErr != nil {
			logs = m.terminalOrbitLogTail(row.OrbitLogTail, job, logsErr)
			logsErr = nil
		}
		_, err = m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, reconcile_after = NULL,
			    orbit_job_missing_since = NULL, orbit_log_tail = ?, processing_error = NULL, updated_at = ? WHERE id = ?
		`, ProcessingVerifying, nullableLogTail(logs, logsErr), now, derivativeID)
	case "FAILED", "STOPPED":
		logs, logsErr := m.orbitLogTail(ctx, row.OrbitJobID.String)
		if logsErr != nil {
			logs = m.terminalOrbitLogTail(row.OrbitLogTail, job, logsErr)
			logsErr = nil
		}
		status := ProcessingFailed
		if strings.EqualFold(job.Status, "STOPPED") {
			status = ProcessingCanceled
		}
		message := strings.TrimSpace(job.Message)
		if message == "" {
			message = "Orbit Job ended with status " + strings.ToUpper(job.Status)
		}
		_, err = m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, processing_error = ?,
			    orbit_log_tail = ?, processing_finished_at = ?, orbit_delete_status = ?,
			    orbit_job_missing_since = NULL, reconcile_after = NULL, updated_at = ? WHERE id = ?
		`, status, message, nullableLogTail(logs, logsErr), now, DeletePending, now, derivativeID)
	default:
		return m.deferActivePoll(ctx, derivativeID, fmt.Errorf("Orbit returned unknown status %q", job.Status))
	}
	if err != nil {
		return fmt.Errorf("persist Orbit status %q: %w", job.Status, err)
	}
	return nil
}

func (m *Manager) reconcileMissingActiveJob(ctx context.Context, row frozenDerivativeRow) error {
	now := m.now().UTC()
	if !row.OrbitJobMissingSince.Valid {
		_, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives
			SET orbit_job_missing_since = ?, processing_error = ?, reconcile_after = ?, updated_at = ?
			WHERE id = ? AND processing_status IN (?, ?)
		`, now, "Orbit Job is temporarily absent", now.Add(m.pollInterval()), now,
			row.ID, ProcessingPending, ProcessingRunning)
		if err != nil {
			return fmt.Errorf("persist missing active Orbit Job: %w", err)
		}
		return fmt.Errorf("active Orbit Job is temporarily absent")
	}
	if now.Before(row.OrbitJobMissingSince.Time.Add(m.activeJobMissingGrace())) {
		return m.deferActivePoll(ctx, row.ID, fmt.Errorf("active Orbit Job remains absent"))
	}
	message := "Orbit Job disappeared before a terminal status was observed"
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status = ?, processing_error = ?, processing_finished_at = ?,
		    orbit_delete_status = ?, orbit_delete_error = NULL, orbit_delete_accepted_at = ?,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND processing_status IN (?, ?)
	`, ProcessingFailed, message, now, DeleteCompleted, now, now,
		row.ID, ProcessingPending, ProcessingRunning)
	if err != nil {
		return fmt.Errorf("persist missing active Orbit Job failure: %w", err)
	}
	return fmt.Errorf("%s", message)
}

func (m *Manager) deferActivePoll(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET processing_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ?
	`, cause.Error(), now.Add(m.pollInterval()), now, derivativeID); err != nil {
		return fmt.Errorf("persist Orbit poll retry after %v: %w", cause, err)
	}
	return cause
}

type processingManifest struct {
	SchemaVersion  int    `json:"schema_version"`
	Status         string `json:"status"`
	Kind           string `json:"kind"`
	ProcessingMode string `json:"processing_mode,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	Generation     int    `json:"generation"`
	ProcessorImage string `json:"processor_image"`
	Source         struct {
		URI       string `json:"uri"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	} `json:"source"`
	Calibration *manifestCalibration `json:"calibration,omitempty"`
	Outputs     struct {
		MCAP     manifestOutput `json:"mcap"`
		Metadata manifestOutput `json:"metadata"`
	} `json:"outputs"`
	Stats      manifestStats `json:"stats"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
}

type manifestCalibration struct {
	AttachmentName string `json:"attachment_name"`
	MediaType      string `json:"media_type"`
	CameraSerial   string `json:"camera_serial"`
	SessionID      string `json:"session_id"`
	CaptureID      string `json:"capture_id"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
}

type manifestOutput struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type manifestStats struct {
	InputMode                 string `json:"input_mode,omitempty"`
	InputMessages             int64  `json:"input_messages"`
	DecodedImages             int64  `json:"decoded_images"`
	LeftImages                int64  `json:"left_images,omitempty"`
	RightImages               int64  `json:"right_images,omitempty"`
	LeftVideos                int64  `json:"left_videos,omitempty"`
	RightVideos               int64  `json:"right_videos,omitempty"`
	IMUMessages               int64  `json:"imu_messages"`
	CopiedMessages            int64  `json:"copied_messages,omitempty"`
	CopiedTopics              int64  `json:"copied_topics,omitempty"`
	SkippedMessages           int64  `json:"skipped_messages"`
	TimestampRepairApplied    bool   `json:"timestamp_repair_applied,omitempty"`
	TimestampRepairReason     string `json:"timestamp_repair_reason,omitempty"`
	TimestampRepairedMessages int64  `json:"timestamp_repaired_messages,omitempty"`
}

type verificationRow struct {
	ID                       int64          `db:"id"`
	Generation               int            `db:"generation"`
	ProcessorImage           string         `db:"processor_image"`
	SourceURI                string         `db:"source_uri"`
	SourceETag               string         `db:"source_etag"`
	SourceChecksum           sql.NullString `db:"source_checksum"`
	SourceSize               int64          `db:"source_size_bytes"`
	CalibrationCameraSerial  sql.NullString `db:"calibration_camera_serial"`
	CalibrationSessionID     sql.NullString `db:"calibration_session_id"`
	CalibrationCaptureID     sql.NullString `db:"calibration_capture_id"`
	CalibrationResultURI     sql.NullString `db:"calibration_result_uri"`
	CalibrationResultETag    sql.NullString `db:"calibration_result_etag"`
	CalibrationResultSize    sql.NullInt64  `db:"calibration_result_size_bytes"`
	CalibrationResultSHA256  sql.NullString `db:"calibration_result_sha256"`
	OutputPrefix             string         `db:"output_prefix"`
	Status                   string         `db:"processing_status"`
	VerificationAttemptCount int            `db:"verification_attempt_count"`
}

func (m *Manager) verifySucceeded(ctx context.Context, derivativeID int64) error {
	var row verificationRow
	if err := m.db.GetContext(ctx, &row, `
		SELECT id, generation, processor_image, source_uri, source_etag, source_checksum,
		       source_size_bytes, calibration_camera_serial, calibration_session_id,
		       calibration_capture_id, calibration_result_uri, calibration_result_etag,
		       calibration_result_size_bytes, calibration_result_sha256,
		       output_prefix, processing_status, verification_attempt_count
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load verifying stereo split: %w", err)
	}
	if row.Status != ProcessingVerifying {
		return nil
	}
	calibration, err := calibrationSnapshotFromVerificationRow(row)
	if err != nil {
		return m.failVerification(ctx, derivativeID, err)
	}
	output, qa, err := m.verifyExecutionAndQA(ctx, ExecutionSnapshot{
		Generation:      row.Generation,
		ProcessorImage:  row.ProcessorImage,
		SourceURI:       row.SourceURI,
		SourceETag:      row.SourceETag,
		SourceChecksum:  row.SourceChecksum.String,
		SourceSizeBytes: row.SourceSize,
		Calibration:     calibration,
		OutputBucket:    m.cfg.OutputBucket,
		OutputPrefix:    row.OutputPrefix,
	})
	if err != nil {
		if errors.Is(err, ErrOutputNotSettled) {
			return m.retryVerification(ctx, row, err)
		}
		return m.failVerification(ctx, derivativeID, err)
	}
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET mcap_path = ?, metadata_path = ?, manifest_path = ?, checksum = ?,
		    file_size_bytes = ?, processing_duration_sec = ?, duration_sec = COALESCE(?, duration_sec), processing_result = ?,
		    processing_status = ?, processing_error = NULL, processing_finished_at = ?,
		    qa_status = ?, qa_attempt_count = qa_attempt_count + 1,
		    qa_started_at = COALESCE(qa_started_at, ?), qa_score = ?,
		    quality_flag = NULLIF(?, ''), qa_result = ?, qa_error = NULLIF(?, ''),
		    qa_finished_at = ?, qa_next_retry_at = NULL, orbit_delete_status = ?,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, output.MCAPObjectKey, output.MetadataObjectKey, output.ManifestObjectKey, output.MCAPChecksumSHA256,
		output.MCAPSizeBytes, output.ProcessingDurationSec, qa.DurationSec, output.ManifestJSON,
		ProcessingSucceeded, now, qa.Status, now, qa.Score, qa.QualityFlag, qa.ResultJSON,
		qa.Error, now, DeletePending, now, derivativeID, ProcessingVerifying)
	if err != nil {
		return fmt.Errorf("persist verified stereo split output and QA: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read verified stereo split update result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("persist verified stereo split output affected %d rows", rows)
	}
	return nil
}

func calibrationSnapshotFromVerificationRow(row verificationRow) (*CalibrationSnapshot, error) {
	return calibrationSnapshotFromColumns(
		row.CalibrationCameraSerial,
		row.CalibrationSessionID,
		row.CalibrationCaptureID,
		row.CalibrationResultURI,
		row.CalibrationResultETag,
		row.CalibrationResultSize,
		row.CalibrationResultSHA256,
	)
}

func calibrationSnapshotFromColumns(
	cameraSerial sql.NullString,
	sessionID sql.NullString,
	captureID sql.NullString,
	resultURI sql.NullString,
	resultETag sql.NullString,
	resultSize sql.NullInt64,
	resultSHA256 sql.NullString,
) (*CalibrationSnapshot, error) {
	required := []bool{
		cameraSerial.Valid,
		resultURI.Valid,
		resultETag.Valid,
		resultSize.Valid,
		resultSHA256.Valid,
	}
	count := 0
	for _, value := range required {
		if value {
			count++
		}
	}
	if count == 0 {
		return nil, nil
	}
	if count != len(required) {
		return nil, fmt.Errorf("frozen calibration result identity is incomplete")
	}
	return &CalibrationSnapshot{
		CameraSerial:    cameraSerial.String,
		SessionID:       sessionID.String,
		CaptureID:       captureID.String,
		ResultURI:       resultURI.String,
		ResultETag:      resultETag.String,
		ResultSizeBytes: resultSize.Int64,
		ResultSHA256:    resultSHA256.String,
	}, nil
}

func validateManifestSnapshot(manifest processingManifest, execution ExecutionSnapshot) error {
	if manifest.Status != "succeeded" || manifest.Kind != Kind ||
		manifest.Generation != execution.Generation || manifest.ProcessorImage != execution.ProcessorImage ||
		manifest.Source.URI != execution.SourceURI || manifest.Source.SizeBytes != execution.SourceSizeBytes {
		return fmt.Errorf("processing manifest does not match frozen execution snapshot")
	}
	switch manifest.SchemaVersion {
	case manifestSchemaV1:
		if strings.TrimSpace(manifest.OutputFormat) != "" {
			return fmt.Errorf("processing manifest v1 has an unexpected output format")
		}
	case manifestSchemaV2:
		if manifest.OutputFormat != stereoH264OutputFormat {
			return fmt.Errorf("processing manifest v2 has an invalid output format")
		}
	case manifestSchemaV3:
		if manifest.OutputFormat != stereoH264OutputFormat {
			return fmt.Errorf("processing manifest v3 has an invalid output format")
		}
	default:
		return fmt.Errorf("processing manifest schema version %d is unsupported", manifest.SchemaVersion)
	}
	if execution.SourceChecksum != "" && manifest.Source.SHA256 != execution.SourceChecksum {
		return fmt.Errorf("processing manifest source checksum does not match snapshot")
	}
	if err := validateManifestCalibration(manifest, execution.Calibration); err != nil {
		return err
	}
	if manifest.Outputs.MCAP.Name != outputMcapName || manifest.Outputs.Metadata.Name != outputMetadataName ||
		manifest.Outputs.MCAP.SizeBytes <= 0 || manifest.Outputs.Metadata.SizeBytes <= 0 ||
		normalizedSHA256(manifest.Outputs.MCAP.SHA256) == "" || normalizedSHA256(manifest.Outputs.Metadata.SHA256) == "" {
		return fmt.Errorf("processing manifest has invalid fixed outputs")
	}
	if manifest.StartedAt.IsZero() || manifest.FinishedAt.IsZero() || manifest.FinishedAt.Before(manifest.StartedAt) {
		return fmt.Errorf("processing manifest has invalid timestamps")
	}
	return nil
}

func validateManifestCalibration(manifest processingManifest, expected *CalibrationSnapshot) error {
	if manifest.Calibration == nil {
		return nil
	}
	if expected == nil {
		return fmt.Errorf("processing manifest has unexpected calibration data")
	}
	actual := manifest.Calibration
	if actual.AttachmentName != calibrationAttachment || actual.MediaType != calibrationMediaType ||
		actual.CameraSerial != expected.CameraSerial || actual.SizeBytes != expected.ResultSizeBytes ||
		normalizedSHA256(actual.SHA256) == "" ||
		normalizedSHA256(actual.SHA256) != normalizedSHA256(expected.ResultSHA256) {
		return fmt.Errorf("processing manifest calibration does not match frozen execution snapshot")
	}
	return nil
}

func (m *Manager) retryVerification(ctx context.Context, row verificationRow, cause error) error {
	if row.VerificationAttemptCount+1 >= maxVerificationAttempts {
		return m.failVerification(ctx, row.ID, fmt.Errorf("verification retry limit reached after transient output error: %w", cause))
	}
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET verification_attempt_count = verification_attempt_count + 1,
		    processing_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, cause.Error(), now.Add(m.pollInterval()), now, row.ID, ProcessingVerifying); err != nil {
		return fmt.Errorf("persist stereo split verification retry after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) failVerification(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET processing_status = ?, processing_error = ?,
		    processing_finished_at = ?, orbit_delete_status = ?, reconcile_after = NULL,
		    updated_at = ? WHERE id = ? AND processing_status = ?
	`, ProcessingFailed, cause.Error(), now, DeletePending, now, derivativeID, ProcessingVerifying); err != nil {
		return fmt.Errorf("persist verification failure after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) reconcileQA(ctx context.Context, derivativeID int64) error {
	var row struct {
		Status                  string         `db:"processing_status"`
		QAStatus                string         `db:"qa_status"`
		McapPath                sql.NullString `db:"mcap_path"`
		Checksum                sql.NullString `db:"checksum"`
		Result                  string         `db:"processing_result"`
		CalibrationCameraSerial sql.NullString `db:"calibration_camera_serial"`
		CalibrationSessionID    sql.NullString `db:"calibration_session_id"`
		CalibrationCaptureID    sql.NullString `db:"calibration_capture_id"`
		CalibrationResultURI    sql.NullString `db:"calibration_result_uri"`
		CalibrationResultETag   sql.NullString `db:"calibration_result_etag"`
		CalibrationResultSize   sql.NullInt64  `db:"calibration_result_size_bytes"`
		CalibrationResultSHA256 sql.NullString `db:"calibration_result_sha256"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT processing_status, qa_status, mcap_path, checksum, processing_result,
		       calibration_camera_serial, calibration_session_id, calibration_capture_id,
		       calibration_result_uri, calibration_result_etag,
		       calibration_result_size_bytes, calibration_result_sha256
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load stereo split QA input: %w", err)
	}
	if row.Status != ProcessingSucceeded || (row.QAStatus != QAPending && row.QAStatus != QARunning) {
		return nil
	}
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET qa_status = ?, qa_attempt_count = qa_attempt_count + 1,
		    qa_started_at = COALESCE(qa_started_at, ?), updated_at = ?
		WHERE id = ? AND qa_status IN (?, ?)
	`, QARunning, now, now, derivativeID, QAPending, QARunning); err != nil {
		return fmt.Errorf("claim stereo split QA: %w", err)
	}
	var manifest processingManifest
	err := json.Unmarshal([]byte(row.Result), &manifest)
	var observed mcapQAObservation
	var contract mcapOutputContract
	if err == nil {
		var calibration *CalibrationSnapshot
		calibration, err = calibrationSnapshotFromColumns(
			row.CalibrationCameraSerial,
			row.CalibrationSessionID,
			row.CalibrationCaptureID,
			row.CalibrationResultURI,
			row.CalibrationResultETag,
			row.CalibrationResultSize,
			row.CalibrationResultSHA256,
		)
		if err == nil {
			err = validateManifestCalibration(manifest, calibration)
		}
	}
	if err == nil {
		contract, err = validateManifestStats(manifest)
	}
	if err == nil {
		observed, err = m.inspectOutputMCAP(ctx, row.McapPath.String, row.Checksum.String, contract)
	}
	approved := err == nil
	qaStatus := QAApproved
	qaScore := 1.0
	qualityFlag := ""
	qaError := ""
	if !approved {
		qaStatus = QAFailed
		qaScore = 0
		qualityFlag = "双目拆分输出统计不满足质检规则"
		if err != nil {
			qaError = err.Error()
		}
	}
	qaResult, marshalErr := json.Marshal(map[string]any{
		"approved":       approved,
		"manifest_stats": manifest.Stats,
		"observed":       observed,
	})
	if marshalErr != nil {
		return fmt.Errorf("encode stereo split QA result: %w", marshalErr)
	}
	var duration any
	if observed.DurationSec > 0 {
		duration = observed.DurationSec
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET qa_status = ?, qa_score = ?, quality_flag = NULLIF(?, ''),
		    qa_result = ?, qa_error = NULLIF(?, ''), qa_finished_at = ?,
		    duration_sec = COALESCE(?, duration_sec), reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND qa_status = ?
	`, qaStatus, qaScore, qualityFlag, string(qaResult), qaError, now,
		duration, now, derivativeID, QARunning); err != nil {
		return fmt.Errorf("persist stereo split QA: %w", err)
	}
	return nil
}

type mcapQAObservation struct {
	SchemaVersion          int     `json:"schema_version"`
	LeftImages             int64   `json:"left_images,omitempty"`
	RightImages            int64   `json:"right_images,omitempty"`
	LeftVideos             int64   `json:"left_videos,omitempty"`
	RightVideos            int64   `json:"right_videos,omitempty"`
	IMUMessages            int64   `json:"imu_messages"`
	CalibrationAttachments int64   `json:"calibration_attachments,omitempty"`
	FirstLogTime           uint64  `json:"first_log_time"`
	LastLogTime            uint64  `json:"last_log_time"`
	DurationSec            float64 `json:"duration_sec"`
	OutputSHA256           string  `json:"output_sha256"`
}

type topicQAState struct {
	Count   int64
	Last    uint64
	HasLast bool
}

type mcapOutputContract struct {
	SchemaVersion        int
	LeftTopic            string
	RightTopic           string
	LeftSchema           string
	RightSchema          string
	LeftSchemaEncoding   string
	RightSchemaEncoding  string
	LeftMessageEncoding  string
	RightMessageEncoding string
	ExpectedLeft         int64
	ExpectedRight        int64
	ExpectedIMU          int64
	Calibration          *manifestCalibration
}

func validateManifestStats(manifest processingManifest) (mcapOutputContract, error) {
	stats := manifest.Stats
	processingMode := manifest.ProcessingMode
	if processingMode == "" {
		processingMode = "convert"
	}
	if processingMode != "convert" && processingMode != "timestamp_repair" {
		return mcapOutputContract{}, fmt.Errorf("processing manifest has unsupported processing mode %q", processingMode)
	}
	contract := mcapOutputContract{
		SchemaVersion: manifest.SchemaVersion,
		ExpectedIMU:   stats.IMUMessages,
	}
	switch manifest.SchemaVersion {
	case manifestSchemaV1:
		if strings.TrimSpace(manifest.OutputFormat) != "" {
			return mcapOutputContract{}, fmt.Errorf("processing manifest v1 has an unexpected output format")
		}
		contract.LeftTopic = leftImageTopic
		contract.RightTopic = rightImageTopic
		contract.LeftSchema = compressedImageSchema
		contract.RightSchema = compressedImageSchema
		contract.LeftSchemaEncoding = "ros2msg"
		contract.RightSchemaEncoding = "ros2msg"
		contract.LeftMessageEncoding = "cdr"
		contract.RightMessageEncoding = "cdr"
		contract.ExpectedLeft = stats.LeftImages
		contract.ExpectedRight = stats.RightImages
		if manifest.Calibration != nil {
			return mcapOutputContract{}, fmt.Errorf("processing manifest v1 has unexpected calibration data")
		}
	case manifestSchemaV2, manifestSchemaV3:
		if manifest.OutputFormat != stereoH264OutputFormat {
			return mcapOutputContract{}, fmt.Errorf("processing manifest v%d has an invalid output format", manifest.SchemaVersion)
		}
		if stats.CopiedMessages < 0 || stats.CopiedTopics < 0 {
			return mcapOutputContract{}, fmt.Errorf("processing manifest contains negative copied-topic statistics")
		}
		contract.LeftTopic = leftVideoTopic
		contract.RightTopic = rightVideoTopic
		contract.LeftSchema = compressedVideoSchema
		contract.RightSchema = compressedVideoSchema
		contract.LeftSchemaEncoding = "protobuf"
		contract.RightSchemaEncoding = "protobuf"
		contract.LeftMessageEncoding = "protobuf"
		contract.RightMessageEncoding = "protobuf"
		contract.ExpectedLeft = stats.LeftVideos
		contract.ExpectedRight = stats.RightVideos
		if manifest.SchemaVersion == manifestSchemaV2 && manifest.Calibration != nil {
			return mcapOutputContract{}, fmt.Errorf("processing manifest v2 has unexpected calibration data")
		}
		if manifest.SchemaVersion == manifestSchemaV3 {
			if err := validateManifestCalibrationShape(manifest.Calibration); err != nil {
				return mcapOutputContract{}, err
			}
			contract.Calibration = manifest.Calibration
		}
	default:
		return mcapOutputContract{}, fmt.Errorf("processing manifest schema version %d is unsupported", manifest.SchemaVersion)
	}
	if processingMode == "timestamp_repair" {
		if manifest.SchemaVersion == manifestSchemaV1 {
			return mcapOutputContract{}, fmt.Errorf("timestamp repair requires a stereo H.264 manifest")
		}
		if stats.InputMode != "split_h264" || stats.InputMessages <= 0 || stats.DecodedImages != 0 || stats.IMUMessages <= 0 ||
			stats.SkippedMessages != 0 {
			return mcapOutputContract{}, fmt.Errorf("timestamp repair manifest contains invalid processing statistics")
		}
		if stats.InputMessages != contract.ExpectedLeft || contract.ExpectedLeft != contract.ExpectedRight {
			return mcapOutputContract{}, fmt.Errorf("timestamp repair manifest frame counts are inconsistent")
		}
		return contract, nil
	}
	if stats.InputMessages <= 0 || stats.DecodedImages <= 0 || contract.ExpectedLeft <= 0 ||
		contract.ExpectedRight <= 0 || stats.IMUMessages <= 0 || stats.SkippedMessages < 0 {
		return mcapOutputContract{}, fmt.Errorf("processing manifest contains non-positive required statistics")
	}
	if stats.InputMessages != stats.DecodedImages+stats.SkippedMessages {
		return mcapOutputContract{}, fmt.Errorf("processing manifest input message accounting is inconsistent")
	}
	if stats.DecodedImages != contract.ExpectedLeft || contract.ExpectedLeft != contract.ExpectedRight {
		return mcapOutputContract{}, fmt.Errorf("processing manifest stereo frame counts are inconsistent")
	}
	return contract, nil
}

func (m *Manager) inspectOutputMCAP(
	ctx context.Context,
	objectKey string,
	expectedChecksum string,
	contract mcapOutputContract,
) (mcapQAObservation, error) {
	if strings.TrimSpace(objectKey) == "" {
		return mcapQAObservation{}, fmt.Errorf("stereo split output MCAP path is empty")
	}
	body, err := m.objects.OpenObject(ctx, m.cfg.OutputBucket, objectKey)
	if err != nil {
		return mcapQAObservation{}, fmt.Errorf("open stereo split output for QA: %w", err)
	}
	digest := sha256.New()
	calibrationAttachments := int64(0)
	lexer, err := mcap.NewLexer(io.TeeReader(body, digest), &mcap.LexerOptions{
		ComputeAttachmentCRCs: true,
		AttachmentCallback: func(attachment *mcap.AttachmentReader) error {
			if attachment.Name != calibrationAttachment {
				return nil
			}
			calibrationAttachments++
			if contract.Calibration == nil {
				return fmt.Errorf("output MCAP has an unexpected calibration attachment")
			}
			return validateCalibrationAttachment(attachment, contract.Calibration)
		},
	})
	if err != nil {
		_ = body.Close()
		return mcapQAObservation{}, fmt.Errorf("open output MCAP lexer: %w", err)
	}
	states := map[string]*topicQAState{
		contract.LeftTopic:  {},
		contract.RightTopic: {},
		imuTopic:            {},
	}
	expectedSchemas := map[string]string{
		contract.LeftTopic:  contract.LeftSchema,
		contract.RightTopic: contract.RightSchema,
		imuTopic:            imuSchema,
	}
	expectedSchemaEncodings := map[string]string{
		contract.LeftTopic:  contract.LeftSchemaEncoding,
		contract.RightTopic: contract.RightSchemaEncoding,
		imuTopic:            "ros2msg",
	}
	expectedEncodings := map[string]string{
		contract.LeftTopic:  contract.LeftMessageEncoding,
		contract.RightTopic: contract.RightMessageEncoding,
		imuTopic:            "cdr",
	}
	var firstLogTime uint64
	var lastLogTime uint64
	var hasLogTime bool
	schemas := make(map[uint16]*mcap.Schema)
	channels := make(map[uint16]*mcap.Channel)
	var recordBuffer []byte
	for {
		token, record, nextErr := lexer.Next(recordBuffer)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("read output MCAP: %w", nextErr)
		}
		recordBuffer = record
		switch token {
		case mcap.TokenSchema:
			schema, parseErr := mcap.ParseSchema(record)
			if parseErr != nil {
				_ = body.Close()
				return mcapQAObservation{}, fmt.Errorf("parse output MCAP schema: %w", parseErr)
			}
			schemas[schema.ID] = schema
			continue
		case mcap.TokenChannel:
			channel, parseErr := mcap.ParseChannel(record)
			if parseErr != nil {
				_ = body.Close()
				return mcapQAObservation{}, fmt.Errorf("parse output MCAP channel: %w", parseErr)
			}
			channels[channel.ID] = channel
			continue
		case mcap.TokenMessage:
		default:
			continue
		}
		current, parseErr := mcap.ParseMessage(record)
		if parseErr != nil {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("parse output MCAP message: %w", parseErr)
		}
		channel, ok := channels[current.ChannelID]
		if !ok {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("output MCAP message references unknown channel %d", current.ChannelID)
		}
		state, required := states[channel.Topic]
		if !required {
			continue
		}
		schema, ok := schemas[channel.SchemaID]
		if !ok {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("topic %s references unknown schema %d", channel.Topic, channel.SchemaID)
		}
		if schema == nil || schema.Name != expectedSchemas[channel.Topic] ||
			schema.Encoding != expectedSchemaEncodings[channel.Topic] {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("topic %s has unexpected schema", channel.Topic)
		}
		if channel.MessageEncoding != expectedEncodings[channel.Topic] {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("topic %s has unexpected message encoding", channel.Topic)
		}
		if state.HasLast && current.LogTime < state.Last {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("topic %s log timestamps are not nondecreasing", channel.Topic)
		}
		state.Count++
		state.Last = current.LogTime
		state.HasLast = true
		if !hasLogTime || current.LogTime < firstLogTime {
			firstLogTime = current.LogTime
		}
		if !hasLogTime || current.LogTime > lastLogTime {
			lastLogTime = current.LogTime
		}
		hasLogTime = true
	}
	if contract.Calibration != nil && calibrationAttachments != 1 {
		_ = body.Close()
		return mcapQAObservation{}, fmt.Errorf("output MCAP calibration attachment count is %d, want 1", calibrationAttachments)
	}
	if err := body.Close(); err != nil {
		return mcapQAObservation{}, fmt.Errorf("close output MCAP after QA: %w", err)
	}
	checksum := hex.EncodeToString(digest.Sum(nil))
	if checksum != normalizedSHA256(expectedChecksum) {
		return mcapQAObservation{}, fmt.Errorf("output MCAP SHA-256 does not match processing manifest")
	}
	leftCount := states[contract.LeftTopic].Count
	rightCount := states[contract.RightTopic].Count
	imuCount := states[imuTopic].Count
	if leftCount <= 0 || leftCount != rightCount || imuCount <= 0 {
		return mcapQAObservation{}, fmt.Errorf("output MCAP required topic counts are invalid")
	}
	if leftCount != contract.ExpectedLeft || rightCount != contract.ExpectedRight || imuCount != contract.ExpectedIMU {
		return mcapQAObservation{}, fmt.Errorf("output MCAP topic counts do not match processing manifest")
	}
	if !hasLogTime || lastLogTime <= firstLogTime {
		return mcapQAObservation{}, fmt.Errorf("output MCAP timestamp span must be positive")
	}
	observed := mcapQAObservation{
		SchemaVersion:          contract.SchemaVersion,
		IMUMessages:            imuCount,
		CalibrationAttachments: calibrationAttachments,
		FirstLogTime:           firstLogTime,
		LastLogTime:            lastLogTime,
		DurationSec:            float64(lastLogTime-firstLogTime) / float64(time.Second),
		OutputSHA256:           checksum,
	}
	if contract.SchemaVersion == manifestSchemaV1 {
		observed.LeftImages = leftCount
		observed.RightImages = rightCount
	} else {
		observed.LeftVideos = leftCount
		observed.RightVideos = rightCount
	}
	return observed, nil
}

func validateManifestCalibrationShape(calibration *manifestCalibration) error {
	if calibration == nil || calibration.AttachmentName != calibrationAttachment ||
		calibration.MediaType != calibrationMediaType || strings.TrimSpace(calibration.CameraSerial) == "" ||
		calibration.SizeBytes <= 0 || normalizedSHA256(calibration.SHA256) == "" {
		return fmt.Errorf("processing manifest v3 has invalid calibration data")
	}
	return nil
}

func validateCalibrationAttachment(
	attachment *mcap.AttachmentReader,
	expected *manifestCalibration,
) error {
	if expected.SizeBytes <= 0 {
		return fmt.Errorf("processing manifest calibration attachment size is invalid")
	}
	expectedSize := uint64(expected.SizeBytes)
	if attachment.MediaType != expected.MediaType || attachment.DataSize != expectedSize {
		return fmt.Errorf("output MCAP calibration attachment metadata does not match manifest")
	}
	data, err := io.ReadAll(io.LimitReader(attachment.Data(), expected.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("read output MCAP calibration attachment: %w", err)
	}
	if int64(len(data)) != expected.SizeBytes {
		return fmt.Errorf("output MCAP calibration attachment size does not match manifest")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != normalizedSHA256(expected.SHA256) {
		return fmt.Errorf("output MCAP calibration attachment SHA-256 does not match manifest")
	}
	computedCRC, err := attachment.ComputedCRC()
	if err != nil {
		return fmt.Errorf("compute output MCAP calibration attachment CRC: %w", err)
	}
	parsedCRC, err := attachment.ParsedCRC()
	if err != nil {
		return fmt.Errorf("read output MCAP calibration attachment CRC: %w", err)
	}
	if parsedCRC != 0 && parsedCRC != computedCRC {
		return fmt.Errorf("output MCAP calibration attachment CRC is invalid")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("output MCAP calibration attachment is invalid JSON: %w", err)
	}
	if len(document) == 0 {
		return fmt.Errorf("output MCAP calibration attachment JSON is empty")
	}
	if rawSerial, ok := document["camera_serial"]; ok {
		var serial string
		if err := json.Unmarshal(rawSerial, &serial); err != nil || serial != expected.CameraSerial {
			return fmt.Errorf("output MCAP calibration attachment identity does not match manifest")
		}
	}
	return nil
}

func (m *Manager) reconcileDelete(ctx context.Context, derivativeID int64) error {
	var row struct {
		JobID   sql.NullString `db:"orbit_job_id"`
		LogTail sql.NullString `db:"orbit_log_tail"`
		Status  string         `db:"orbit_delete_status"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT orbit_job_id, orbit_log_tail, orbit_delete_status FROM episode_derivatives
		WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load Orbit delete state: %w", err)
	}
	if row.Status != DeletePending {
		return nil
	}
	now := m.now().UTC()
	if !row.JobID.Valid || strings.TrimSpace(row.JobID.String) == "" {
		_, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET orbit_delete_status = ?, orbit_delete_error = NULL,
			    reconcile_after = NULL, updated_at = ? WHERE id = ? AND orbit_delete_status = ?
		`, DeleteNotRequired, now, derivativeID, DeletePending)
		return err
	}
	if !row.LogTail.Valid || strings.TrimSpace(row.LogTail.String) == "" {
		logs, err := m.orbitLogTail(ctx, row.JobID.String)
		if err != nil {
			return m.deferOrbitDelete(ctx, derivativeID, fmt.Errorf("capture Orbit logs before delete: %w", err))
		}
		result, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET orbit_log_tail = ?, orbit_delete_error = NULL,
			    orbit_delete_next_retry_at = NULL, reconcile_after = NULL, updated_at = ?
			WHERE id = ? AND orbit_delete_status = ? AND COALESCE(orbit_log_tail, '') = ?
		`, logs, now, derivativeID, DeletePending, row.LogTail.String)
		if err != nil {
			return fmt.Errorf("persist Orbit logs before delete: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check persisted Orbit logs before delete: %w", err)
		}
		if updated != 1 {
			return fmt.Errorf("persist Orbit logs before delete: derivative state changed")
		}
	}
	err := m.orbit.Delete(ctx, row.JobID.String)
	if err != nil && !errors.Is(err, orbitapi.ErrNotFound) {
		return m.deferOrbitDelete(ctx, derivativeID, fmt.Errorf("delete Orbit Job: %w", err))
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET orbit_delete_status = ?, orbit_delete_error = NULL,
		    orbit_delete_accepted_at = ?, orbit_delete_next_retry_at = NULL,
		    reconcile_after = NULL, updated_at = ? WHERE id = ? AND orbit_delete_status = ?
	`, DeleteCompleted, now, now, derivativeID, DeletePending); err != nil {
		return fmt.Errorf("persist accepted Orbit delete: %w", err)
	}
	return nil
}

func (m *Manager) deferOrbitDelete(ctx context.Context, derivativeID int64, cause error) error {
	now := m.now().UTC()
	_, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET orbit_delete_attempt_count = orbit_delete_attempt_count + 1,
		    orbit_delete_next_retry_at = ?, orbit_delete_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND orbit_delete_status = ?
	`, now.Add(m.pollInterval()), cause.Error(), now.Add(m.pollInterval()), now, derivativeID, DeletePending)
	if err != nil {
		return fmt.Errorf("persist Orbit delete retry after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) orbitLogTail(ctx context.Context, jobID string) (string, error) {
	if m.orbit == nil {
		return "", fmt.Errorf("Orbit is not configured")
	}
	logs, err := m.orbit.Logs(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("load Orbit logs: %w", err)
	}
	if strings.TrimSpace(logs) == "" {
		return "", fmt.Errorf("load Orbit logs: empty response")
	}
	return m.boundOrbitLogTail(logs), nil
}

func (m *Manager) terminalOrbitLogTail(previous sql.NullString, job orbitapi.Job, logErr error) string {
	logs := ""
	if previous.Valid {
		logs = previous.String
	}
	if logs != "" && !strings.HasSuffix(logs, "\n") {
		logs += "\n"
	}
	logs += fmt.Sprintf(
		"[keystone] Orbit terminal diagnostics\nstatus=%s\nmessage=%q\nlog_error=%q\n",
		strings.ToUpper(strings.TrimSpace(job.Status)),
		strings.TrimSpace(job.Message),
		logErr.Error(),
	)
	return m.boundOrbitLogTail(logs)
}

func (m *Manager) boundOrbitLogTail(logs string) string {
	limit := m.cfg.LogTailBytes
	if limit <= 0 || len(logs) <= limit {
		return logs
	}
	return logs[len(logs)-limit:]
}

func nullableLogTail(logs string, err error) any {
	if err != nil {
		return nil
	}
	return logs
}

func (m *Manager) pollInterval() time.Duration {
	if m.cfg.PollInterval <= 0 {
		return 5 * time.Second
	}
	return m.cfg.PollInterval
}

func (m *Manager) submissionAbsenceGrace() time.Duration {
	grace := m.pollInterval()
	if grace < 10*time.Second {
		return 10 * time.Second
	}
	return grace
}

func (m *Manager) activeJobMissingGrace() time.Duration {
	grace := 3 * m.pollInterval()
	if grace < 15*time.Second {
		return 15 * time.Second
	}
	return grace
}

func parseFrozenTOSURI(uri string) (string, string, error) {
	remainder, ok := strings.CutPrefix(strings.TrimSpace(uri), "tos://")
	if !ok {
		return "", "", fmt.Errorf("frozen source URI is not a TOS URI")
	}
	bucket, objectKey, ok := strings.Cut(remainder, "/")
	if !ok || strings.TrimSpace(bucket) == "" || strings.TrimSpace(objectKey) == "" {
		return "", "", fmt.Errorf("frozen source URI is incomplete")
	}
	return bucket, objectKey, nil
}

func randomOutputSuffix() (string, error) {
	var data [3]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func normalizedSHA256(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
