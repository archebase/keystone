// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

const testProcessorImage = "registry.example/archebase/calibration@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestManagerProcessesOneCaptureThroughOrbitAndSucceedsSession(t *testing.T) {
	db := newCalibrationTestDB(t)
	orbit := &fakeOrbit{}
	objects := &fakeObjectStore{objects: map[string]fakeObject{
		"calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap": {
			size: 1024,
			etag: "source-etag",
		},
	}}
	manager := NewManager(db, orbit, objects, testCalibrationConfig())

	capture, created, err := manager.Start(
		context.Background(),
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"admin-user",
	)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !created || capture.Status != StatusQueued {
		t.Fatalf("Start() = %+v created=%t", capture, created)
	}

	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("freeze ReconcileOnce() worked=%t error=%v", worked, err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("submit ReconcileOnce() worked=%t error=%v", worked, err)
	}
	if orbit.submitCalls != 1 {
		t.Fatalf("Orbit submit calls = %d, want 1", orbit.submitCalls)
	}
	if orbit.request.Image != testProcessorImage || len(orbit.request.DataBindings) != 2 {
		t.Fatalf("Orbit request = %+v", orbit.request)
	}
	frozen, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() frozen capture error = %v", err)
	}
	if frozen.ProcessorConfigRevisionID != 1 || frozen.ProcessorImage != testProcessorImage {
		t.Fatalf("frozen processing config = %+v", frozen)
	}
	if got := orbit.request.DataBindings[0].URI; got != "tos://bucket-1/calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap" {
		t.Fatalf("input binding URI = %q", got)
	}
	if got := orbit.request.DataBindings[1].URI; got != "tos://bucket-1/calibration-results/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/" {
		t.Fatalf("output binding URI = %q", got)
	}

	orbit.job = orbitapi.Job{
		JobID:        "abs-job-calibration-1",
		SubmissionID: orbit.request.SubmissionID,
		Status:       "SUCCEEDED",
		Image:        orbit.request.Image,
		DataBindings: orbit.request.DataBindings,
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("terminal ReconcileOnce() worked=%t error=%v", worked, err)
	}
	queuedCaptureID := "d4ad1825-35b4-4572-83aa-70cf3d8dd083"
	if _, err := db.Exec(`
		INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			file_size_bytes, checksum_sha256, logical_upload_id, upload_id,
			reconcile_after, created_at, updated_at, uploaded_at
		) VALUES (?, '7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 2, 'queued', 'bucket-1',
			'capture-2.mcap', 1024, ?, 'logical-2', 'upload-2',
			'2099-01-01 00:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, queuedCaptureID, "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"); err != nil {
		t.Fatalf("insert queued capture: %v", err)
	}

	resultBody := `{
		"schema_version": 1,
		"status": "succeeded",
		"algorithm_version": "placeholder-v1",
		"placeholder": true,
		"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		"capture_id": "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"processor_image": "` + testProcessorImage + `",
		"source": {
			"uri": "tos://bucket-1/calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap",
			"size_bytes": 1024,
			"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		},
		"result": {"camera_matrix": [[1,0,0],[0,1,0],[0,0,1]]},
		"started_at": "2026-08-02T10:00:00Z",
		"finished_at": "2026-08-02T10:00:01Z"
	}`
	resultDigest := sha256.Sum256([]byte(resultBody))
	objects.objects["calibration-results/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/result.json"] = fakeObject{
		size: int64(len(resultBody)),
		etag: "result-etag",
		body: resultBody,
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("verify ReconcileOnce() worked=%t error=%v", worked, err)
	}

	capture, err = manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if capture.Status != StatusSucceeded || capture.ResultObjectKey == "" ||
		capture.ResultChecksumSHA256 != hex.EncodeToString(resultDigest[:]) {
		t.Fatalf("completed capture = %+v", capture)
	}
	session, err := manager.GetSessionStatus(context.Background(), "7f9af590-75c2-47ad-b6e0-76ebf05c44f7", 1)
	if err != nil {
		t.Fatalf("GetSessionStatus() error = %v", err)
	}
	if session.Status != SessionSucceeded || session.SuccessfulCaptureID != capture.CaptureID || session.ProcessedCount != 1 {
		t.Fatalf("completed session = %+v", session)
	}
	superseded, err := manager.Get(context.Background(), queuedCaptureID)
	if err != nil {
		t.Fatalf("Get() queued capture error = %v", err)
	}
	if superseded.Status != StatusSuperseded {
		t.Fatalf("queued capture status = %q, want %q", superseded.Status, StatusSuperseded)
	}
}

func TestManagerFailedCaptureLeavesSessionRunning(t *testing.T) {
	db := newCalibrationTestDB(t)
	if _, err := db.Exec(`
		UPDATE calibration_captures
		SET status = 'verifying', processor_image = ?, source_etag = 'source-etag'
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`, testProcessorImage); err != nil {
		t.Fatalf("prepare verifying capture: %v", err)
	}
	resultBody := `{
		"schema_version": 1,
		"status": "failed",
		"algorithm_version": "placeholder-v1",
		"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		"capture_id": "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"processor_image": "` + testProcessorImage + `",
		"error_message": "target was not visible",
		"source": {
			"uri": "tos://bucket-1/calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap",
			"size_bytes": 1024,
			"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		}
	}`
	objects := &fakeObjectStore{objects: map[string]fakeObject{
		"calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap": {
			size: 1024,
			etag: "source-etag",
		},
		"calibration-results/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/result.json": {
			size: int64(len(resultBody)),
			etag: "result-etag",
			body: resultBody,
		},
	}}
	manager := NewManager(db, nil, objects, testCalibrationConfig())

	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("ReconcileOnce() worked=%t error=%v", worked, err)
	}
	capture, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	session, err := manager.GetSessionStatus(context.Background(), "7f9af590-75c2-47ad-b6e0-76ebf05c44f7", 1)
	if err != nil {
		t.Fatalf("GetSessionStatus() error = %v", err)
	}
	if capture.Status != StatusFailed || capture.CalibrationError != "target was not visible" {
		t.Fatalf("failed capture = %+v", capture)
	}
	if _, err := db.Exec(`
		INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			file_size_bytes, checksum_sha256, logical_upload_id, upload_id,
			created_at, updated_at, uploaded_at
		) VALUES (?, ?, 2, 'superseded', 'bucket-1', 'capture-2.mcap', 1024, ?, 'logical-2', 'upload-2',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, "d4ad1825-35b4-4572-83aa-70cf3d8dd083", "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"); err != nil {
		t.Fatalf("insert upload-only superseded capture: %v", err)
	}
	session, err = manager.GetSessionStatus(context.Background(), "7f9af590-75c2-47ad-b6e0-76ebf05c44f7", 1)
	if err != nil {
		t.Fatalf("GetSessionStatus() after superseded upload error = %v", err)
	}
	if session.Status != SessionRunning || session.SuccessfulCaptureID != "" || session.ProcessedCount != 1 {
		t.Fatalf("session after failed capture = %+v", session)
	}
}

func TestManagerSessionStatusIsHiddenFromAnotherDevice(t *testing.T) {
	manager := NewManager(newCalibrationTestDB(t), nil, nil, testCalibrationConfig())

	_, err := manager.GetSessionStatus(
		context.Background(),
		"7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		999,
	)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetSessionStatus() error = %v, want %v", err, ErrSessionNotFound)
	}
}

