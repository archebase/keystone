// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
)

const (
	processingCommand = "/app/run_calibration.py"
	resultFileName    = "result.json"
)

// ReconcileOnce advances at most one durable Capture state transition.
func (m *Manager) ReconcileOnce(ctx context.Context) (bool, error) {
	if m == nil || m.db == nil {
		return false, nil
	}
	var candidate struct {
		ID              int64  `db:"id"`
		Status          string `db:"status"`
		CancelRequested bool   `db:"cancel_requested"`
	}
	err := m.db.GetContext(ctx, &candidate, `
		SELECT id, status, cancel_requested_at IS NOT NULL AS cancel_requested
		FROM calibration_captures
		WHERE status IN (?, ?, ?, ?, ?)
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
		ORDER BY CASE
		  WHEN cancel_requested_at IS NOT NULL THEN 0
		  WHEN status IN ('submitting', 'pending', 'running') THEN 1
		  WHEN status = 'verifying' THEN 2
		  ELSE 3 END,
		  updated_at, id
		LIMIT 1
	`, StatusQueued, StatusSubmitting, StatusPending, StatusRunning, StatusVerifying, m.now().UTC())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("select calibration reconciliation candidate: %w", err)
	}
	if candidate.CancelRequested {
		return true, m.reconcileCancellation(ctx, candidate.ID)
	}

	switch candidate.Status {
	case StatusQueued:
		m.configMu.RLock()
		defer m.configMu.RUnlock()
		atCapacity, err := m.atOrbitCapacity(ctx)
		if err != nil {
			return true, err
		}
		if atCapacity {
			return false, nil
		}
		return true, m.freezeQueued(ctx, candidate.ID)
	case StatusSubmitting:
		return true, m.reconcileSubmitting(ctx, candidate.ID)
	case StatusPending, StatusRunning:
		return true, m.reconcileOrbitStatus(ctx, candidate.ID)
	case StatusVerifying:
		return true, m.verifyResult(ctx, candidate.ID)
	default:
		return false, nil
	}
}

func (m *Manager) atOrbitCapacity(ctx context.Context) (bool, error) {
	current, err := m.CurrentProcessingConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("load calibration concurrency setting: %w", err)
	}
	var active int
	if err := m.db.GetContext(ctx, &active, `
		SELECT COUNT(*) FROM calibration_captures
		WHERE status IN (?, ?, ?)
	`, StatusSubmitting, StatusPending, StatusRunning); err != nil {
		return false, fmt.Errorf("count active calibration jobs: %w", err)
	}
	return active >= current.MaxConcurrent, nil
}

