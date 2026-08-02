// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type calibrationSessionUploadRow struct {
	RobotID             int64          `db:"robot_id"`
	DeviceID            string         `db:"device_id"`
	WorkspaceID         int64          `db:"workspace_id"`
	Status              string         `db:"status"`
	SuccessfulCaptureID sql.NullString `db:"successful_capture_id"`
}

type calibrationCaptureUploadRow struct {
	CalibrationSessionID string          `db:"calibration_session_id"`
	AttemptNo            int64           `db:"attempt_no"`
	Status               string          `db:"status"`
	Bucket               string          `db:"bucket"`
	ObjectKey            string          `db:"object_key"`
	FileSize             sql.NullInt64   `db:"file_size_bytes"`
	Duration             sql.NullFloat64 `db:"duration_sec"`
	ChecksumSHA256       string          `db:"checksum_sha256"`
}

func (s *gatewayService) persistCalibrationUploadStart(
	ctx context.Context,
	principal devicePrincipal,
	intent uploadIntent,
	session *uploadSession,
) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return calibrationUploadDatabaseError("begin calibration upload transaction failed", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSession calibrationSessionUploadRow
	query := `
		SELECT robot_id, device_id, workspace_id, status, successful_capture_id
		FROM calibration_sessions
		WHERE session_id = ?`
	if tx.DriverName() != "sqlite" {
		query += " FOR UPDATE"
	}
	err = tx.GetContext(ctx, &existingSession, query, intent.CalibrationSessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := s.now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calibration_sessions (
				session_id, robot_id, device_id, workspace_id, status, created_at, updated_at
			) VALUES (?, ?, ?, ?, 'running', ?, ?)
		`, intent.CalibrationSessionID, principal.RobotID, principal.DeviceID, principal.WorkspaceID, now, now); err != nil {
			return calibrationUploadDatabaseError("create calibration session failed", err)
		}
	case err != nil:
		return calibrationUploadDatabaseError("calibration session lookup unavailable", err)
	case existingSession.RobotID != principal.RobotID ||
		existingSession.DeviceID != principal.DeviceID ||
		existingSession.WorkspaceID != principal.WorkspaceID:
		return status.Error(codes.PermissionDenied, "calibration session belongs to another device")
	case existingSession.Status == "succeeded":
		return status.Error(codes.FailedPrecondition, "calibration session already succeeded")
	}

	var attemptCaptureID string
	attemptQuery := `
		SELECT capture_id
		FROM calibration_captures
		WHERE calibration_session_id = ? AND attempt_no = ?`
	if tx.DriverName() != "sqlite" {
		attemptQuery += " FOR UPDATE"
	}
	err = tx.GetContext(ctx, &attemptCaptureID, attemptQuery, intent.CalibrationSessionID, intent.AttemptNo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return calibrationUploadDatabaseError("calibration attempt lookup unavailable", err)
	}
	if err == nil && attemptCaptureID != intent.CaptureID {
		return status.Error(codes.FailedPrecondition, "attempt_no is already used by another calibration capture")
	}

	var existingCapture calibrationCaptureUploadRow
	err = tx.GetContext(ctx, &existingCapture, `
		SELECT calibration_session_id, attempt_no, status, bucket, object_key,
		       file_size_bytes, duration_sec, checksum_sha256
		FROM calibration_captures
		WHERE capture_id = ?
	`, intent.CaptureID)
	now := s.now()
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calibration_captures (
				capture_id, calibration_session_id, attempt_no, status,
				bucket, object_key, checksum_sha256, logical_upload_id, upload_id,
				source, local_operator, created_at, updated_at
			) VALUES (?, ?, ?, 'uploading', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, intent.CaptureID, intent.CalibrationSessionID, intent.AttemptNo,
			session.Bucket, session.ObjectKey, intent.ChecksumSHA256,
			session.LogicalUploadID, session.UploadID,
			strings.TrimSpace(session.ClientHints["source"]), strings.TrimSpace(session.ClientHints["local_operator"]),
			now, now); err != nil {
			return calibrationUploadDatabaseError("create calibration capture failed", err)
		}
	case err != nil:
		return calibrationUploadDatabaseError("calibration capture lookup unavailable", err)
	case existingCapture.CalibrationSessionID != intent.CalibrationSessionID ||
		existingCapture.AttemptNo != intent.AttemptNo ||
		existingCapture.ChecksumSHA256 != intent.ChecksumSHA256 ||
		existingCapture.Bucket != session.Bucket || existingCapture.ObjectKey != session.ObjectKey:
		return status.Error(codes.FailedPrecondition, "capture_id conflicts with an existing calibration capture")
	case existingCapture.Status != "uploading":
		return status.Error(codes.FailedPrecondition, "calibration capture is no longer uploadable")
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE calibration_captures
			SET logical_upload_id = ?, upload_id = ?, updated_at = ?
			WHERE capture_id = ? AND status = 'uploading'
		`, session.LogicalUploadID, session.UploadID, now, intent.CaptureID); err != nil {
			return calibrationUploadDatabaseError("resume calibration capture failed", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return calibrationUploadDatabaseError("commit calibration upload transaction failed", err)
	}
	return nil
}

func (s *gatewayService) completeCalibrationUpload(
	ctx context.Context,
	session *uploadSession,
	req *cloudpb.CompleteUploadRequest,
) error {
	if s.db == nil {
		return nil
	}
	rawTags := req.GetRawTags()
	for _, key := range []string{"upload_kind", "attempt_no"} {
		if err := requireMatchingRawTag(rawTags, key, session.ClientHints[key]); err != nil {
			return err
		}
	}
	for _, key := range []string{"calibration_session_id", "capture_id"} {
		if err := requireMatchingUUIDRawTag(rawTags, key, session.ClientHints[key]); err != nil {
			return err
		}
	}
	if err := requireMatchingDigestRawTag(rawTags, "checksum_sha256", session.ClientHints["checksum_sha256"]); err != nil {
		return err
	}
	for _, key := range []string{"source", "local_operator"} {
		if strings.TrimSpace(session.ClientHints[key]) != "" {
			if err := requireMatchingRawTag(rawTags, key, session.ClientHints[key]); err != nil {
				return err
			}
		}
	}
	if req.GetFileSize() <= 0 {
		return status.Error(codes.InvalidArgument, "file_size must be positive")
	}
	duration, err := uploadDurationSec(rawTags)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return calibrationUploadDatabaseError("begin calibration completion transaction failed", err)
	}
	defer func() { _ = tx.Rollback() }()

	var calibrationSession calibrationSessionUploadRow
	query := `
		SELECT robot_id, device_id, workspace_id, status, successful_capture_id
		FROM calibration_sessions
		WHERE session_id = ?`
	if tx.DriverName() != "sqlite" {
		query += " FOR UPDATE"
	}
	if err := tx.GetContext(ctx, &calibrationSession, query, session.ClientHints["calibration_session_id"]); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.NotFound, "calibration session not found")
		}
		return calibrationUploadDatabaseError("calibration session lookup unavailable", err)
	}
	principal, err := principalFromContext(ctx)
	if err != nil {
		return err
	}
	if calibrationSession.RobotID != principal.RobotID || calibrationSession.DeviceID != principal.DeviceID || calibrationSession.WorkspaceID != principal.WorkspaceID {
		return status.Error(codes.PermissionDenied, "calibration session belongs to another device")
	}

	var capture calibrationCaptureUploadRow
	if err := tx.GetContext(ctx, &capture, `
		SELECT calibration_session_id, attempt_no, status, bucket, object_key,
		       file_size_bytes, duration_sec, checksum_sha256
		FROM calibration_captures
		WHERE capture_id = ?
	`, session.ClientHints["capture_id"]); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status.Error(codes.NotFound, "calibration capture not found")
		}
		return calibrationUploadDatabaseError("calibration capture lookup unavailable", err)
	}
	attemptNo, err := parseInt64(session.ClientHints["attempt_no"])
	if err != nil || attemptNo <= 0 {
		return status.Error(codes.Internal, "calibration upload session has invalid attempt_no")
	}
	if capture.CalibrationSessionID != session.ClientHints["calibration_session_id"] ||
		capture.AttemptNo != attemptNo ||
		capture.Bucket != session.Bucket || capture.ObjectKey != session.ObjectKey ||
		capture.ChecksumSHA256 != strings.ToLower(strings.TrimSpace(rawTags["checksum_sha256"])) {
		return status.Error(codes.FailedPrecondition, "calibration capture identity changed")
	}
	if capture.Status != "uploading" {
		if capture.FileSize.Valid && capture.FileSize.Int64 == req.GetFileSize() &&
			capture.ChecksumSHA256 == strings.ToLower(strings.TrimSpace(rawTags["checksum_sha256"])) {
			if err := tx.Commit(); err != nil {
				return calibrationUploadDatabaseError("commit idempotent calibration completion failed", err)
			}
			return nil
		}
		return status.Error(codes.FailedPrecondition, "calibration completion differs from stored capture")
	}

	nextStatus := "uploaded"
	if calibrationSession.Status == "succeeded" {
		nextStatus = "superseded"
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE calibration_captures
		SET status = ?, file_size_bytes = ?, duration_sec = ?, object_etag = ?,
		    uploaded_at = ?, updated_at = ?
		WHERE capture_id = ? AND status = 'uploading'
	`, nextStatus, req.GetFileSize(), duration, strings.TrimSpace(req.GetObjectEtag()), now, now,
		session.ClientHints["capture_id"]); err != nil {
		return calibrationUploadDatabaseError("complete calibration capture failed", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE calibration_sessions SET updated_at = ? WHERE session_id = ?
	`, now, session.ClientHints["calibration_session_id"]); err != nil {
		return calibrationUploadDatabaseError("update calibration session failed", err)
	}
	if err := tx.Commit(); err != nil {
		return calibrationUploadDatabaseError("commit calibration completion transaction failed", err)
	}
	return nil
}

func calibrationUploadDatabaseError(message string, err error) error {
	logger.Printf("[DGW_COMPAT] %s: %v", message, err)
	return status.Error(codes.Unavailable, message)
}

func requireMatchingUUIDRawTag(tags map[string]string, key, expected string) error {
	actualUUID, actualErr := parseCanonicalV4UUID(tags[key])
	expectedUUID, expectedErr := parseCanonicalV4UUID(expected)
	if actualErr != nil || expectedErr != nil || actualUUID != expectedUUID {
		return status.Errorf(codes.FailedPrecondition, "%s does not match upload session", key)
	}
	return nil
}