func TestManagerPollsActiveCaptureBeforeQueuedCaptureAtCapacity(t *testing.T) {
	db := newCalibrationTestDB(t)
	orbit := &fakeOrbit{}
	objects := &fakeObjectStore{objects: map[string]fakeObject{
		"calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap": {
			size: 1024,
			etag: "source-etag",
		},
	}}
	manager := NewManager(db, orbit, objects, testCalibrationConfig())
	if _, err := db.Exec(`UPDATE calibration_processing_configs SET max_concurrent = 1 WHERE id = 1`); err != nil {
		t.Fatalf("set max concurrency: %v", err)
	}

	if _, _, err := manager.Start(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011", "admin-user"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
			t.Fatalf("admission ReconcileOnce() %d worked=%t error=%v", i, worked, err)
		}
	}
	orbit.job = orbitapi.Job{
		JobID:        "abs-job-calibration-1",
		SubmissionID: orbit.request.SubmissionID,
		Status:       "RUNNING",
		Image:        orbit.request.Image,
		DataBindings: orbit.request.DataBindings,
	}
	if _, err := db.Exec(`
		INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			file_size_bytes, checksum_sha256, logical_upload_id, upload_id,
			created_at, updated_at, uploaded_at
		) VALUES (?, '7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 2, 'queued', 'bucket-1',
			'capture-2.mcap', 1024, ?, 'logical-2', 'upload-2',
			'2000-01-01 00:00:00', '2000-01-01 00:00:00', CURRENT_TIMESTAMP)
	`, "d4ad1825-35b4-4572-83aa-70cf3d8dd083",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"); err != nil {
		t.Fatalf("insert older queued capture: %v", err)
	}

	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("active poll ReconcileOnce() worked=%t error=%v", worked, err)
	}
	active, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() active capture error = %v", err)
	}
	if active.Status != StatusRunning {
		t.Fatalf("active capture status = %q, want %q", active.Status, StatusRunning)
	}
}

