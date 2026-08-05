// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/services/stereosplit"
	"github.com/jmoiron/sqlx"
)

// Manager exposes the calibration lifecycle through a small durable interface.
type Manager struct {
	db      *sqlx.DB
	orbit   Orbit
	objects ObjectStore
	stereo  StereoPreprocessor
	cfg     Config
	now     func() time.Time

	runnerMu     sync.Mutex
	configMu     sync.RWMutex
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}
	wake         chan struct{}
}

// SetStereoPreprocessor installs the shared stereo-split pipeline dependency.
func (m *Manager) SetStereoPreprocessor(preprocessor StereoPreprocessor) {
	if m != nil {
		m.stereo = preprocessor
	}
}

// NewManager constructs the calibration module.
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

// Start queues one uploaded Capture or returns its existing active execution.
func (m *Manager) Start(ctx context.Context, captureID, actor string) (Capture, bool, error) {
	if m == nil || m.db == nil {
		return Capture{}, false, fmt.Errorf("start calibration: database is not configured")
	}
	captureID = strings.TrimSpace(captureID)
	if captureID == "" {
		return Capture{}, false, ErrCaptureNotFound
	}
	currentConfig, err := m.CurrentProcessingConfig(ctx)
	if err != nil {
		return Capture{}, false, err
	}
	if currentConfig.ImageRef == "" {
		return Capture{}, false, ErrImageNotConfigured
	}
	if m.orbit == nil || m.objects == nil {
		return Capture{}, false, ErrProcessingUnavailable
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Capture{}, false, fmt.Errorf("begin calibration admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var row struct {
		Status        string `db:"status"`
		SessionStatus string `db:"session_status"`
	}
	query := `
		SELECT c.status, s.status AS session_status
		FROM calibration_captures c
		INNER JOIN calibration_sessions s ON s.session_id = c.calibration_session_id
		WHERE c.capture_id = ?` + forUpdateClause(m.db)
	if err := tx.GetContext(ctx, &row, query, captureID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Capture{}, false, ErrCaptureNotFound
		}
		return Capture{}, false, fmt.Errorf("lock calibration capture: %w", err)
	}
	if row.SessionStatus == SessionSucceeded {
		return Capture{}, false, ErrSessionSucceeded
	}
	switch row.Status {
	case StatusUploaded:
		now := m.now().UTC()
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_captures
			SET status = ?, created_by = ?, reconcile_after = NULL,
			    calibration_error = NULL, updated_at = ?
			WHERE capture_id = ? AND status = ?
		`, StatusQueued, strings.TrimSpace(actor), now, captureID, StatusUploaded); err != nil {
			return Capture{}, false, fmt.Errorf("queue calibration capture: %w", err)
		}
	case StatusQueued, StatusSubmitting, StatusPending, StatusRunning, StatusVerifying:
		if err := tx.Commit(); err != nil {
			return Capture{}, false, fmt.Errorf("commit idempotent calibration admission: %w", err)
		}
		capture, err := m.Get(ctx, captureID)
		m.wakeReconciler()
		return capture, false, err
	case StatusUploading:
		return Capture{}, false, ErrCaptureUploading
	default:
		return Capture{}, false, ErrCaptureProcessed
	}
	if err := tx.Commit(); err != nil {
		return Capture{}, false, fmt.Errorf("commit calibration admission: %w", err)
	}
	capture, err := m.Get(ctx, captureID)
	if err != nil {
		return Capture{}, false, err
	}
	m.wakeReconciler()
	return capture, true, nil
}

// Get returns one admin-visible Capture.
func (m *Manager) Get(ctx context.Context, captureID string) (Capture, error) {
	if m == nil || m.db == nil {
		return Capture{}, fmt.Errorf("get calibration capture: database is not configured")
	}
	var capture Capture
	if err := m.db.GetContext(ctx, &capture, captureSelect+` WHERE c.capture_id = ?`, strings.TrimSpace(captureID)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Capture{}, ErrCaptureNotFound
		}
		return Capture{}, fmt.Errorf("get calibration capture: %w", err)
	}
	if err := decodeCaptureResult(&capture); err != nil {
		return Capture{}, err
	}
	return capture, nil
}

// List returns a bounded page and total count of admin-visible Captures.
func (m *Manager) List(ctx context.Context, filter ListFilter) ([]CaptureSummary, int64, error) {
	if m == nil || m.db == nil {
		return nil, 0, fmt.Errorf("list calibration captures: database is not configured")
	}
	conditions := []string{"1 = 1"}
	args := make([]any, 0, 5)
	if value := strings.TrimSpace(filter.Status); value != "" {
		conditions = append(conditions, "c.status = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.SessionID); value != "" {
		conditions = append(conditions, "c.calibration_session_id = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.DeviceID); value != "" {
		conditions = append(conditions, "s.device_id = ?")
		args = append(args, value)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")
	var total int64
	if err := m.db.GetContext(ctx, &total, `
		SELECT COUNT(*)
		FROM calibration_captures c
		INNER JOIN calibration_sessions s ON s.session_id = c.calibration_session_id
	`+where, args...); err != nil {
		return nil, 0, fmt.Errorf("count calibration captures: %w", err)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	var captures []CaptureSummary
	if err := m.db.SelectContext(ctx, &captures,
		captureSummarySelect+where+" ORDER BY c.created_at DESC, c.id DESC LIMIT ? OFFSET ?",
		queryArgs...); err != nil {
		return nil, 0, fmt.Errorf("list calibration captures: %w", err)
	}
	return captures, total, nil
}

func decodeCaptureResult(capture *Capture) error {
	if capture == nil {
		return nil
	}
	if value := strings.TrimSpace(capture.StereoSplitExecutionJSON); value != "" {
		var execution stereosplit.ExecutionSnapshot
		if err := json.Unmarshal([]byte(value), &execution); err != nil {
			return fmt.Errorf("decode stored stereo split execution: %w", err)
		}
		capture.StereoSplitExecution = &execution
	}
	if value := strings.TrimSpace(capture.StereoSplitResultJSON); value != "" {
		var result stereosplit.VerifiedOutput
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return fmt.Errorf("decode stored stereo split result: %w", err)
		}
		capture.StereoSplitResult = &result
	}
	if value := strings.TrimSpace(capture.ResultJSON); value != "" {
		if err := json.Unmarshal([]byte(value), &capture.Result); err != nil {
			return fmt.Errorf("decode stored calibration result: %w", err)
		}
	}
	return nil
}

// GetSessionStatus returns one Session status only when it belongs to the
// authenticated device identified by robotID.
func (m *Manager) GetSessionStatus(ctx context.Context, sessionID string, robotID int64) (SessionStatus, error) {
	return m.getSessionStatus(ctx, sessionID, &robotID)
}

// GetAdminSessionStatus returns one Session status without device ownership
// filtering. Callers must enforce administrator authentication.
func (m *Manager) GetAdminSessionStatus(ctx context.Context, sessionID string) (SessionStatus, error) {
	return m.getSessionStatus(ctx, sessionID, nil)
}

func (m *Manager) getSessionStatus(ctx context.Context, sessionID string, robotID *int64) (SessionStatus, error) {
	if m == nil || m.db == nil {
		return SessionStatus{}, fmt.Errorf("get calibration session: database is not configured")
	}
	query := `
		SELECT s.session_id, COALESCE(s.camera_serial, '') AS camera_serial, s.status,
		       COALESCE(s.successful_capture_id, '') AS successful_capture_id,
		       COUNT(c.id) AS capture_count,
		       COALESCE(SUM(CASE WHEN c.status <> 'uploading' THEN 1 ELSE 0 END), 0) AS uploaded_count,
		       COALESCE(SUM(CASE WHEN c.result_json IS NOT NULL THEN 1 ELSE 0 END), 0) AS processed_count,
		       s.updated_at
		FROM calibration_sessions s
		LEFT JOIN calibration_captures c ON c.calibration_session_id = s.session_id
		WHERE s.session_id = ?`
	args := []any{strings.TrimSpace(sessionID)}
	if robotID != nil {
		query += " AND s.robot_id = ?"
		args = append(args, *robotID)
	}
	query += `
		GROUP BY s.id, s.session_id, s.camera_serial, s.status, s.successful_capture_id, s.updated_at
	`
	var result SessionStatus
	err := m.db.GetContext(ctx, &result, query, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionStatus{}, ErrSessionNotFound
		}
		return SessionStatus{}, fmt.Errorf("get calibration session: %w", err)
	}
	return result, nil
}

const captureSummaryColumns = `c.id, c.capture_id, c.calibration_session_id, c.attempt_no, c.status,
	       s.robot_id, s.device_id, s.workspace_id, COALESCE(s.camera_serial, '') AS camera_serial,
	       c.bucket, c.object_key, COALESCE(c.file_size_bytes, 0) AS file_size_bytes,
	       COALESCE(c.duration_sec, 0) AS duration_sec, c.checksum_sha256,
	       COALESCE(c.object_etag, '') AS object_etag,
	       COALESCE(c.source, '') AS source,
	       COALESCE(c.local_operator, '') AS local_operator,
	       c.processing_stage,
	       COALESCE(c.processor_config_revision_id, 0) AS processor_config_revision_id,
	       COALESCE(c.processor_image, '') AS processor_image,
	       COALESCE(c.source_etag, '') AS source_etag,
	       COALESCE(c.orbit_submission_id, '') AS orbit_submission_id,
	       COALESCE(c.orbit_job_id, '') AS orbit_job_id,
	       c.verification_attempt_count,
	       COALESCE(c.result_object_key, '') AS result_object_key,
	       COALESCE(c.result_size_bytes, 0) AS result_size_bytes,
	       COALESCE(c.result_checksum_sha256, '') AS result_checksum_sha256,
	       COALESCE(c.algorithm_version, '') AS algorithm_version,
	       COALESCE(c.calibration_error, '') AS calibration_error,
	       c.created_at, c.updated_at`

const captureSummarySelect = `
	SELECT ` + captureSummaryColumns + `
	FROM calibration_captures c
	INNER JOIN calibration_sessions s ON s.session_id = c.calibration_session_id`

const captureSelect = `
	SELECT ` + captureSummaryColumns + `,
	       COALESCE(c.stereo_split_execution, '') AS stereo_split_execution,
	       COALESCE(c.stereo_split_orbit_job_id, '') AS stereo_split_orbit_job_id,
	       COALESCE(c.stereo_split_orbit_log_tail, '') AS stereo_split_orbit_log_tail,
	       COALESCE(c.stereo_split_result, '') AS stereo_split_result,
	       COALESCE(c.result_json, '') AS result_json
	FROM calibration_captures c
	INNER JOIN calibration_sessions s ON s.session_id = c.calibration_session_id`

func forUpdateClause(db *sqlx.DB) string {
	if db != nil && db.DriverName() != "sqlite" {
		return " FOR UPDATE"
	}
	return ""
}

func (m *Manager) wakeReconciler() {
	if m == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
