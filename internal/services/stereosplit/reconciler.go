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

	leftImageTopic  = "/decxin/left_rgb/compressed"
	rightImageTopic = "/decxin/right_rgb/compressed"
	imuTopic        = "/decxin/imu"

	compressedImageSchema = "sensor_msgs/msg/CompressedImage"
	imuSchema             = "sensor_msgs/msg/Imu"
)

var mcapMagic = []byte{0x89, 'M', 'C', 'A', 'P', '0', '\r', '\n'}

type reconcileEpisodeRow struct {
	ID                 int64          `db:"id"`
	StorageBackend     string         `db:"storage_backend"`
	McapPath           string         `db:"mcap_path"`
	Checksum           sql.NullString `db:"checksum"`
	Metadata           sql.NullString `db:"metadata"`
	CloudPublishSource sql.NullString `db:"cloud_publish_source"`
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
	var candidate frozenDerivativeRow
	err := m.db.GetContext(ctx, &candidate, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives
		WHERE kind = ?
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
		  AND (
		    processing_status IN (?, ?, ?, ?, ?)
		    OR (processing_status = ? AND qa_status IN (?, ?))
		    OR orbit_delete_status = ?
		  )
		ORDER BY CASE
		  WHEN processing_status IN ('submitting', 'pending', 'running', 'verifying') THEN CASE processing_status
		    WHEN 'submitting' THEN 0
		    WHEN 'pending' THEN 1
		    WHEN 'running' THEN 1
		    WHEN 'verifying' THEN 2
		    ELSE 3 END
		  WHEN processing_status = 'succeeded' AND qa_status IN ('pending', 'running') THEN 3
		  WHEN orbit_delete_status = 'pending' THEN 4
		  ELSE 5 END,
		  updated_at ASC, id ASC
		LIMIT 1
	`, Kind, m.now().UTC(), ProcessingQueued, ProcessingSubmitting, ProcessingPending, ProcessingRunning, ProcessingVerifying,
		ProcessingSucceeded, QAPending, QARunning, DeletePending)
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
			return false, nil
		}
		if err := m.freezeQueued(ctx, candidate); err != nil {
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

func (m *Manager) atOrbitCapacity(ctx context.Context) (bool, error) {
	current, err := m.CurrentImageConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("load stereo split concurrency setting: %w", err)
	}
	limit := configuredMaxConcurrent(current.MaxConcurrent)
	var active int
	if err := m.db.GetContext(ctx, &active, `
		SELECT COUNT(*) FROM episode_derivatives
		WHERE kind = ? AND processing_status IN (?, ?, ?)
	`, Kind, ProcessingSubmitting, ProcessingPending, ProcessingRunning); err != nil {
		return false, fmt.Errorf("count active stereo split jobs: %w", err)
	}
	return active >= limit, nil
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
		       orbit_submission_id, orbit_request, orbit_job_id,
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
	logs := m.orbitLogTail(ctx, job.JobID)
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
		`, job.JobID, ProcessingVerifying, logs, now, row.ID)
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
		`, job.JobID, processingStatus, message, logs, now, DeletePending, now, row.ID)
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
		SELECT id, storage_backend, mcap_path, checksum, metadata, cloud_publish_source
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
	submissionID := fmt.Sprintf("derivative-%d-stereo-split-g%d", candidate.ID, candidate.Generation)
	execution, err := m.PrepareExecution(ctx, ExecutionInput{
		SourceBucket:    bucket,
		SourceObjectKey: objectKey,
		SourceChecksum:  episode.Checksum.String,
		OutputScope:     path.Join(fmt.Sprintf("%d", candidate.EpisodeID), "stereo-split"),
		SubmissionID:    submissionID,
		Generation:      candidate.Generation,
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
		SELECT id, storage_backend, mcap_path, metadata, cloud_publish_source
		FROM episodes WHERE id = ? AND deleted_at IS NULL`+forUpdateClause(m.db), candidate.EpisodeID); err != nil {
		return fmt.Errorf("lock stereo split source episode: %w", err)
	}
	lockedBucket, lockedKey, err := normalizeEpisodeSource(lockedEpisode)
	if err != nil {
		return err
	}
	if lockedBucket != bucket || lockedKey != objectKey || strings.EqualFold(strings.TrimSpace(lockedEpisode.CloudPublishSource.String), CloudSourceOriginal) {
		return ErrCloudSourceLocked
	}
	now := m.now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processor_config_revision_id = ?, processor_image = ?,
		    source_uri = ?, source_etag = ?, source_checksum = NULLIF(?, ''),
		    source_size_bytes = ?, processing_status = ?,
		    orbit_submission_id = ?, orbit_request = ?, orbit_snapshot_frozen_at = ?,
		    output_prefix = ?, submit_attempt_count = 0, reconcile_after = NULL,
		    processing_error = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, execution.ProcessorConfigRevisionID, execution.ProcessorImage, execution.SourceURI,
		execution.SourceETag, execution.SourceChecksum, execution.SourceSizeBytes,
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
		       orbit_submission_id, orbit_request, orbit_job_id,
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
		logs := m.orbitLogTail(ctx, row.OrbitJobID.String)
		_, err = m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET processing_status = ?, reconcile_after = NULL,
			    orbit_job_missing_since = NULL, orbit_log_tail = ?, processing_error = NULL, updated_at = ? WHERE id = ?
		`, ProcessingVerifying, logs, now, derivativeID)
	case "FAILED", "STOPPED":
		logs := m.orbitLogTail(ctx, row.OrbitJobID.String)
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
		`, status, message, logs, now, DeletePending, now, derivativeID)
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
	Generation     int    `json:"generation"`
	ProcessorImage string `json:"processor_image"`
	Source         struct {
		URI       string `json:"uri"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	} `json:"source"`
	Outputs struct {
		MCAP     manifestOutput `json:"mcap"`
		Metadata manifestOutput `json:"metadata"`
	} `json:"outputs"`
	Stats      manifestStats `json:"stats"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
}