func TestManagerSuccessfulCaptureStopsOtherActiveJobs(t *testing.T) {
	db := newCalibrationTestDB(t)
	if _, err := db.Exec(`
		UPDATE calibration_captures
		SET status = 'verifying', processor_image = ?, source_etag = 'source-etag'
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`, testProcessorImage); err != nil {
		t.Fatalf("prepare winning capture: %v", err)
	}
	activeCaptureID := "d4ad1825-35b4-4572-83aa-70cf3d8dd083"
	if _, err := db.Exec(`
		INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			file_size_bytes, checksum_sha256, logical_upload_id, upload_id,
			processor_image, orbit_submission_id, orbit_job_id, reconcile_after,
			created_at, updated_at, uploaded_at, processing_started_at
		) VALUES (?, '7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 2, 'running', 'bucket-1',
			'capture-2.mcap', 1024, ?, 'logical-2', 'upload-2', ?,
			'calibration-d4ad1825-35b4-4572-83aa-70cf3d8dd083', 'abs-job-calibration-2',
			'2099-01-01 00:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, activeCaptureID, "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		testProcessorImage); err != nil {
		t.Fatalf("insert active capture: %v", err)
	}

	resultBody := `{
		"schema_version": 1,
		"status": "succeeded",
		"algorithm_version": "placeholder-v1",
		"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		"capture_id": "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"processor_image": "` + testProcessorImage + `",
		"source": {
			"uri": "tos://bucket-1/calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap",
			"size_bytes": 1024,
			"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		},
		"result": {"camera_matrix": [[1,0,0],[0,1,0],[0,0,1]]}
	}`
	objects := &fakeObjectStore{objects: map[string]fakeObject{
		"calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap": {
			size: 1024,
			etag: "source-etag",
		},
		"calibration-results/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/result.json": {
			size: int64(len(resultBody)),
			etag: "result-etag",
			body: resultBody,
		},
	}}
	orbit := &fakeOrbit{job: orbitapi.Job{JobID: "abs-job-calibration-2", Status: "RUNNING"}}
	manager := NewManager(db, orbit, objects, testCalibrationConfig())

	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("verify winning capture worked=%t error=%v", worked, err)
	}
	active, err := manager.Get(context.Background(), activeCaptureID)
	if err != nil {
		t.Fatalf("Get() active capture error = %v", err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("cancel active capture worked=%t error=%v", worked, err)
	}
	if len(orbit.stopCalls) != 1 || orbit.stopCalls[0] != "abs-job-calibration-2" {
		t.Fatalf("Orbit stop calls = %v", orbit.stopCalls)
	}
	active, err = manager.Get(context.Background(), activeCaptureID)
	if err != nil {
		t.Fatalf("Get() cancelled capture error = %v", err)
	}
	if active.Status != StatusSuperseded {
		t.Fatalf("cancelled capture status = %q, want %q", active.Status, StatusSuperseded)
	}
}

func TestManagerRetriesTransientOrbitStopFailure(t *testing.T) {
	db := newCalibrationTestDB(t)
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		UPDATE calibration_captures
		SET status = 'running', processor_image = ?,
		    orbit_submission_id = 'calibration-92cd6f2f-d131-4bf0-9b4a-d96258d09011',
		    orbit_job_id = 'abs-job-calibration-1', cancel_requested_at = ?,
		    reconcile_after = NULL
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`, testProcessorImage, now); err != nil {
		t.Fatalf("prepare cancellation retry: %v", err)
	}
	orbit := &fakeOrbit{
		job:     orbitapi.Job{JobID: "abs-job-calibration-1", Status: "RUNNING"},
		stopErr: errors.New("temporary Orbit failure"),
	}
	manager := NewManager(db, orbit, &fakeObjectStore{}, testCalibrationConfig())
	manager.now = func() time.Time { return now }

	if worked, err := manager.ReconcileOnce(context.Background()); err == nil || !worked {
		t.Fatalf("first cancellation worked=%t error=%v", worked, err)
	}
	if len(orbit.stopCalls) != 1 {
		t.Fatalf("Orbit stop calls after failure = %v", orbit.stopCalls)
	}
	capture, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() deferred capture error = %v", err)
	}
	if capture.Status != StatusRunning {
		t.Fatalf("deferred capture status = %q, want %q", capture.Status, StatusRunning)
	}

	now = now.Add(time.Second)
	orbit.stopErr = nil
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("retried cancellation worked=%t error=%v", worked, err)
	}
	if len(orbit.stopCalls) != 2 {
		t.Fatalf("Orbit stop calls after retry = %v", orbit.stopCalls)
	}
	capture, err = manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() cancelled capture error = %v", err)
	}
	if capture.Status != StatusSuperseded {
		t.Fatalf("retried capture status = %q, want %q", capture.Status, StatusSuperseded)
	}
}