func (m *Manager) freezeQueued(ctx context.Context, capturePK int64) error {
	if m.orbit == nil {
		return m.failCapture(ctx, capturePK, StatusQueued, "Orbit is not configured")
	}
	if m.objects == nil {
		return m.failCapture(ctx, capturePK, StatusQueued, "TOS object reader is not configured")
	}
	currentConfig, err := m.CurrentProcessingConfig(ctx)
	if err != nil {
		return err
	}
	if currentConfig.ImageRef == "" {
		return m.deferCapture(ctx, capturePK, StatusQueued, ErrImageNotConfigured)
	}
	image, err := validateProcessingImageRef(currentConfig.ImageRef)
	if err != nil {
		return fmt.Errorf("current calibration image is invalid: %w", err)
	}
	var capture Capture
	if err := m.db.GetContext(ctx, &capture, captureSelect+` WHERE c.id = ?`, capturePK); err != nil {
		return fmt.Errorf("load queued calibration capture: %w", err)
	}
	sourceSize, sourceETag, err := m.objects.StatObject(ctx, capture.Bucket, capture.ObjectKey)
	if err != nil {
		return m.deferCapture(ctx, capturePK, StatusQueued, fmt.Errorf("stat calibration MCAP: %w", err))
	}
	if sourceSize <= 0 || sourceSize != capture.FileSizeBytes || strings.TrimSpace(sourceETag) == "" {
		return m.failCapture(ctx, capturePK, StatusQueued, "calibration MCAP identity does not match upload completion")
	}

	sourceURI := "tos://" + capture.Bucket + "/" + capture.ObjectKey
	resultPrefix := path.Join("calibration-results", capture.DeviceID, capture.CalibrationSessionID, capture.CaptureID)
	outputURI := "tos://" + capture.Bucket + "/" + resultPrefix + "/"
	inputPath := "/bindings/input/capture.mcap"
	submissionID := "calibration-" + capture.CaptureID
	backoffLimit := int32(0)
	ttlSeconds := m.cfg.TTLSecondsAfterDone
	deadline := m.cfg.ActiveDeadline
	request := orbitapi.SubmitRequest{
		SubmissionID: submissionID,
		Image:        image,
		Command:      []string{"python3", processingCommand},
		Args: []string{
			"--input", inputPath,
			"--output", "/bindings/output/" + resultFileName,
			"--calibration-session-id", capture.CalibrationSessionID,
			"--capture-id", capture.CaptureID,
			"--expected-source-size", strconv.FormatInt(sourceSize, 10),
			"--expected-source-checksum", strings.ToLower(capture.ChecksumSHA256),
			"--source-uri", sourceURI,
			"--processor-image", image,
		},
		DataBindings: []orbitapi.DataBinding{
			{URI: sourceURI, Path: inputPath, Mode: "read"},
			{URI: outputURI, Path: "/bindings/output/", Mode: "write"},
		},
		Resources: orbitapi.Resources{
			Requests: cloneStringMap(m.cfg.Resources.Requests),
			Limits:   cloneStringMap(m.cfg.Resources.Limits),
		},
		TTLSecondsAfterDone:  &ttlSeconds,
		BackoffLimit:         &backoffLimit,
		ActiveDeadlineSecond: &deadline,
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return m.failCapture(ctx, capturePK, StatusQueued, "encode Orbit request failed")
	}
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, processor_config_revision_id = ?, processor_image = ?, source_etag = ?,
		    orbit_submission_id = ?, orbit_request = ?, reconcile_after = NULL,
		    calibration_error = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, StatusSubmitting, currentConfig.ID, image, strings.TrimSpace(sourceETag), submissionID, string(requestJSON), now, capturePK, StatusQueued)
	if err != nil {
		return fmt.Errorf("freeze calibration Orbit request: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read calibration freeze result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("calibration capture changed while freezing Orbit request")
	}
	return nil
}

func (m *Manager) reconcileSubmitting(ctx context.Context, capturePK int64) error {
	if m.orbit == nil {
		return m.deferCapture(ctx, capturePK, StatusSubmitting, errors.New("Orbit is not configured"))
	}
	var row struct {
		Status       string `db:"status"`
		SubmissionID string `db:"orbit_submission_id"`
		RequestJSON  string `db:"orbit_request"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT status, COALESCE(orbit_submission_id, '') AS orbit_submission_id,
		       COALESCE(orbit_request, '') AS orbit_request
		FROM calibration_captures WHERE id = ?
	`, capturePK); err != nil {
		return fmt.Errorf("load submitting calibration capture: %w", err)
	}
	if row.Status != StatusSubmitting {
		return nil
	}
	var request orbitapi.SubmitRequest
	if row.SubmissionID == "" || json.Unmarshal([]byte(row.RequestJSON), &request) != nil ||
		request.SubmissionID != row.SubmissionID {
		return m.failCapture(ctx, capturePK, StatusSubmitting, "frozen Orbit request is invalid")
	}
	submitting, err := m.recordSubmissionAttempt(ctx, capturePK)
	if err != nil {
		return err
	}
	if !submitting {
		return m.reconcileCancellation(ctx, capturePK)
	}
	response, err := m.orbit.Submit(ctx, request)
	if err != nil {
		if errors.Is(err, orbitapi.ErrConflict) {
			job, getErr := m.orbit.Get(ctx, request.SubmissionID)
			if getErr == nil {
				return m.markOrbitAccepted(ctx, capturePK, request, job)
			}
		}
		return m.deferCapture(ctx, capturePK, StatusSubmitting, fmt.Errorf("submit Orbit job: %w", err))
	}
	if response.SubmissionID != request.SubmissionID || strings.TrimSpace(response.JobID) == "" {
		return m.failCapture(ctx, capturePK, StatusSubmitting, "Orbit submit response has mismatched identity")
	}
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, orbit_job_id = ?, reconcile_after = ?,
		    orbit_submit_absent_at = NULL, calibration_error = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, StatusPending, response.JobID, now, now, capturePK, StatusSubmitting); err != nil {
		return fmt.Errorf("persist accepted calibration Orbit job: %w", err)
	}
	return nil
}

