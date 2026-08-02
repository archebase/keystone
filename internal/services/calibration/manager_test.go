// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"crypto/sha256"
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
	session, err := manager.GetSessionStatus(context.Background(), "7f9af590-75c2-47ad-b6e0-76ebf05c44f7")
	if err != nil {
		t.Fatalf("GetSessionStatus() error = %v", err)
	}
	if session.Status != SessionSucceeded || session.SuccessfulCaptureID != capture.CaptureID || session.ProcessedCount != 1 {
		t.Fatalf("completed session = %+v", session)
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
	session, err := manager.GetSessionStatus(context.Background(), "7f9af590-75c2-47ad-b6e0-76ebf05c44f7")
	if err != nil {
		t.Fatalf("GetSessionStatus() error = %v", err)
	}
	if capture.Status != StatusFailed || capture.CalibrationError != "target was not visible" {
		t.Fatalf("failed capture = %+v", capture)
	}
	if session.Status != SessionRunning || session.SuccessfulCaptureID != "" || session.ProcessedCount != 1 {
		t.Fatalf("session after failed capture = %+v", session)
	}
}

type fakeOrbit struct {
	request     orbitapi.SubmitRequest
	submitCalls int
	job         orbitapi.Job
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
		Enabled:             true,
		ProcessorImage:      testProcessorImage,
		AllowedRepositories: []string{"registry.example/archebase/calibration"},
		Resources: Resources{
			Requests: map[string]string{"cpu": "1", "memory": "1Gi"},
			Limits:   map[string]string{"cpu": "2", "memory": "2Gi"},
		},
		ActiveDeadline:      600,
		TTLSecondsAfterDone: 86400,
		MaxConcurrent:       2,
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
			processor_image TEXT,
			source_etag TEXT,
			orbit_submission_id TEXT,
			orbit_request TEXT,
			orbit_job_id TEXT,
			orbit_log_tail TEXT,
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