func TestManagerConfirmsCanceledSubmissionIsAbsentAcrossTwoPolls(t *testing.T) {
	db := newCalibrationTestDB(t)
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		UPDATE calibration_captures
		SET status = 'submitting',
		    orbit_submission_id = 'calibration-92cd6f2f-d131-4bf0-9b4a-d96258d09011',
		    orbit_request = '{}', submit_attempt_count = 1,
		    cancel_requested_at = ?, reconcile_after = NULL
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("prepare missing submission cancellation: %v", err)
	}
	manager := NewManager(db, &fakeOrbit{}, &fakeObjectStore{}, testCalibrationConfig())
	manager.now = func() time.Time { return now }

	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("first absence poll worked=%t error=%v", worked, err)
	}
	capture, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() after first absence error = %v", err)
	}
	if capture.Status != StatusSubmitting {
		t.Fatalf("status after first absence = %q, want %q", capture.Status, StatusSubmitting)
	}
	var absentAt sql.NullTime
	if err := db.Get(&absentAt, `
		SELECT orbit_submit_absent_at FROM calibration_captures
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`); err != nil {
		t.Fatalf("load first absence observation: %v", err)
	}
	if !absentAt.Valid || !absentAt.Time.Equal(now) {
		t.Fatalf("orbit_submit_absent_at = %v, want %v", absentAt, now)
	}

	now = now.Add(manager.cancellationAbsenceGrace())
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("confirmed absence poll worked=%t error=%v", worked, err)
	}
	capture, err = manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() after confirmed absence error = %v", err)
	}
	if capture.Status != StatusSuperseded {
		t.Fatalf("status after confirmed absence = %q, want %q", capture.Status, StatusSuperseded)
	}
}