func (m *Manager) recordSubmissionAttempt(ctx context.Context, capturePK int64) (bool, error) {
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET submit_attempt_count = submit_attempt_count + 1,
		    orbit_submit_absent_at = NULL, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND status = ? AND cancel_requested_at IS NULL
	`, now.Add(m.pollInterval()), now, capturePK, StatusSubmitting)
	if err != nil {
		return false, fmt.Errorf("record calibration Orbit submission attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read calibration Orbit submission attempt result: %w", err)
	}
	return rows == 1, nil
}

func (m *Manager) markOrbitAccepted(ctx context.Context, capturePK int64, request orbitapi.SubmitRequest, job orbitapi.Job) error {
	if err := validateOrbitJob(job, request); err != nil {
		return m.failCapture(ctx, capturePK, StatusSubmitting, err.Error())
	}
	now := m.now().UTC()
	_, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, orbit_job_id = ?, reconcile_after = ?,
		    orbit_submit_absent_at = NULL, calibration_error = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, StatusPending, job.JobID, now, now, capturePK, StatusSubmitting)
	if err != nil {
		return fmt.Errorf("adopt calibration Orbit job: %w", err)
	}
	return nil
}

func (m *Manager) reconcileOrbitStatus(ctx context.Context, capturePK int64) error {
	var row struct {
		Status              string       `db:"status"`
		JobID               string       `db:"orbit_job_id"`
		RequestJSON         string       `db:"orbit_request"`
		OrbitSubmitAbsentAt sql.NullTime `db:"orbit_submit_absent_at"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT status, COALESCE(orbit_job_id, '') AS orbit_job_id,
		       COALESCE(orbit_request, '') AS orbit_request,
		       orbit_submit_absent_at
		FROM calibration_captures WHERE id = ?
	`, capturePK); err != nil {
		return fmt.Errorf("load active calibration Orbit job: %w", err)
	}
	var request orbitapi.SubmitRequest
	if row.JobID == "" || json.Unmarshal([]byte(row.RequestJSON), &request) != nil {
		return m.failCapture(ctx, capturePK, row.Status, "active calibration Orbit identity is invalid")
	}
	job, err := m.orbit.Get(ctx, row.JobID)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			// Submission adoption always clears this timestamp, so it can also
			// durably track the first absence observed while the Job is active.
			return m.reconcileMissingActiveJob(ctx, capturePK, row.Status, row.OrbitSubmitAbsentAt)
		}
		return m.deferCapture(ctx, capturePK, row.Status, fmt.Errorf("query calibration Orbit job: %w", err))
	}
	if row.OrbitSubmitAbsentAt.Valid {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE calibration_captures
			SET orbit_submit_absent_at = NULL, updated_at = ?
			WHERE id = ? AND status = ?
		`, m.now().UTC(), capturePK, row.Status); err != nil {
			return fmt.Errorf("clear recovered calibration Orbit absence: %w", err)
		}
	}
	if err := validateOrbitJob(job, request); err != nil {
		return m.failCapture(ctx, capturePK, row.Status, err.Error())
	}
	now := m.now().UTC()
	next := now.Add(m.pollInterval())
	switch strings.ToUpper(strings.TrimSpace(job.Status)) {
	case "PENDING":
		_, err = m.db.ExecContext(ctx, `
			UPDATE calibration_captures SET status = ?, reconcile_after = ?,
			    calibration_error = NULL, updated_at = ?
			WHERE id = ? AND status IN (?, ?)
		`, StatusPending, next, now, capturePK, StatusPending, StatusRunning)
	case "RUNNING":
		_, err = m.db.ExecContext(ctx, `
			UPDATE calibration_captures SET status = ?,
			    processing_started_at = COALESCE(processing_started_at, ?),
			    reconcile_after = ?, calibration_error = NULL, updated_at = ?
			WHERE id = ? AND status IN (?, ?)
		`, StatusRunning, now, next, now, capturePK, StatusPending, StatusRunning)
	case "SUCCEEDED":
		logs := m.orbitLogTail(ctx, row.JobID)
		_, err = m.db.ExecContext(ctx, `
			UPDATE calibration_captures SET status = ?, orbit_log_tail = ?,
			    reconcile_after = NULL, calibration_error = NULL, updated_at = ?
			WHERE id = ? AND status IN (?, ?)
		`, StatusVerifying, logs, now, capturePK, StatusPending, StatusRunning)
	case "FAILED", "STOPPED":
		message := strings.TrimSpace(job.Message)
		if message == "" {
			message = "Orbit Job ended with status " + strings.ToUpper(job.Status)
		}
		logs := m.orbitLogTail(ctx, row.JobID)
		_, err = m.db.ExecContext(ctx, `
			UPDATE calibration_captures SET status = ?, calibration_error = ?,
			    orbit_log_tail = ?, processing_finished_at = ?,
			    reconcile_after = NULL, updated_at = ?
			WHERE id = ? AND status IN (?, ?)
		`, StatusFailed, message, logs, now, now, capturePK, StatusPending, StatusRunning)
	default:
		return m.deferCapture(ctx, capturePK, row.Status, fmt.Errorf("Orbit returned unknown status %q", job.Status))
	}
	if err != nil {
		return fmt.Errorf("persist calibration Orbit status: %w", err)
	}
	return nil
}