type manifestOutput struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type manifestStats struct {
	InputMessages   int64 `json:"input_messages"`
	DecodedImages   int64 `json:"decoded_images"`
	LeftImages      int64 `json:"left_images"`
	RightImages     int64 `json:"right_images"`
	IMUMessages     int64 `json:"imu_messages"`
	SkippedMessages int64 `json:"skipped_messages"`
}

type verificationRow struct {
	ID                       int64          `db:"id"`
	Generation               int            `db:"generation"`
	ProcessorImage           string         `db:"processor_image"`
	SourceURI                string         `db:"source_uri"`
	SourceETag               string         `db:"source_etag"`
	SourceChecksum           sql.NullString `db:"source_checksum"`
	SourceSize               int64          `db:"source_size_bytes"`
	OutputPrefix             string         `db:"output_prefix"`
	Status                   string         `db:"processing_status"`
	VerificationAttemptCount int            `db:"verification_attempt_count"`
}

func (m *Manager) verifySucceeded(ctx context.Context, derivativeID int64) error {
	var row verificationRow
	if err := m.db.GetContext(ctx, &row, `
		SELECT id, generation, processor_image, source_uri, source_etag, source_checksum,
		       source_size_bytes, output_prefix, processing_status, verification_attempt_count
		FROM episode_derivatives WHERE id = ? AND kind = ?
	`, derivativeID, Kind); err != nil {
		return fmt.Errorf("load verifying stereo split: %w", err)
	}
	if row.Status != ProcessingVerifying {
		return nil
	}
	output, err := m.VerifyExecution(ctx, ExecutionSnapshot{
		Generation:      row.Generation,
		ProcessorImage:  row.ProcessorImage,
		SourceURI:       row.SourceURI,
		SourceETag:      row.SourceETag,
		SourceChecksum:  row.SourceChecksum.String,
		SourceSizeBytes: row.SourceSize,
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
		    file_size_bytes = ?, duration_sec = ?, processing_result = ?,
		    processing_status = ?, processing_error = NULL, processing_finished_at = ?,
		    qa_status = ?, qa_next_retry_at = NULL, orbit_delete_status = ?,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND processing_status = ?
	`, output.MCAPObjectKey, output.MetadataObjectKey, output.ManifestObjectKey, output.MCAPChecksumSHA256,
		output.MCAPSizeBytes, output.ProcessingDurationSec, output.ManifestJSON,
		ProcessingSucceeded, now, QAPending, DeletePending, now,
		derivativeID, ProcessingVerifying)
	if err != nil {
		return fmt.Errorf("persist verified stereo split output: %w", err)
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

func validateManifestSnapshot(manifest processingManifest, row verificationRow) error {
	if manifest.SchemaVersion != 1 || manifest.Status != "succeeded" || manifest.Kind != Kind ||
		manifest.Generation != row.Generation || manifest.ProcessorImage != row.ProcessorImage ||
		manifest.Source.URI != row.SourceURI || manifest.Source.SizeBytes != row.SourceSize {
		return fmt.Errorf("processing manifest does not match frozen execution snapshot")
	}
	if row.SourceChecksum.Valid && row.SourceChecksum.String != "" && manifest.Source.SHA256 != row.SourceChecksum.String {
		return fmt.Errorf("processing manifest source checksum does not match snapshot")
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
		Status   string         `db:"processing_status"`
		QAStatus string         `db:"qa_status"`
		McapPath sql.NullString `db:"mcap_path"`
		Checksum sql.NullString `db:"checksum"`
		Result   string         `db:"processing_result"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT processing_status, qa_status, mcap_path, checksum, processing_result
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
	if err == nil {
		err = validateManifestStats(manifest.Stats)
	}
	if err == nil {
		observed, err = m.inspectOutputMCAP(ctx, row.McapPath.String, row.Checksum.String, manifest.Stats)
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
	LeftImages   int64   `json:"left_images"`
	RightImages  int64   `json:"right_images"`
	IMUMessages  int64   `json:"imu_messages"`
	FirstLogTime uint64  `json:"first_log_time"`
	LastLogTime  uint64  `json:"last_log_time"`
	DurationSec  float64 `json:"duration_sec"`
	OutputSHA256 string  `json:"output_sha256"`
}

type topicQAState struct {
	Count   int64
	Last    uint64
	HasLast bool
}

func validateManifestStats(stats manifestStats) error {
	if stats.InputMessages <= 0 || stats.DecodedImages <= 0 || stats.LeftImages <= 0 ||
		stats.RightImages <= 0 || stats.IMUMessages <= 0 || stats.SkippedMessages < 0 {
		return fmt.Errorf("processing manifest contains non-positive required statistics")
	}
	if stats.InputMessages != stats.DecodedImages+stats.SkippedMessages {
		return fmt.Errorf("processing manifest input message accounting is inconsistent")
	}
	if stats.DecodedImages != stats.LeftImages || stats.LeftImages != stats.RightImages {
		return fmt.Errorf("processing manifest stereo image counts are inconsistent")
	}
	return nil
}

func (m *Manager) inspectOutputMCAP(
	ctx context.Context,
	objectKey string,
	expectedChecksum string,
	expected manifestStats,
) (mcapQAObservation, error) {
	if strings.TrimSpace(objectKey) == "" {
		return mcapQAObservation{}, fmt.Errorf("stereo split output MCAP path is empty")
	}
	body, err := m.objects.OpenObject(ctx, m.cfg.OutputBucket, objectKey)
	if err != nil {
		return mcapQAObservation{}, fmt.Errorf("open stereo split output for QA: %w", err)
	}
	digest := sha256.New()
	reader, err := mcap.NewReader(io.TeeReader(body, digest))
	if err != nil {
		_ = body.Close()
		return mcapQAObservation{}, fmt.Errorf("open output MCAP reader: %w", err)
	}
	defer reader.Close()

	iterator, err := reader.Messages(mcap.UsingIndex(false))
	if err != nil {
		_ = body.Close()
		return mcapQAObservation{}, fmt.Errorf("create output MCAP iterator: %w", err)
	}
	states := map[string]*topicQAState{
		leftImageTopic:  {},
		rightImageTopic: {},
		imuTopic:        {},
	}
	expectedSchemas := map[string]string{
		leftImageTopic:  compressedImageSchema,
		rightImageTopic: compressedImageSchema,
		imuTopic:        imuSchema,
	}
	var firstLogTime uint64
	var lastLogTime uint64
	var hasLogTime bool
	message := &mcap.Message{}
	for {
		schema, channel, current, nextErr := iterator.NextInto(message)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("read output MCAP message: %w", nextErr)
		}
		state, required := states[channel.Topic]
		if !required {
			continue
		}
		if schema == nil || schema.Name != expectedSchemas[channel.Topic] {
			_ = body.Close()
			return mcapQAObservation{}, fmt.Errorf("topic %s has unexpected schema", channel.Topic)
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
	if err := body.Close(); err != nil {
		return mcapQAObservation{}, fmt.Errorf("close output MCAP after QA: %w", err)
	}
	checksum := hex.EncodeToString(digest.Sum(nil))
	if checksum != normalizedSHA256(expectedChecksum) {
		return mcapQAObservation{}, fmt.Errorf("output MCAP SHA-256 does not match processing manifest")
	}
	leftCount := states[leftImageTopic].Count
	rightCount := states[rightImageTopic].Count
	imuCount := states[imuTopic].Count
	if leftCount <= 0 || leftCount != rightCount || imuCount <= 0 {
		return mcapQAObservation{}, fmt.Errorf("output MCAP required topic counts are invalid")
	}
	if leftCount != expected.LeftImages || rightCount != expected.RightImages || imuCount != expected.IMUMessages {
		return mcapQAObservation{}, fmt.Errorf("output MCAP topic counts do not match processing manifest")
	}
	if !hasLogTime || lastLogTime <= firstLogTime {
		return mcapQAObservation{}, fmt.Errorf("output MCAP timestamp span must be positive")
	}
	return mcapQAObservation{
		LeftImages:   leftCount,
		RightImages:  rightCount,
		IMUMessages:  imuCount,
		FirstLogTime: firstLogTime,
		LastLogTime:  lastLogTime,
		DurationSec:  float64(lastLogTime-firstLogTime) / float64(time.Second),
		OutputSHA256: checksum,
	}, nil
}

func (m *Manager) reconcileDelete(ctx context.Context, derivativeID int64) error {
	var row struct {
		JobID  sql.NullString `db:"orbit_job_id"`
		Status string         `db:"orbit_delete_status"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT orbit_job_id, orbit_delete_status FROM episode_derivatives
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
	err := m.orbit.Delete(ctx, row.JobID.String)
	if err != nil && !errors.Is(err, orbitapi.ErrNotFound) {
		_, persistErr := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives SET orbit_delete_attempt_count = orbit_delete_attempt_count + 1,
			    orbit_delete_next_retry_at = ?, orbit_delete_error = ?, reconcile_after = ?, updated_at = ?
			WHERE id = ? AND orbit_delete_status = ?
		`, now.Add(m.pollInterval()), err.Error(), now.Add(m.pollInterval()), now, derivativeID, DeletePending)
		if persistErr != nil {
			return fmt.Errorf("persist Orbit delete retry after %v: %w", err, persistErr)
		}
		return fmt.Errorf("delete Orbit Job: %w", err)
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

func (m *Manager) orbitLogTail(ctx context.Context, jobID string) string {
	logs, err := m.orbit.Logs(ctx, jobID)
	if err != nil {
		return ""
	}
	limit := m.cfg.LogTailBytes
	if limit <= 0 || len(logs) <= limit {
		return logs
	}
	return logs[len(logs)-limit:]
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