func TestManagerMissingActiveJobFailsAfterGraceAndReleasesCapacity(t *testing.T) {
	db := newCalibrationTestDB(t)
	if _, err := db.Exec(`UPDATE calibration_processing_configs SET max_concurrent = 1 WHERE id = 1`); err != nil {
		t.Fatalf("set max concurrency: %v", err)
	}
	orbit := &fakeOrbit{}
	objects := &fakeObjectStore{objects: map[string]fakeObject{
		"calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap": {
			size: 1024,
			etag: "source-etag",
		},
	}}
	manager := NewManager(db, orbit, objects, testCalibrationConfig())
	now := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }

	if _, _, err := manager.Start(
		context.Background(),
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"admin-user",
	); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("freeze ReconcileOnce() worked=%t error=%v", worked, err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("submit ReconcileOnce() worked=%t error=%v", worked, err)
	}

	if worked, err := manager.ReconcileOnce(context.Background()); err == nil || !worked {
		t.Fatalf("first missing poll worked=%t error=%v", worked, err)
	}
	var first struct {
		Status   string       `db:"status"`
		AbsentAt sql.NullTime `db:"orbit_submit_absent_at"`
	}
	if err := db.Get(&first, `
		SELECT status, orbit_submit_absent_at
		FROM calibration_captures
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`); err != nil {
		t.Fatalf("load first missing observation: %v", err)
	}
	if first.Status != StatusPending || !first.AbsentAt.Valid || !first.AbsentAt.Time.Equal(now) {
		t.Fatalf("first missing observation = %+v, want pending at %v", first, now)
	}

	now = now.Add(manager.activeJobMissingGrace())
	if worked, err := manager.ReconcileOnce(context.Background()); err == nil || !worked {
		t.Fatalf("expired missing poll worked=%t error=%v", worked, err)
	}
	capture, err := manager.Get(context.Background(), "92cd6f2f-d131-4bf0-9b4a-d96258d09011")
	if err != nil {
		t.Fatalf("Get() after missing grace error = %v", err)
	}
	if capture.Status != StatusFailed {
		t.Fatalf("capture status = %q, want %q", capture.Status, StatusFailed)
	}
	atCapacity, err := manager.atOrbitCapacity(context.Background())
	if err != nil {
		t.Fatalf("atOrbitCapacity() error = %v", err)
	}
	if atCapacity {
		t.Fatal("atOrbitCapacity() = true after missing Orbit Job failure")
	}
}

func TestManagerUpdatesAuditedProcessingConfig(t *testing.T) {
	db := newCalibrationTestDB(t)
	manager := NewManager(db, nil, nil, testCalibrationConfig())
	current, err := manager.CurrentProcessingConfig(context.Background())
	if err != nil {
		t.Fatalf("CurrentProcessingConfig() error = %v", err)
	}
	if current.ID != 1 || current.ImageRef != testProcessorImage || current.MaxConcurrent != 2 {
		t.Fatalf("current config = %+v", current)
	}

	nextImage := "registry.example/archebase/calibration@sha256:" + strings.Repeat("b", 64)
	updated, err := manager.UpdateProcessingConfig(context.Background(), nextImage, 3, current.ID, "admin-user")
	if err != nil {
		t.Fatalf("UpdateProcessingConfig() error = %v", err)
	}
	if updated.ID != 2 || updated.ImageRef != nextImage || updated.MaxConcurrent != 3 ||
		updated.PreviousImageRef != testProcessorImage || updated.CreatedBy != "admin-user" {
		t.Fatalf("updated config = %+v", updated)
	}
	if _, err := manager.UpdateProcessingConfig(context.Background(), nextImage, 3, current.ID, "stale-admin"); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("stale UpdateProcessingConfig() error = %v", err)
	}
	history, err := manager.ListProcessingConfigHistory(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("ListProcessingConfigHistory() error = %v", err)
	}
	if len(history) != 2 || history[0].ID != updated.ID {
		t.Fatalf("config history = %+v", history)
	}
}