func (m *Manager) reconcileMissingActiveJob(
	ctx context.Context,
	capturePK int64,
	status string,
	absentAt sql.NullTime,
) error {
	now := m.now().UTC()
	if !absentAt.Valid {
		message := "Orbit Job is temporarily absent"
		if _, err := m.db.ExecContext(ctx, `
			UPDATE calibration_captures
			SET orbit_submit_absent_at = ?, calibration_error = ?,
			    reconcile_after = ?, updated_at = ?
			WHERE id = ? AND status = ?
		`, now, message, now.Add(m.pollInterval()), now, capturePK, status); err != nil {
			return fmt.Errorf("persist missing calibration Orbit Job: %w", err)
		}
		return errors.New(message)
	}
	if now.Before(absentAt.Time.Add(m.activeJobMissingGrace())) {
		return m.deferCapture(ctx, capturePK, status, errors.New("Orbit Job remains absent"))
	}
	return m.failCapture(ctx, capturePK, status, "Orbit Job disappeared before a terminal status was observed")
}

type calibrationResult struct {
	SchemaVersion        int             `json:"schema_version"`
	Status               string          `json:"status"`
	AlgorithmVersion     string          `json:"algorithm_version"`
	CalibrationSessionID string          `json:"calibration_session_id"`
	CaptureID            string          `json:"capture_id"`
	ProcessorImage       string          `json:"processor_image"`
	ErrorMessage         string          `json:"error_message"`
	Result               json.RawMessage `json:"result"`
	Source               struct {
		URI       string `json:"uri"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	} `json:"source"`
}

func (m *Manager) verifyResult(ctx context.Context, capturePK int64) error {
	var capture Capture
	if err := m.db.GetContext(ctx, &capture, captureSelect+` WHERE c.id = ?`, capturePK); err != nil {
		return fmt.Errorf("load verifying calibration capture: %w", err)
	}
	sourceSize, sourceETag, err := m.objects.StatObject(ctx, capture.Bucket, capture.ObjectKey)
	if err != nil {
		return m.deferCapture(ctx, capturePK, StatusVerifying, fmt.Errorf("stat frozen calibration MCAP: %w", err))
	}
	if sourceSize != capture.FileSizeBytes || strings.TrimSpace(sourceETag) != capture.SourceETag {
		return m.failCapture(ctx, capturePK, StatusVerifying, "calibration MCAP identity changed during processing")
	}
	resultKey := path.Join("calibration-results", capture.DeviceID, capture.CalibrationSessionID, capture.CaptureID, resultFileName)
	resultSize, _, err := m.objects.StatObject(ctx, capture.Bucket, resultKey)
	if err != nil {
		return m.deferCapture(ctx, capturePK, StatusVerifying, fmt.Errorf("stat calibration result JSON: %w", err))
	}
	if resultSize <= 0 || (m.cfg.MaxResultBytes > 0 && resultSize > m.cfg.MaxResultBytes) {
		return m.failCapture(ctx, capturePK, StatusVerifying, "calibration result JSON size is invalid")
	}
	body, err := m.objects.OpenObject(ctx, capture.Bucket, resultKey)
	if err != nil {
		return m.deferCapture(ctx, capturePK, StatusVerifying, fmt.Errorf("open calibration result JSON: %w", err))
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, resultSize+1))
	if err != nil {
		return m.deferCapture(ctx, capturePK, StatusVerifying, fmt.Errorf("read calibration result JSON: %w", err))
	}
	if int64(len(data)) != resultSize {
		return m.deferCapture(ctx, capturePK, StatusVerifying, errors.New("calibration result JSON size changed while reading"))
	}
	var result calibrationResult
	if err := json.Unmarshal(data, &result); err != nil {
		return m.failCapture(ctx, capturePK, StatusVerifying, "calibration result JSON is invalid")
	}
	sourceURI := "tos://" + capture.Bucket + "/" + capture.ObjectKey
	if result.SchemaVersion != 1 ||
		(result.Status != StatusSucceeded && result.Status != StatusFailed) ||
		strings.TrimSpace(result.AlgorithmVersion) == "" ||
		result.CalibrationSessionID != capture.CalibrationSessionID ||
		result.CaptureID != capture.CaptureID ||
		result.ProcessorImage != capture.ProcessorImage ||
		result.Source.URI != sourceURI ||
		result.Source.SizeBytes != capture.FileSizeBytes ||
		!strings.EqualFold(result.Source.SHA256, capture.ChecksumSHA256) {
		return m.failCapture(ctx, capturePK, StatusVerifying, "calibration result JSON does not match the frozen Capture")
	}
	if result.Status == StatusSucceeded && len(result.Result) == 0 {
		return m.failCapture(ctx, capturePK, StatusVerifying, "successful calibration result is missing result data")
	}
	digest := sha256.Sum256(data)
	return m.persistVerifiedResult(ctx, capture, resultKey, data, result, hex.EncodeToString(digest[:]))
}

func (m *Manager) persistVerifiedResult(
	ctx context.Context,
	capture Capture,
	resultKey string,
	data []byte,
	result calibrationResult,
	checksum string,
) error {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin calibration result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var session struct {
		Status              string         `db:"status"`
		SuccessfulCaptureID sql.NullString `db:"successful_capture_id"`
	}
	if err := tx.GetContext(ctx, &session, `
		SELECT status, successful_capture_id
		FROM calibration_sessions WHERE session_id = ?`+forUpdateClause(m.db),
		capture.CalibrationSessionID); err != nil {
		return fmt.Errorf("lock calibration session result: %w", err)
	}
	var captureStatus string
	if err := tx.GetContext(ctx, &captureStatus, `
		SELECT status FROM calibration_captures WHERE id = ?`+forUpdateClause(m.db), capture.ID); err != nil {
		return fmt.Errorf("lock verified calibration capture: %w", err)
	}
	if captureStatus != StatusVerifying {
		return nil
	}
	finalStatus := result.Status
	errorMessage := strings.TrimSpace(result.ErrorMessage)
	if result.Status == StatusSucceeded && session.Status == SessionSucceeded &&
		session.SuccessfulCaptureID.String != capture.CaptureID {
		finalStatus = StatusSuperseded
	}
	now := m.now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, result_object_key = ?, result_size_bytes = ?,
		    result_checksum_sha256 = ?, result_json = ?, algorithm_version = ?,
		    calibration_error = NULLIF(?, ''), processing_finished_at = ?,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, finalStatus, resultKey, len(data), checksum, string(data), result.AlgorithmVersion,
		errorMessage, now, now, capture.ID, StatusVerifying); err != nil {
		return fmt.Errorf("persist calibration result: %w", err)
	}
	if finalStatus == StatusSucceeded && session.Status == SessionRunning {
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_sessions
			SET status = ?, successful_capture_id = ?, updated_at = ?
			WHERE session_id = ? AND status = ?
		`, SessionSucceeded, capture.CaptureID, now, capture.CalibrationSessionID, SessionRunning); err != nil {
			return fmt.Errorf("succeed calibration session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_captures
			SET status = ?, calibration_error = ?, processing_finished_at = ?,
			    reconcile_after = NULL, updated_at = ?
			WHERE calibration_session_id = ? AND id <> ? AND status IN (?, ?, ?)
		`, StatusSuperseded, "superseded by successful capture "+capture.CaptureID, now, now,
			capture.CalibrationSessionID, capture.ID, StatusUploaded, StatusQueued, StatusVerifying); err != nil {
			return fmt.Errorf("supersede queued calibration captures: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_captures
			SET cancel_requested_at = ?, calibration_error = ?, reconcile_after = ?, updated_at = ?
			WHERE calibration_session_id = ? AND id <> ? AND status IN (?, ?, ?)
		`, now, "superseded by successful capture "+capture.CaptureID, now, now,
			capture.CalibrationSessionID, capture.ID, StatusSubmitting, StatusPending, StatusRunning); err != nil {
			return fmt.Errorf("request cancellation of active calibration captures: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_sessions SET updated_at = ? WHERE session_id = ?
		`, now, capture.CalibrationSessionID); err != nil {
			return fmt.Errorf("update calibration session result time: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit calibration result transaction: %w", err)
	}
	if finalStatus == StatusSucceeded && session.Status == SessionRunning {
		m.wakeReconciler()
	}
	return nil
}

func (m *Manager) reconcileCancellation(ctx context.Context, capturePK int64) error {
	var row struct {
		Status              string       `db:"status"`
		SubmissionID        string       `db:"orbit_submission_id"`
		JobID               string       `db:"orbit_job_id"`
		RequestJSON         string       `db:"orbit_request"`
		CancelRequestedAt   sql.NullTime `db:"cancel_requested_at"`
		SubmitAttemptCount  int          `db:"submit_attempt_count"`
		OrbitSubmitAbsentAt sql.NullTime `db:"orbit_submit_absent_at"`
	}
	if err := m.db.GetContext(ctx, &row, `
		SELECT status,
		       COALESCE(orbit_submission_id, '') AS orbit_submission_id,
		       COALESCE(orbit_job_id, '') AS orbit_job_id,
		       COALESCE(orbit_request, '') AS orbit_request,
		       cancel_requested_at, submit_attempt_count, orbit_submit_absent_at
		FROM calibration_captures WHERE id = ?
	`, capturePK); err != nil {
		return fmt.Errorf("load calibration cancellation: %w", err)
	}
	if !row.CancelRequestedAt.Valid {
		return nil
	}
	lookupID := strings.TrimSpace(row.JobID)
	if lookupID == "" {
		lookupID = strings.TrimSpace(row.SubmissionID)
	}
	if lookupID == "" {
		if row.SubmitAttemptCount > 0 {
			return m.deferCapture(ctx, capturePK, row.Status,
				errors.New("canceled calibration submission has no Orbit identity"))
		}
		return m.finishCanceledCapture(ctx, capturePK, "", "superseded before Orbit submission")
	}
	job, err := m.orbit.Get(ctx, lookupID)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			if row.Status == StatusSubmitting {
				confirmed, confirmErr := m.confirmCanceledSubmissionAbsent(ctx, capturePK, row.SubmitAttemptCount, row.OrbitSubmitAbsentAt)
				if confirmErr != nil || !confirmed {
					return confirmErr
				}
			}
			return m.finishCanceledCapture(ctx, capturePK, row.JobID, "superseded; Orbit Job is absent")
		}
		return m.deferCapture(ctx, capturePK, row.Status, fmt.Errorf("query Orbit Job for cancellation: %w", err))
	}
	if row.RequestJSON != "" {
		var request orbitapi.SubmitRequest
		if json.Unmarshal([]byte(row.RequestJSON), &request) != nil {
			return m.deferCapture(ctx, capturePK, row.Status, errors.New("canceled calibration capture has invalid Orbit request"))
		}
		if err := validateOrbitJob(job, request); err != nil {
			return m.deferCapture(ctx, capturePK, row.Status, err)
		}
	}
	if isTerminalOrbitStatus(job.Status) {
		return m.finishCanceledCapture(ctx, capturePK, job.JobID, "superseded; Orbit Job ended with status "+strings.ToUpper(job.Status))
	}
	stopped, err := m.orbit.Stop(ctx, job.JobID)
	if err != nil {
		if errors.Is(err, orbitapi.ErrNotFound) {
			return m.finishCanceledCapture(ctx, capturePK, job.JobID, "superseded; Orbit Job disappeared during cancellation")
		}
		return m.deferCapture(ctx, capturePK, row.Status, fmt.Errorf("stop Orbit Job: %w", err))
	}
	if strings.TrimSpace(stopped.JobID) == "" {
		stopped.JobID = job.JobID
	}
	if !isTerminalOrbitStatus(stopped.Status) {
		return m.deferCapture(ctx, capturePK, row.Status,
			fmt.Errorf("Orbit stop has not reached a terminal status: %s", stopped.Status))
	}
	return m.finishCanceledCapture(ctx, capturePK, stopped.JobID,
		"superseded; Orbit Job ended with status "+strings.ToUpper(stopped.Status))
}

func (m *Manager) confirmCanceledSubmissionAbsent(
	ctx context.Context,
	capturePK int64,
	submitAttemptCount int,
	absentAt sql.NullTime,
) (bool, error) {
	if submitAttemptCount == 0 {
		return true, nil
	}
	now := m.now().UTC()
	if !absentAt.Valid {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE calibration_captures
			SET orbit_submit_absent_at = ?, calibration_error = ?, reconcile_after = ?, updated_at = ?
			WHERE id = ? AND cancel_requested_at IS NOT NULL
		`, now, "waiting to confirm canceled Orbit submission is absent",
			now.Add(m.cancellationAbsenceGrace()), now, capturePK); err != nil {
			return false, fmt.Errorf("persist canceled calibration Orbit absence observation: %w", err)
		}
		return false, nil
	}
	confirmAt := absentAt.Time.Add(m.cancellationAbsenceGrace())
	if now.Before(confirmAt) {
		return false, m.deferCancellation(ctx, capturePK, confirmAt,
			"waiting to confirm canceled Orbit submission is absent")
	}
	return true, nil
}

func (m *Manager) deferCancellation(ctx context.Context, capturePK int64, retryAt time.Time, message string) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET calibration_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND cancel_requested_at IS NOT NULL
	`, message, retryAt, now, capturePK); err != nil {
		return fmt.Errorf("defer calibration cancellation: %w", err)
	}
	return nil
}

func (m *Manager) finishCanceledCapture(ctx context.Context, capturePK int64, jobID, message string) error {
	now := m.now().UTC()
	logs := m.orbitLogTail(ctx, jobID)
	if _, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, orbit_job_id = NULLIF(?, ''), orbit_log_tail = ?,
		    calibration_error = ?, cancel_requested_at = NULL,
		    orbit_submit_absent_at = NULL,
		    processing_finished_at = ?, reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND cancel_requested_at IS NOT NULL
	`, StatusSuperseded, strings.TrimSpace(jobID), logs, message, now, now, capturePK); err != nil {
		return fmt.Errorf("finish canceled calibration capture: %w", err)
	}
	return nil
}