func TestManagerRejectsMutableProcessingImage(t *testing.T) {
	manager := NewManager(newCalibrationTestDB(t), nil, nil, testCalibrationConfig())
	_, err := manager.UpdateProcessingConfig(
		context.Background(),
		"registry.example/archebase/calibration:latest",
		2,
		1,
		"admin-user",
	)
	if err == nil {
		t.Fatal("UpdateProcessingConfig() error = nil, want immutable image rejection")
	}
}

func TestManagerRejectsProcessingWhenImageIsUnconfigured(t *testing.T) {
	db := newCalibrationTestDB(t)
	if _, err := db.Exec("UPDATE calibration_processing_configs SET image_ref = NULL WHERE id = 1"); err != nil {
		t.Fatalf("clear processing image: %v", err)
	}
	manager := NewManager(db, nil, nil, testCalibrationConfig())
	_, _, err := manager.Start(
		context.Background(),
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"admin-user",
	)
	if !errors.Is(err, ErrImageNotConfigured) {
		t.Fatalf("Start() error = %v, want ErrImageNotConfigured", err)
	}
}

func TestManagerDefersAutomaticallyQueuedCaptureUntilImageIsConfigured(t *testing.T) {
	db := newCalibrationTestDB(t)
	if _, err := db.Exec("UPDATE calibration_processing_configs SET image_ref = NULL WHERE id = 1"); err != nil {
		t.Fatalf("clear processing image: %v", err)
	}
	if _, err := db.Exec("UPDATE calibration_captures SET status = 'queued' WHERE capture_id = ?",
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011"); err != nil {
		t.Fatalf("queue capture: %v", err)
	}
	manager := NewManager(db, &fakeOrbit{}, &fakeObjectStore{}, testCalibrationConfig())
	worked, err := manager.ReconcileOnce(context.Background())
	if !worked || !errors.Is(err, ErrImageNotConfigured) {
		t.Fatalf("ReconcileOnce() worked=%t error=%v", worked, err)
	}
	var retryAt sql.NullTime
	if err := db.Get(&retryAt, "SELECT reconcile_after FROM calibration_captures WHERE capture_id = ?",
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011"); err != nil {
		t.Fatalf("query queued retry: %v", err)
	}
	if !retryAt.Valid {
		t.Fatal("reconcile_after is NULL, want deferred retry")
	}
}

func TestManagerRejectsProcessingWhenOrbitIsUnavailable(t *testing.T) {
	manager := NewManager(newCalibrationTestDB(t), nil, nil, testCalibrationConfig())
	_, _, err := manager.Start(
		context.Background(),
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"admin-user",
	)
	if !errors.Is(err, ErrProcessingUnavailable) {
		t.Fatalf("Start() error = %v, want ErrProcessingUnavailable", err)
	}
}

func TestManagerRefusesToStartWithoutProcessingDependencies(t *testing.T) {
	tests := []struct {
		name    string
		orbit   Orbit
		objects ObjectStore
	}{
		{name: "Orbit", objects: &fakeObjectStore{}},
		{name: "TOS", orbit: &fakeOrbit{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(newCalibrationTestDB(t), test.orbit, test.objects, testCalibrationConfig())
			if err := manager.StartReconciler(); err == nil {
				t.Fatalf("StartReconciler() error = nil without %s", test.name)
			}
		})
	}
}

type fakeOrbit struct {
	request     orbitapi.SubmitRequest
	submitCalls int
	job         orbitapi.Job
	stopCalls   []string
	stopErr     error
}

func (f *fakeOrbit) Submit(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
	f.submitCalls++
	f.request = request
	return orbitapi.SubmitResponse{JobID: "abs-job-calibration-1", SubmissionID: request.SubmissionID}, nil
}

func (f *fakeOrbit) Get(context.Context, string) (orbitapi.Job, error) {
	if f.job.JobID == "" {
		return orbitapi.Job{}, orbitapi.ErrNotFound
	}
	return f.job, nil
}

func (f *fakeOrbit) Logs(context.Context, string) (string, error) { return "placeholder logs", nil }

func (f *fakeOrbit) Stop(_ context.Context, id string) (orbitapi.Job, error) {
	f.stopCalls = append(f.stopCalls, id)
	if f.stopErr != nil {
		return orbitapi.Job{}, f.stopErr
	}
	return orbitapi.Job{JobID: id, Status: "STOPPED"}, nil
}

type fakeObject struct {
	size int64
	etag string
	body string
}

type fakeObjectStore struct {
	objects map[string]fakeObject
}

func (f *fakeObjectStore) StatObject(_ context.Context, _ string, objectName string) (int64, string, error) {
	object, ok := f.objects[objectName]
	if !ok {
		return 0, "", errors.New("object not found")
	}
	return object.size, object.etag, nil
}

func (f *fakeObjectStore) OpenObject(_ context.Context, _ string, objectName string) (io.ReadCloser, error) {
	object, ok := f.objects[objectName]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(strings.NewReader(object.body)), nil
}

func testCalibrationConfig() Config {
	return Config{
		Resources: Resources{
			Requests: map[string]string{"cpu": "1", "memory": "1Gi"},
			Limits:   map[string]string{"cpu": "2", "memory": "2Gi"},
		},
		ActiveDeadline:      600,
		TTLSecondsAfterDone: 86400,
		PollInterval:        time.Second,
		MaxResultBytes:      1024 * 1024,
		LogTailBytes:        64 * 1024,
	}
}

func newCalibrationTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE calibration_processing_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_ref TEXT,
			previous_image_ref TEXT,
			max_concurrent INTEGER NOT NULL DEFAULT 1,
			previous_max_concurrent INTEGER,
			created_by TEXT,
			created_at TIMESTAMP NOT NULL
		)`,
		`INSERT INTO calibration_processing_configs (
			image_ref, previous_image_ref, max_concurrent, created_by, created_at
		) VALUES (
			'` + testProcessorImage + `', NULL, 2, 'migration-bootstrap', CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE calibration_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL UNIQUE,
			robot_id INTEGER NOT NULL,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			successful_capture_id TEXT,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE calibration_captures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			capture_id TEXT NOT NULL UNIQUE,
			calibration_session_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			status TEXT NOT NULL,
			bucket TEXT NOT NULL,
			object_key TEXT NOT NULL,
			file_size_bytes INTEGER,
			duration_sec REAL,
			checksum_sha256 TEXT NOT NULL,
			object_etag TEXT,
			logical_upload_id TEXT NOT NULL,
			upload_id TEXT NOT NULL,
			source TEXT,
			local_operator TEXT,
			uploaded_at TIMESTAMP,
			processor_config_revision_id INTEGER,
			processor_image TEXT,
			source_etag TEXT,
			orbit_submission_id TEXT,
			orbit_request TEXT,
			orbit_job_id TEXT,
			orbit_log_tail TEXT,
			cancel_requested_at TIMESTAMP,
			submit_attempt_count INTEGER NOT NULL DEFAULT 0,
			orbit_submit_absent_at TIMESTAMP,
			reconcile_after TIMESTAMP,
			processing_started_at TIMESTAMP,
			processing_finished_at TIMESTAMP,
			result_object_key TEXT,
			result_size_bytes INTEGER,
			result_checksum_sha256 TEXT,
			result_json TEXT,
			algorithm_version TEXT,
			calibration_error TEXT,
			created_by TEXT,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`INSERT INTO calibration_sessions (
			session_id, robot_id, device_id, workspace_id, status, created_at, updated_at
		) VALUES (
			'7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 1, '101', 10, 'running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
		`INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			file_size_bytes, checksum_sha256, object_etag, logical_upload_id, upload_id,
			created_at, updated_at, uploaded_at
		) VALUES (
			'92cd6f2f-d131-4bf0-9b4a-d96258d09011',
			'7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 1, 'uploaded', 'bucket-1',
			'calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap',
			1024, '9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08',
			'etag-1', 'logical-1', 'upload-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec schema: %v\n%s", err, statement)
		}
	}
	return db
}