func (m *Manager) cancellationAbsenceGrace() time.Duration {
	grace := 2 * m.pollInterval()
	if grace < 15*time.Second {
		return 15 * time.Second
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

func isTerminalOrbitStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCEEDED", "FAILED", "STOPPED":
		return true
	default:
		return false
	}
}

func (m *Manager) deferCapture(ctx context.Context, capturePK int64, expectedStatus string, cause error) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET calibration_error = ?, reconcile_after = ?, updated_at = ?
		WHERE id = ? AND status = ?
	`, cause.Error(), now.Add(m.pollInterval()), now, capturePK, expectedStatus); err != nil {
		return fmt.Errorf("defer calibration reconciliation after %v: %w", cause, err)
	}
	return cause
}

func (m *Manager) failCapture(ctx context.Context, capturePK int64, expectedStatus, message string) error {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, calibration_error = ?, processing_finished_at = ?,
		    reconcile_after = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, StatusFailed, message, now, now, capturePK, expectedStatus); err != nil {
		return fmt.Errorf("fail calibration capture: %w", err)
	}
	return fmt.Errorf("%s", message)
}

func validateOrbitJob(job orbitapi.Job, request orbitapi.SubmitRequest) error {
	if strings.TrimSpace(job.JobID) == "" || job.SubmissionID != request.SubmissionID {
		return fmt.Errorf("Orbit Job identity does not match frozen request")
	}
	if job.Image != request.Image || !equalBindings(job.DataBindings, request.DataBindings) {
		return fmt.Errorf("Orbit Job specification does not match frozen request")
	}
	return nil
}

func equalBindings(left, right []orbitapi.DataBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (m *Manager) orbitLogTail(ctx context.Context, jobID string) string {
	if m.orbit == nil || strings.TrimSpace(jobID) == "" {
		return ""
	}
	logs, err := m.orbit.Logs(ctx, jobID)
	if err != nil {
		return ""
	}
	if m.cfg.LogTailBytes > 0 && len(logs) > m.cfg.LogTailBytes {
		return logs[len(logs)-m.cfg.LogTailBytes:]
	}
	return logs
}

func (m *Manager) pollInterval() time.Duration {
	if m.cfg.PollInterval <= 0 {
		return 5 * time.Second
	}
	return m.cfg.PollInterval
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
