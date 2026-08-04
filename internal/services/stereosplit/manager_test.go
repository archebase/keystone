// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"github.com/foxglove/mcap/go/mcap"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

const testImageDigest = "ghcr.io/archebase/stereo-split@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestManagerStartCreatesOneQueuedDerivative(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 1, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")

	manager := NewManager(db, nil, nil, Config{Enabled: true})
	first, created, err := manager.Start(context.Background(), 1, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !created {
		t.Fatal("Start() created = false, want true")
	}
	if first.EpisodeID != 1 || first.Kind != Kind || first.Generation != 1 || first.ProcessingStatus != ProcessingQueued {
		t.Fatalf("Start() derivative = %+v", first)
	}

	second, created, err := manager.Start(context.Background(), 1, "admin")
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if created {
		t.Fatal("second Start() created = true, want false")
	}
	if second.ID != first.ID || second.Generation != first.Generation {
		t.Fatalf("second Start() derivative = %+v, want id=%d generation=%d", second, first.ID, first.Generation)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM episode_derivatives WHERE episode_id = ? AND kind = ?", 1, Kind); err != nil {
		t.Fatalf("count derivatives: %v", err)
	}
	if count != 1 {
		t.Fatalf("derivative count = %d, want 1", count)
	}
}

func TestManagerPreparesReusableStereoSplitExecution(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	manager := NewManager(db, nil, &fakeObjectStore{size: 1024, etag: "source-etag"}, testManagerConfig())

	execution, err := manager.PrepareExecution(context.Background(), ExecutionInput{
		SourceBucket:    "source-bucket",
		SourceObjectKey: "calibration/capture.mcap",
		SourceChecksum:  "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		OutputScope:     "calibration/device/session/capture/stereo-split",
		SubmissionID:    "calibration-capture-stereo-split",
		Generation:      1,
	})
	if err != nil {
		t.Fatalf("PrepareExecution() error = %v", err)
	}
	if execution.ProcessorImage != testImageDigest || execution.SourceSizeBytes != 1024 ||
		execution.SourceETag != "source-etag" || execution.OutputBucket != "output-bucket" {
		t.Fatalf("PrepareExecution() = %+v", execution)
	}
	if execution.Request.Image != testImageDigest || execution.Request.SubmissionID != "calibration-capture-stereo-split" ||
		len(execution.Request.DataBindings) != 2 {
		t.Fatalf("PrepareExecution() request = %+v", execution.Request)
	}
	if got := execution.Request.DataBindings[0].URI; got != "tos://source-bucket/calibration/capture.mcap" {
		t.Fatalf("input URI = %q", got)
	}
	if got := execution.Request.DataBindings[1].URI; !strings.HasPrefix(got, "tos://output-bucket/derived/episodes/calibration/device/session/capture/stereo-split/g1-") {
		t.Fatalf("output URI = %q", got)
	}
}

func TestManagerVerifiesReusableStereoSplitExecution(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	objects := &fakeObjectStore{size: 1024, etag: "source-etag"}
	manager := NewManager(db, nil, objects, testManagerConfig())
	execution, err := manager.PrepareExecution(context.Background(), ExecutionInput{
		SourceBucket:    "source-bucket",
		SourceObjectKey: "calibration/capture.mcap",
		OutputScope:     "calibration/device/session/capture/stereo-split",
		SubmissionID:    "calibration-capture-stereo-split",
		Generation:      1,
	})
	if err != nil {
		t.Fatalf("PrepareExecution() error = %v", err)
	}
	manifest := `{
		"schema_version":1,
		"status":"succeeded",
		"kind":"stereo_split",
		"generation":1,
		"processor_image":"` + testImageDigest + `",
		"source":{"uri":"` + execution.SourceURI + `","size_bytes":1024,"sha256":""},
		"outputs":{
			"mcap":{"name":"output_bag.mcap","size_bytes":32,"sha256":"` + strings.Repeat("a", 64) + `"},
			"metadata":{"name":"metadata.yaml","size_bytes":16,"sha256":"` + strings.Repeat("b", 64) + `"}
		},
		"stats":{"input_messages":1,"decoded_images":1,"left_images":1,"right_images":1,"imu_messages":1,"skipped_messages":0},
		"started_at":"2026-08-02T10:00:00Z",
		"finished_at":"2026-08-02T10:00:01Z"
	}`
	objects.objects = map[string]fakeStoredObject{
		path.Join(execution.OutputPrefix, manifestName):       {size: int64(len(manifest)), etag: "manifest-etag", body: manifest},
		path.Join(execution.OutputPrefix, outputMcapName):     {size: 32, etag: "mcap-etag", body: strings.Repeat("m", 32)},
		path.Join(execution.OutputPrefix, outputMetadataName): {size: 16, etag: "metadata-etag", body: strings.Repeat("y", 16)},
	}

	output, err := manager.VerifyExecution(context.Background(), execution)
	if err != nil {
		t.Fatalf("VerifyExecution() error = %v", err)
	}
	if output.MCAPObjectKey != path.Join(execution.OutputPrefix, outputMcapName) ||
		output.MCAPSizeBytes != 32 || output.MCAPChecksumSHA256 != strings.Repeat("a", 64) ||
		output.MCAPETag != "mcap-etag" || output.ManifestJSON != manifest {
		t.Fatalf("VerifyExecution() = %+v", output)
	}
}

func TestManagerVerifyExecutionRejectsEmptyFrozenSourceETag(t *testing.T) {
	manager := NewManager(newTestDB(t), nil, &fakeObjectStore{}, testManagerConfig())

	_, err := manager.VerifyExecution(context.Background(), ExecutionSnapshot{})
	if err == nil || !strings.Contains(err.Error(), "source ETag is empty") {
		t.Fatalf("VerifyExecution() error = %v", err)
	}
}

func TestManagerStartRejectsOriginalCloudSource(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 2, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, CloudSourceOriginal)

	manager := NewManager(db, nil, nil, Config{Enabled: true})
	_, _, err := manager.Start(context.Background(), 2, "admin")
	if !errors.Is(err, ErrCloudSourceLocked) {
		t.Fatalf("Start() error = %v, want ErrCloudSourceLocked", err)
	}
}

func TestManagerRetryCreatesFreshGenerationAfterOrbitDeleteAccepted(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 5, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	manager := NewManager(db, nil, nil, Config{Enabled: true})
	derivative, _, err := manager.Start(context.Background(), 5, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, orbit_delete_status = ?,
		    processor_image = ?, orbit_request = '{}', output_prefix = 'old/output',
		    qa_status = ?, processing_error = 'failed'
		WHERE id = ?
	`, ProcessingFailed, DeleteCompleted, testImageDigest, QAFailed, derivative.ID); err != nil {
		t.Fatalf("prepare failed derivative: %v", err)
	}

	retried, err := manager.Retry(context.Background(), 5, "admin")
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if retried.Generation != 2 || retried.ProcessingStatus != ProcessingQueued || retried.QAStatus != QANotStarted || retried.OrbitDeleteStatus != DeleteNotRequired {
		t.Fatalf("Retry() derivative = %+v", retried)
	}
	if retried.ProcessorImage != "" || retried.OutputPrefix != "" || retried.ProcessingError != "" {
		t.Fatalf("Retry() retained old execution fields: %+v", retried)
	}
}

func TestManagerAdmitBulkPersistsStableAdmissionResults(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 20, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/new.mcap"}`, "")
	insertTestEpisode(t, db, 21, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/retry.mcap"}`, "")
	insertTestEpisode(t, db, 22, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/done.mcap"}`, "")
	insertTestEpisode(t, db, 23, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/active.mcap"}`, "")
	insertTestEpisode(t, db, 24, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/original.mcap"}`, CloudSourceOriginal)
	if _, err := db.Exec(`INSERT INTO bulk_runs (run_id, action, status) VALUES ('stereo_run_1', 'stereo_split', 'running')`); err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}

	manager := NewManager(db, nil, nil, Config{Enabled: true})
	failed, _, err := manager.Start(context.Background(), 21, "admin")
	if err != nil {
		t.Fatalf("prepare retry derivative: %v", err)
	}
	done, _, err := manager.Start(context.Background(), 22, "admin")
	if err != nil {
		t.Fatalf("prepare succeeded derivative: %v", err)
	}
	if _, _, err := manager.Start(context.Background(), 23, "admin"); err != nil {
		t.Fatalf("prepare active derivative: %v", err)
	}
	if _, err := db.Exec(`UPDATE episode_derivatives SET processing_status = ?, orbit_delete_status = ? WHERE id = ?`, ProcessingFailed, DeleteCompleted, failed.ID); err != nil {
		t.Fatalf("mark failed derivative: %v", err)
	}
	if _, err := db.Exec(`UPDATE episode_derivatives SET processing_status = ?, qa_status = ?, orbit_delete_status = ? WHERE id = ?`, ProcessingSucceeded, QAApproved, DeleteCompleted, done.ID); err != nil {
		t.Fatalf("mark succeeded derivative: %v", err)
	}

	tests := []struct {
		episodeID      int64
		wantAdmission  string
		wantReason     string
		wantGeneration int
	}{
		{episodeID: 20, wantAdmission: BulkAdmissionAdmitted, wantReason: BulkReasonEligible, wantGeneration: 1},
		{episodeID: 21, wantAdmission: BulkAdmissionAdmitted, wantReason: BulkReasonEligibleRetry, wantGeneration: 2},
		{episodeID: 22, wantAdmission: BulkAdmissionSkipped, wantReason: BulkReasonAlreadyDerived, wantGeneration: 1},
		{episodeID: 23, wantAdmission: BulkAdmissionSkipped, wantReason: BulkReasonProcessingActive, wantGeneration: 1},
		{episodeID: 24, wantAdmission: BulkAdmissionSkipped, wantReason: BulkReasonCloudSourceLocked},
	}
	for _, tt := range tests {
		result, err := manager.AdmitBulk(context.Background(), "stereo_run_1", tt.episodeID, "admin")
		if err != nil {
			t.Fatalf("AdmitBulk(%d) error = %v", tt.episodeID, err)
		}
		if result.AdmissionStatus != tt.wantAdmission || result.Reason != tt.wantReason {
			t.Fatalf("AdmitBulk(%d) = %+v", tt.episodeID, result)
		}
		if tt.wantGeneration > 0 && result.DerivativeGeneration != tt.wantGeneration {
			t.Fatalf("AdmitBulk(%d) generation = %d, want %d", tt.episodeID, result.DerivativeGeneration, tt.wantGeneration)
		}
	}

	result, err := manager.AdmitBulk(context.Background(), "stereo_run_1", 20, "admin")
	if err != nil {
		t.Fatalf("idempotent AdmitBulk() error = %v", err)
	}
	if result.AdmissionStatus != BulkAdmissionAdmitted || result.DerivativeGeneration != 1 {
		t.Fatalf("idempotent AdmitBulk() = %+v", result)
	}
	var itemCount int
	if err := db.Get(&itemCount, `SELECT COUNT(*) FROM bulk_run_items WHERE bulk_run_id = ?`, "stereo_run_1"); err != nil {
		t.Fatalf("count bulk items: %v", err)
	}
	if itemCount != len(tests) {
		t.Fatalf("bulk item count = %d, want %d", itemCount, len(tests))
	}
}

func TestManagerAdmitBulkStopsAfterRunCancellationRequested(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 27, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/canceled.mcap"}`, "")
	if _, err := db.Exec(`
		INSERT INTO bulk_runs (run_id, action, status)
		VALUES ('stereo_run_canceled', 'stereo_split', 'cancel_requested')
	`); err != nil {
		t.Fatalf("insert canceled bulk run: %v", err)
	}

	manager := NewManager(db, nil, nil, Config{Enabled: true})
	admission, err := manager.AdmitBulk(context.Background(), "stereo_run_canceled", 27, "admin")
	if err != nil {
		t.Fatalf("AdmitBulk() error = %v", err)
	}
	if admission.AdmissionStatus != BulkAdmissionCanceled || admission.Reason != BulkReasonCanceledBeforeAdmit {
		t.Fatalf("AdmitBulk() = %+v, want canceled before admission", admission)
	}
	var derivativeCount int
	if err := db.Get(&derivativeCount, "SELECT COUNT(*) FROM episode_derivatives WHERE episode_id = 27"); err != nil {
		t.Fatalf("count derivatives: %v", err)
	}
	if derivativeCount != 0 {
		t.Fatalf("derivative count = %d, want 0", derivativeCount)
	}
	var itemCount int
	if err := db.Get(&itemCount, "SELECT COUNT(*) FROM bulk_run_items WHERE bulk_run_id = 'stereo_run_canceled'"); err != nil {
		t.Fatalf("count bulk items: %v", err)
	}
	if itemCount != 0 {
		t.Fatalf("bulk item count = %d, want 0", itemCount)
	}
}

func TestManagerRetryFreezesOldBulkGenerationBeforeReset(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 25, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/retry-history.mcap"}`, "")
	if _, err := db.Exec(`INSERT INTO bulk_runs (run_id, action, status) VALUES ('stereo_run_retry', 'stereo_split', 'running')`); err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}
	manager := NewManager(db, nil, nil, Config{Enabled: true})
	admission, err := manager.AdmitBulk(context.Background(), "stereo_run_retry", 25, "admin")
	if err != nil {
		t.Fatalf("AdmitBulk() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, processing_error = 'transform failed',
		    processing_finished_at = CURRENT_TIMESTAMP, orbit_delete_status = ?
		WHERE id = ?
	`, ProcessingFailed, DeleteCompleted, admission.DerivativeID); err != nil {
		t.Fatalf("mark derivative failed: %v", err)
	}

	retried, err := manager.Retry(context.Background(), 25, "admin")
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if retried.Generation != 2 {
		t.Fatalf("Retry() generation = %d, want 2", retried.Generation)
	}
	var snapshot string
	if err := db.Get(&snapshot, `
		SELECT result_snapshot FROM bulk_run_items
		WHERE bulk_run_id = 'stereo_run_retry' AND episode_id = 25
	`); err != nil {
		t.Fatalf("load old bulk result snapshot: %v", err)
	}
	if !strings.Contains(snapshot, `"generation":1`) || !strings.Contains(snapshot, `"processing_status":"failed"`) {
		t.Fatalf("old bulk result snapshot = %s", snapshot)
	}
}

func TestManagerFreezesSucceededBulkResultSnapshot(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 26, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/succeeded.mcap"}`, "")
	if _, err := db.Exec(`INSERT INTO bulk_runs (run_id, action, status) VALUES ('stereo_run_done', 'stereo_split', 'running')`); err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}
	manager := NewManager(db, nil, nil, Config{Enabled: true})
	admission, err := manager.AdmitBulk(context.Background(), "stereo_run_done", 26, "admin")
	if err != nil {
		t.Fatalf("AdmitBulk() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, qa_status = ?, orbit_delete_status = ?,
		    mcap_path = 'derived/output.mcap', checksum = ?, file_size_bytes = 4096
		WHERE id = ?
	`, ProcessingSucceeded, QAApproved, DeleteCompleted, strings.Repeat("d", 64), admission.DerivativeID); err != nil {
		t.Fatalf("mark derivative succeeded: %v", err)
	}

	worked, err := manager.FreezeBulkResultSnapshotsOnce(context.Background())
	if err != nil {
		t.Fatalf("FreezeBulkResultSnapshotsOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("FreezeBulkResultSnapshotsOnce() worked = false, want true")
	}
	var snapshot string
	if err := db.Get(&snapshot, `SELECT result_snapshot FROM bulk_run_items WHERE bulk_run_id = 'stereo_run_done'`); err != nil {
		t.Fatalf("load succeeded snapshot: %v", err)
	}
	if !strings.Contains(snapshot, `"qa_status":"approved"`) || !strings.Contains(snapshot, `"file_size_bytes":4096`) {
		t.Fatalf("succeeded bulk result snapshot = %s", snapshot)
	}
}

func TestManagerCancelQueuedDoesNotCallOrbit(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 6, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	fake := &fakeOrbit{}
	manager := NewManager(db, fake, nil, Config{Enabled: true})
	if _, _, err := manager.Start(context.Background(), 6, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	canceled, err := manager.Cancel(context.Background(), 6, "admin")
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if canceled.ProcessingStatus != ProcessingCanceled || canceled.OrbitDeleteStatus != DeleteNotRequired || fake.deleteCalls != 0 {
		t.Fatalf("Cancel() derivative = %+v delete_calls=%d", canceled, fake.deleteCalls)
	}
}

func TestReconcileOnceStopsActiveJobAfterCancellation(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 7, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		fake.lastRequest = request
		return orbitapi.SubmitResponse{JobID: "active-job-7", SubmissionID: request.SubmissionID}, nil
	}
	manager := NewManager(db, fake, &fakeObjectStore{size: 100, etag: "source-etag"}, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 7, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("submit ReconcileOnce() error = %v", err)
	}
	if _, err := manager.Cancel(context.Background(), 7, "admin"); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	fake.getErr = nil
	fake.job = orbitapi.Job{
		JobID:        "active-job-7",
		SubmissionID: fake.lastRequest.SubmissionID,
		Status:       "RUNNING",
		Image:        fake.lastRequest.Image,
		DataBindings: fake.lastRequest.DataBindings,
	}
	fake.stopJob = orbitapi.Job{JobID: "active-job-7", Status: "STOPPED"}

	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("cancel ReconcileOnce() error = %v", err)
	}
	derivative, err := manager.Get(context.Background(), 7)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingCanceled || derivative.OrbitDeleteStatus != DeletePending || fake.stopCalls != 1 {
		t.Fatalf("canceled derivative=%+v stop_calls=%d", derivative, fake.stopCalls)
	}
}

func TestReconcileOnceHonorsGlobalOrbitCapacity(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 8, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/one.mcap"}`, "")
	insertTestEpisode(t, db, 9, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/two.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		return orbitapi.SubmitResponse{JobID: "job-" + request.SubmissionID, SubmissionID: request.SubmissionID}, nil
	}
	manager := NewManager(db, fake, &fakeObjectStore{size: 100, etag: "source-etag"}, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 8, "admin"); err != nil {
		t.Fatalf("Start(8) error = %v", err)
	}
	if _, _, err := manager.Start(context.Background(), 9, "admin"); err != nil {
		t.Fatalf("Start(9) error = %v", err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("first ReconcileOnce() worked=%v error=%v", worked, err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || worked {
		t.Fatalf("capacity ReconcileOnce() worked=%v error=%v", worked, err)
	}
	if fake.submitCalls != 1 {
		t.Fatalf("submit calls=%d want 1", fake.submitCalls)
	}
}

func TestManagerConcurrencySettingTakesEffectWithoutRestart(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 81, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/one.mcap"}`, "")
	insertTestEpisode(t, db, 82, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/two.mcap"}`, "")
	if _, err := db.Exec(`
		INSERT INTO stereo_split_image_configs (image_ref, max_concurrent, created_by)
		VALUES (?, 1, 'admin')
	`, testImageDigest); err != nil {
		t.Fatalf("insert processing settings: %v", err)
	}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		return orbitapi.SubmitResponse{JobID: "job-" + request.SubmissionID, SubmissionID: request.SubmissionID}, nil
	}
	manager := NewManager(db, fake, &fakeObjectStore{size: 100, etag: "source-etag"}, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 81, "admin"); err != nil {
		t.Fatalf("Start(81) error = %v", err)
	}
	if _, _, err := manager.Start(context.Background(), 82, "admin"); err != nil {
		t.Fatalf("Start(82) error = %v", err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("first ReconcileOnce() worked=%v error=%v", worked, err)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || worked {
		t.Fatalf("capacity ReconcileOnce() worked=%v error=%v", worked, err)
	}

	current, err := manager.CurrentImageConfig(context.Background())
	if err != nil {
		t.Fatalf("CurrentImageConfig() error = %v", err)
	}
	updated, err := manager.UpdateImageConfig(context.Background(), testImageDigest, 2, current.ID, "admin")
	if err != nil {
		t.Fatalf("UpdateImageConfig() error = %v", err)
	}
	if updated.MaxConcurrent != 2 || updated.PreviousMaxConcurrent == nil || *updated.PreviousMaxConcurrent != 1 {
		t.Fatalf("updated processing settings = %+v", updated)
	}
	if worked, err := manager.ReconcileOnce(context.Background()); err != nil || !worked {
		t.Fatalf("ReconcileOnce() after increasing concurrency worked=%v error=%v", worked, err)
	}
	if fake.submitCalls != 2 {
		t.Fatalf("submit calls=%d want 2", fake.submitCalls)
	}
}

func TestManagerConcurrencyUpdateWaitsForInFlightSubmission(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 83, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/racing.mcap"}`, "")
	if _, err := db.Exec(`
		INSERT INTO stereo_split_image_configs (image_ref, max_concurrent, created_by)
		VALUES (?, 2, 'admin')
	`, testImageDigest); err != nil {
		t.Fatalf("insert processing settings: %v", err)
	}

	submitEntered := make(chan struct{})
	releaseSubmit := make(chan struct{})
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		close(submitEntered)
		<-releaseSubmit
		return orbitapi.SubmitResponse{JobID: "job-" + request.SubmissionID, SubmissionID: request.SubmissionID}, nil
	}
	manager := NewManager(db, fake, &fakeObjectStore{size: 100, etag: "source-etag"}, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 83, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	current, err := manager.CurrentImageConfig(context.Background())
	if err != nil {
		t.Fatalf("CurrentImageConfig() error = %v", err)
	}

	reconcileDone := make(chan error, 1)
	go func() {
		_, reconcileErr := manager.ReconcileOnce(context.Background())
		reconcileDone <- reconcileErr
	}()
	<-submitEntered

	updateDone := make(chan error, 1)
	go func() {
		_, updateErr := manager.UpdateImageConfig(context.Background(), testImageDigest, 1, current.ID, "admin")
		updateDone <- updateErr
	}()
	select {
	case updateErr := <-updateDone:
		t.Fatalf("concurrency update completed before in-flight submit: %v", updateErr)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseSubmit)
	if reconcileErr := <-reconcileDone; reconcileErr != nil {
		t.Fatalf("ReconcileOnce() error = %v", reconcileErr)
	}
	if updateErr := <-updateDone; updateErr != nil {
		t.Fatalf("UpdateImageConfig() error = %v", updateErr)
	}
}

func TestManagerUpdateImageAcceptsAnyValidDigestRepository(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (NULL, 'migration-bootstrap')"); err != nil {
		t.Fatalf("insert bootstrap revision: %v", err)
	}
	manager := NewManager(db, nil, nil, Config{})

	updated, err := manager.UpdateImageConfig(
		context.Background(),
		"registry.example.com/team/stereo-split@sha256:"+strings.Repeat("c", 64),
		1,
		1,
		"admin",
	)
	if err != nil {
		t.Fatalf("UpdateImageConfig() error = %v", err)
	}
	if updated.ImageRef != "registry.example.com/team/stereo-split@sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("UpdateImageConfig().ImageRef = %q", updated.ImageRef)
	}
}

func TestReconcileOncePersistsCompleteSnapshotBeforeOrbitSubmit(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 3, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	manager := NewManager(db, nil, &fakeObjectStore{size: 301234567, etag: "source-etag"}, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 3, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		var frozen struct {
			Status       string `db:"processing_status"`
			OrbitRequest string `db:"orbit_request"`
			OutputPrefix string `db:"output_prefix"`
			SubmissionID string `db:"orbit_submission_id"`
		}
		if err := db.Get(&frozen, `
			SELECT processing_status, orbit_request, output_prefix, orbit_submission_id
			FROM episode_derivatives WHERE episode_id = 3 AND kind = ?
		`, Kind); err != nil {
			t.Fatalf("load frozen derivative during Submit: %v", err)
		}
		if frozen.Status != ProcessingSubmitting || frozen.OrbitRequest == "" || frozen.OutputPrefix == "" || frozen.SubmissionID != request.SubmissionID {
			t.Fatalf("snapshot at Submit = %+v request=%+v", frozen, request)
		}
		return orbitapi.SubmitResponse{JobID: "abs-job-derivative-3", SubmissionID: request.SubmissionID}, nil
	}
	manager.orbit = fake

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked || fake.submitCalls != 1 {
		t.Fatalf("ReconcileOnce() worked=%v submit_calls=%d", worked, fake.submitCalls)
	}
	derivative, err := manager.Get(context.Background(), 3)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingPending || derivative.OrbitJobID != "abs-job-derivative-3" || derivative.SourceURI != "tos://source-bucket/raw/source.mcap" {
		t.Fatalf("derivative after submit = %+v", derivative)
	}
}

func TestReconcileOnceVerifiesRunsQAAndRequestsOrbitDelete(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 4, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	objects := &fakeObjectStore{size: 301234567, etag: "source-etag"}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		fake.lastRequest = request
		return orbitapi.SubmitResponse{JobID: "abs-job-derivative-4", SubmissionID: request.SubmissionID}, nil
	}
	manager := NewManager(db, fake, objects, testManagerConfig())
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, _, err := manager.Start(context.Background(), 4, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("submit ReconcileOnce() error = %v", err)
	}

	fake.getErr = nil
	fake.job = orbitapi.Job{
		JobID:        "abs-job-derivative-4",
		SubmissionID: fake.lastRequest.SubmissionID,
		Status:       "SUCCEEDED",
		Image:        fake.lastRequest.Image,
		DataBindings: fake.lastRequest.DataBindings,
	}
	now = now.Add(2 * time.Second)
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("terminal ReconcileOnce() error = %v", err)
	}

	derivative, err := manager.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get() after Orbit success error = %v", err)
	}
	outputMCAP, outputChecksum := makeStereoSplitQAOutput(t, 10, 10, 101)
	manifest := `{
		"schema_version":1,
		"status":"succeeded",
		"kind":"stereo_split",
		"generation":1,
		"processor_image":"` + testImageDigest + `",
		"source":{"uri":"` + derivative.SourceURI + `","binding_path":"/bindings/input/source.mcap","size_bytes":301234567,"sha256":""},
		"outputs":{
			"mcap":{"name":"output_bag.mcap","size_bytes":` + strconv.Itoa(len(outputMCAP)) + `,"sha256":"` + outputChecksum + `"},
			"metadata":{"name":"metadata.yaml","size_bytes":50,"sha256":"` + strings.Repeat("c", 64) + `"}
		},
		"stats":{"input_messages":10,"decoded_images":10,"left_images":10,"right_images":10,"imu_messages":101,"skipped_messages":0},
		"started_at":"2026-08-02T10:00:00Z",
		"finished_at":"2026-08-02T10:00:10Z"
	}`
	objects.objects = map[string]fakeStoredObject{
		derivative.OutputPrefix + "/processing_manifest.json": {size: int64(len(manifest)), etag: "manifest-etag", body: manifest},
		derivative.OutputPrefix + "/output_bag.mcap":          {size: int64(len(outputMCAP)), etag: "mcap-etag", body: string(outputMCAP)},
		derivative.OutputPrefix + "/metadata.yaml":            {size: 50, etag: "metadata-etag", body: "metadata"},
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("verify ReconcileOnce() error = %v", err)
	}
	derivative, err = manager.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get() after output verification error = %v", err)
	}
	if derivative.DurationSec != nil {
		t.Fatalf("duration_sec after output verification = %v, want nil until QA", *derivative.DurationSec)
	}
	if derivative.ProcessingDurationSec == nil || *derivative.ProcessingDurationSec != 10 {
		t.Fatalf("processing_duration_sec after output verification = %v, want 10", derivative.ProcessingDurationSec)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("QA ReconcileOnce() error = %v", err)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("delete ReconcileOnce() error = %v", err)
	}

	derivative, err = manager.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get() final error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingSucceeded || derivative.QAStatus != QAApproved || derivative.OrbitDeleteStatus != DeleteCompleted {
		t.Fatalf("final derivative = %+v", derivative)
	}
	if derivative.McapPath == "" || derivative.Checksum != outputChecksum || fake.deleteCalls != 1 {
		t.Fatalf("final outputs/delete = %+v delete_calls=%d", derivative, fake.deleteCalls)
	}
	if derivative.DurationSec == nil || *derivative.DurationSec != 0.1 {
		t.Fatalf("final duration_sec = %v, want 0.1", derivative.DurationSec)
	}
	if derivative.ProcessingDurationSec == nil || *derivative.ProcessingDurationSec != 10 {
		t.Fatalf("final processing_duration_sec = %v, want 10", derivative.ProcessingDurationSec)
	}
}

func TestReconcileCancellationConfirmsMissingSubmittedJobAfterGrace(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 28, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(context.Context, orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		return orbitapi.SubmitResponse{}, errors.New("submit response was lost")
	}
	manager := NewManager(db, fake, &fakeObjectStore{size: 301234567, etag: "source-etag"}, testManagerConfig())
	now := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, _, err := manager.Start(context.Background(), 28, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("submit ReconcileOnce() error = nil, want ambiguous submit error")
	}
	if _, err := manager.Cancel(context.Background(), 28, "admin"); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("first missing cancellation ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("first missing cancellation ReconcileOnce() worked = false")
	}
	var first struct {
		Status   string       `db:"processing_status"`
		AbsentAt sql.NullTime `db:"orbit_submit_absent_at"`
	}
	if err := db.Get(&first, `
		SELECT processing_status, orbit_submit_absent_at
		FROM episode_derivatives WHERE episode_id = 28 AND kind = ?
	`, Kind); err != nil {
		t.Fatalf("load first absence observation: %v", err)
	}
	if first.Status != ProcessingSubmitting || !first.AbsentAt.Valid {
		t.Fatalf("first absence observation = %+v, want submitting with timestamp", first)
	}

	now = now.Add(manager.submissionAbsenceGrace() - time.Second)
	worked, err = manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("early confirmation ReconcileOnce() error = %v", err)
	}
	if worked {
		t.Fatal("early confirmation ReconcileOnce() worked = true before grace")
	}

	now = now.Add(time.Second)
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("confirmed missing cancellation ReconcileOnce() error = %v", err)
	}
	derivative, err := manager.Get(context.Background(), 28)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingCanceled || derivative.OrbitDeleteStatus != DeleteNotRequired {
		t.Fatalf("confirmed missing derivative = %+v", derivative)
	}
}

func TestReconcileMissingActiveJobFailsAfterGraceAndReleasesCapacity(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 29, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	if _, err := db.Exec("INSERT INTO stereo_split_image_configs (image_ref, created_by) VALUES (?, 'admin')", testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	fake := &fakeOrbit{getErr: orbitapi.ErrNotFound}
	fake.submit = func(_ context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
		fake.lastRequest = request
		return orbitapi.SubmitResponse{JobID: "abs-job-derivative-29", SubmissionID: request.SubmissionID}, nil
	}
	config := testManagerConfig()
	manager := NewManager(db, fake, &fakeObjectStore{size: 301234567, etag: "source-etag"}, config)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if _, _, err := manager.Start(context.Background(), 29, "admin"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("submit ReconcileOnce() error = %v", err)
	}

	now = now.Add(manager.pollInterval())
	if _, err := manager.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("first missing active ReconcileOnce() error = nil")
	}
	derivative, err := manager.Get(context.Background(), 29)
	if err != nil {
		t.Fatalf("Get() after first missing error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingPending {
		t.Fatalf("first missing active derivative = %+v", derivative)
	}

	now = now.Add(manager.activeJobMissingGrace())
	if _, err := manager.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expired missing active ReconcileOnce() error = nil")
	}
	derivative, err = manager.Get(context.Background(), 29)
	if err != nil {
		t.Fatalf("Get() after missing grace error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingFailed || derivative.OrbitDeleteStatus != DeleteCompleted {
		t.Fatalf("expired missing active derivative = %+v", derivative)
	}
	atCapacity, err := manager.atOrbitCapacity(context.Background())
	if err != nil {
		t.Fatalf("atOrbitCapacity() error = %v", err)
	}
	if atCapacity {
		t.Fatal("atOrbitCapacity() = true after missing job failure")
	}
}

func TestVerifySucceededRejectsChangedSourceETagAtSameSize(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 30, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	manager := NewManager(db, nil, &fakeObjectStore{size: 301234567, etag: "changed-etag"}, testManagerConfig())
	derivative, _, err := manager.Start(context.Background(), 30, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, processor_image = ?,
		    source_uri = 'tos://source-bucket/raw/source.mcap', source_etag = 'source-etag',
		    source_size_bytes = 301234567, output_prefix = 'derived/etag-change'
		WHERE id = ?
	`, ProcessingVerifying, testImageDigest, derivative.ID); err != nil {
		t.Fatalf("prepare verifying derivative: %v", err)
	}

	if _, err := manager.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("ReconcileOnce() error = nil, want changed source identity error")
	}
	derivative, err = manager.Get(context.Background(), 30)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.ProcessingStatus != ProcessingFailed || !strings.Contains(derivative.ProcessingError, "identity changed") {
		t.Fatalf("changed source derivative = %+v", derivative)
	}
}

func TestVerifySucceededLimitsTransientManifestRetries(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 31, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	manifestKey := "derived/transient/" + manifestName
	objects := &fakeObjectStore{
		size:       301234567,
		etag:       "source-etag",
		openErrors: map[string]error{manifestKey: errors.New("manifest is not visible yet")},
	}
	manager := NewManager(db, nil, objects, testManagerConfig())
	now := time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	derivative, _, err := manager.Start(context.Background(), 31, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, processor_image = ?,
		    source_uri = 'tos://source-bucket/raw/source.mcap', source_etag = 'source-etag',
		    source_size_bytes = 301234567, output_prefix = 'derived/transient'
		WHERE id = ?
	`, ProcessingVerifying, testImageDigest, derivative.ID); err != nil {
		t.Fatalf("prepare verifying derivative: %v", err)
	}

	for attempt := 1; attempt <= maxVerificationAttempts; attempt++ {
		if _, err := manager.ReconcileOnce(context.Background()); err == nil {
			t.Fatalf("ReconcileOnce() attempt %d error = nil", attempt)
		}
		var state struct {
			Status   string `db:"processing_status"`
			Attempts int    `db:"verification_attempt_count"`
		}
		if err := db.Get(&state, `
			SELECT processing_status, verification_attempt_count
			FROM episode_derivatives WHERE id = ?
		`, derivative.ID); err != nil {
			t.Fatalf("load verification attempt %d: %v", attempt, err)
		}
		if attempt < maxVerificationAttempts {
			if state.Status != ProcessingVerifying || state.Attempts != attempt {
				t.Fatalf("verification attempt %d state = %+v", attempt, state)
			}
		} else if state.Status != ProcessingFailed || state.Attempts != maxVerificationAttempts-1 {
			t.Fatalf("verification terminal state = %+v", state)
		}
		now = now.Add(manager.pollInterval())
	}
}

func TestReconcileQAFailsWhenManifestCountsDoNotMatchMCAP(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 32, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	outputMCAP, outputChecksum := makeStereoSplitQAOutput(t, 10, 10, 100)
	outputKey := "derived/qa-mismatch/output_bag.mcap"
	objects := &fakeObjectStore{objects: map[string]fakeStoredObject{
		outputKey: {size: int64(len(outputMCAP)), etag: "mcap-etag", body: string(outputMCAP)},
	}}
	manager := NewManager(db, nil, objects, testManagerConfig())
	derivative, _, err := manager.Start(context.Background(), 32, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	processingResult := `{"schema_version":1,"stats":{"input_messages":11,"decoded_images":11,"left_images":11,"right_images":11,"imu_messages":100,"skipped_messages":0}}`
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, qa_status = ?, mcap_path = ?,
		    checksum = ?, processing_result = ?, orbit_delete_status = ?
		WHERE id = ?
	`, ProcessingSucceeded, QAPending, outputKey, outputChecksum, processingResult, DeletePending, derivative.ID); err != nil {
		t.Fatalf("prepare QA derivative: %v", err)
	}

	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	derivative, err = manager.Get(context.Background(), 32)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.QAStatus != QAFailed || !strings.Contains(derivative.QAError, "counts do not match") {
		t.Fatalf("QA mismatch derivative = %+v", derivative)
	}
}

func TestReconcileQAApprovesStereoH264ManifestV2(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 33, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	outputMCAP, outputChecksum := makeStereoSplitH264QAOutput(t, 10, 10, 100)
	outputKey := "derived/qa-h264/output_bag.mcap"
	objects := &fakeObjectStore{objects: map[string]fakeStoredObject{
		outputKey: {size: int64(len(outputMCAP)), etag: "mcap-etag", body: string(outputMCAP)},
	}}
	manager := NewManager(db, nil, objects, testManagerConfig())
	derivative, _, err := manager.Start(context.Background(), 33, "admin")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	processingResult := `{
		"schema_version":2,
		"output_format":"stereo_h264",
		"stats":{
			"input_messages":10,
			"decoded_images":10,
			"left_videos":10,
			"right_videos":10,
			"imu_messages":100,
			"copied_messages":3,
			"copied_topics":1,
			"skipped_messages":0
		}
	}`
	if _, err := db.Exec(`
		UPDATE episode_derivatives SET processing_status = ?, qa_status = ?, mcap_path = ?,
		    checksum = ?, processing_result = ?, orbit_delete_status = ?
		WHERE id = ?
	`, ProcessingSucceeded, QAPending, outputKey, outputChecksum, processingResult, DeletePending, derivative.ID); err != nil {
		t.Fatalf("prepare H.264 QA derivative: %v", err)
	}

	if _, err := manager.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	derivative, err = manager.Get(context.Background(), 33)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if derivative.QAStatus != QAApproved || derivative.QAError != "" {
		t.Fatalf("H.264 QA derivative = %+v", derivative)
	}
	if derivative.DurationSec == nil || *derivative.DurationSec <= 0 {
		t.Fatalf("H.264 QA duration = %v", derivative.DurationSec)
	}
}

func TestValidateManifestSnapshotSupportsStereoSplitV1AndV2(t *testing.T) {
	row := verificationRow{
		Generation:     1,
		ProcessorImage: testImageDigest,
		SourceURI:      "tos://source-bucket/raw/source.mcap",
		SourceSize:     123,
	}
	decode := func(payload string) processingManifest {
		t.Helper()
		var manifest processingManifest
		if err := json.Unmarshal([]byte(payload), &manifest); err != nil {
			t.Fatalf("decode manifest fixture: %v", err)
		}
		return manifest
	}
	common := `
		"status":"succeeded",
		"kind":"stereo_split",
		"generation":1,
		"processor_image":"` + testImageDigest + `",
		"source":{"uri":"tos://source-bucket/raw/source.mcap","size_bytes":123,"sha256":""},
		"outputs":{
			"mcap":{"name":"output_bag.mcap","size_bytes":10,"sha256":"` + strings.Repeat("a", 64) + `"},
			"metadata":{"name":"metadata.yaml","size_bytes":10,"sha256":"` + strings.Repeat("b", 64) + `"}
		},
		"started_at":"2026-08-02T10:00:00Z",
		"finished_at":"2026-08-02T10:00:01Z"`

	for name, payload := range map[string]string{
		"v1": `{"schema_version":1,` + common + `}`,
		"v2": `{"schema_version":2,"output_format":"stereo_h264",` + common + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateManifestSnapshot(decode(payload), row); err != nil {
				t.Fatalf("validateManifestSnapshot() error = %v", err)
			}
		})
	}

	invalid := decode(`{"schema_version":2,"output_format":"",` + common + `}`)
	if err := validateManifestSnapshot(invalid, row); err == nil || !strings.Contains(err.Error(), "output format") {
		t.Fatalf("validateManifestSnapshot() invalid format error = %v", err)
	}
}

func makeStereoSplitQAOutput(t *testing.T, leftCount, rightCount, imuCount int) ([]byte, string) {
	return makeStereoSplitQAOutputForContract(
		t,
		compressedImageSchema,
		"ros2msg",
		leftImageTopic,
		rightImageTopic,
		"cdr",
		leftCount,
		rightCount,
		imuCount,
	)
}

func makeStereoSplitH264QAOutput(t *testing.T, leftCount, rightCount, imuCount int) ([]byte, string) {
	return makeStereoSplitQAOutputForContract(
		t,
		compressedVideoSchema,
		"protobuf",
		leftVideoTopic,
		rightVideoTopic,
		"protobuf",
		leftCount,
		rightCount,
		imuCount,
	)
}

func makeStereoSplitQAOutputForContract(
	t *testing.T,
	stereoSchema string,
	stereoSchemaEncoding string,
	leftTopic string,
	rightTopic string,
	stereoMessageEncoding string,
	leftCount int,
	rightCount int,
	imuCount int,
) ([]byte, string) {
	t.Helper()
	var output bytes.Buffer
	writer, err := mcap.NewWriter(&output, &mcap.WriterOptions{Chunked: false})
	if err != nil {
		t.Fatalf("create MCAP writer: %v", err)
	}
	if err := writer.WriteHeader(&mcap.Header{Profile: "ros2"}); err != nil {
		t.Fatalf("write MCAP header: %v", err)
	}
	for _, schema := range []*mcap.Schema{
		{ID: 1, Name: stereoSchema, Encoding: stereoSchemaEncoding, Data: []byte("test")},
		{ID: 2, Name: imuSchema, Encoding: "ros2msg", Data: []byte("test")},
	} {
		if err := writer.WriteSchema(schema); err != nil {
			t.Fatalf("write MCAP schema: %v", err)
		}
	}
	for _, channel := range []*mcap.Channel{
		{ID: 1, SchemaID: 1, Topic: leftTopic, MessageEncoding: stereoMessageEncoding},
		{ID: 2, SchemaID: 1, Topic: rightTopic, MessageEncoding: stereoMessageEncoding},
		{ID: 3, SchemaID: 2, Topic: imuTopic, MessageEncoding: "cdr"},
	} {
		if err := writer.WriteChannel(channel); err != nil {
			t.Fatalf("write MCAP channel: %v", err)
		}
	}
	writeMessages := func(channelID uint16, count int, base uint64) {
		for index := 0; index < count; index++ {
			logTime := base + uint64(index)*uint64(time.Millisecond)
			if err := writer.WriteMessage(&mcap.Message{
				ChannelID:   channelID,
				LogTime:     logTime,
				PublishTime: logTime,
				Data:        []byte{byte(index)},
			}); err != nil {
				t.Fatalf("write MCAP message: %v", err)
			}
		}
	}
	writeMessages(1, leftCount, uint64(time.Second))
	writeMessages(2, rightCount, uint64(time.Second))
	writeMessages(3, imuCount, uint64(time.Second))
	if err := writer.Close(); err != nil {
		t.Fatalf("close MCAP writer: %v", err)
	}
	digest := sha256.Sum256(output.Bytes())
	return output.Bytes(), hex.EncodeToString(digest[:])
}

func testManagerConfig() Config {
	return Config{
		Enabled:      true,
		OutputBucket: "output-bucket",
		OutputPrefix: "derived/episodes",
		Resources: Resources{
			Requests: map[string]string{"cpu": "2", "memory": "4Gi", "ephemeral-storage": "4Gi"},
			Limits:   map[string]string{"cpu": "4", "memory": "8Gi", "ephemeral-storage": "8Gi"},
		},
		ActiveDeadline:      3600,
		TTLSecondsAfterDone: 604800,
		PollInterval:        time.Second,
		MaxSourceBytes:      500 * 1024 * 1024 * 1024,
		LogTailBytes:        1024 * 1024,
	}
}

type fakeOrbit struct {
	submit      func(context.Context, orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error)
	submitCalls int
	job         orbitapi.Job
	getErr      error
	lastRequest orbitapi.SubmitRequest
	deleteCalls int
	stopCalls   int
	stopJob     orbitapi.Job
	stopErr     error
}

func (f *fakeOrbit) Submit(ctx context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error) {
	f.submitCalls++
	if f.submit != nil {
		return f.submit(ctx, request)
	}
	return orbitapi.SubmitResponse{}, errors.New("unexpected Submit")
}

func (f *fakeOrbit) Get(context.Context, string) (orbitapi.Job, error) {
	return f.job, f.getErr
}

func (f *fakeOrbit) Logs(context.Context, string) (string, error) { return "", nil }
func (f *fakeOrbit) Stop(context.Context, string) (orbitapi.Job, error) {
	f.stopCalls++
	if f.stopErr != nil {
		return orbitapi.Job{}, f.stopErr
	}
	if strings.TrimSpace(f.stopJob.Status) != "" {
		return f.stopJob, nil
	}
	return orbitapi.Job{Status: "STOPPED"}, nil
}
func (f *fakeOrbit) Delete(context.Context, string) error {
	f.deleteCalls++
	return nil
}

type fakeStoredObject struct {
	size int64
	etag string
	body string
}

type fakeObjectStore struct {
	size       int64
	etag       string
	body       string
	objects    map[string]fakeStoredObject
	openErrors map[string]error
}

func (f *fakeObjectStore) StatObject(_ context.Context, _ string, objectName string) (int64, string, error) {
	if object, ok := f.objects[objectName]; ok {
		return object.size, object.etag, nil
	}
	return f.size, f.etag, nil
}

func (f *fakeObjectStore) OpenObject(_ context.Context, _ string, objectName string) (io.ReadCloser, error) {
	if err, ok := f.openErrors[objectName]; ok {
		return nil, err
	}
	if object, ok := f.objects[objectName]; ok {
		return io.NopCloser(strings.NewReader(object.body)), nil
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func (f *fakeObjectStore) OpenObjectRange(_ context.Context, _ string, objectName string, offset, length, _ int64, _ string) (io.ReadCloser, error) {
	if object, ok := f.objects[objectName]; ok {
		if objectName == path.Join(path.Dir(objectName), outputMcapName) {
			return io.NopCloser(strings.NewReader(string(mcapMagic))), nil
		}
		body := object.body
		start := int(offset)
		end := min(len(body), start+int(length))
		if start >= 0 && start < end {
			return io.NopCloser(strings.NewReader(body[start:end])), nil
		}
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			storage_backend TEXT NOT NULL,
			mcap_path TEXT NOT NULL,
			checksum TEXT,
			file_size_bytes INTEGER,
			duration_sec REAL,
			metadata TEXT,
			qa_status TEXT NOT NULL DEFAULT 'pending_qa',
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			cloud_synced_at TIMESTAMP NULL,
			cloud_mcap_path TEXT,
			hilbert_raw_data_id INTEGER,
			cloud_publish_source TEXT,
			cloud_publish_claimed_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			source_snapshot TEXT,
			started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE episode_derivatives (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			generation INTEGER NOT NULL,
			processor_config_revision_id INTEGER,
			processor_image TEXT,
			source_uri TEXT,
			source_etag TEXT,
			source_checksum TEXT,
			source_size_bytes INTEGER,
			processing_status TEXT NOT NULL DEFAULT 'queued',
			cancel_requested_at TIMESTAMP NULL,
			reconcile_after TIMESTAMP NULL,
			orbit_submission_id TEXT,
			orbit_request TEXT,
			orbit_snapshot_frozen_at TIMESTAMP NULL,
			orbit_job_id TEXT,
			orbit_submit_absent_at TIMESTAMP NULL,
			orbit_job_missing_since TIMESTAMP NULL,
			output_prefix TEXT,
			mcap_path TEXT,
			metadata_path TEXT,
			manifest_path TEXT,
			checksum TEXT,
			file_size_bytes INTEGER,
			duration_sec REAL,
			processing_duration_sec REAL,
			processing_result TEXT,
			processing_error TEXT,
			orbit_log_tail TEXT,
			submit_attempt_count INTEGER NOT NULL DEFAULT 0,
			verification_attempt_count INTEGER NOT NULL DEFAULT 0,
			processing_started_at TIMESTAMP NULL,
			processing_finished_at TIMESTAMP NULL,
			orbit_delete_status TEXT NOT NULL DEFAULT 'not_required',
			orbit_delete_attempt_count INTEGER NOT NULL DEFAULT 0,
			orbit_delete_next_retry_at TIMESTAMP NULL,
			orbit_delete_error TEXT,
			orbit_delete_accepted_at TIMESTAMP NULL,
			qa_status TEXT NOT NULL DEFAULT 'not_started',
			qa_attempt_count INTEGER NOT NULL DEFAULT 0,
			qa_next_retry_at TIMESTAMP NULL,
			qa_score REAL,
			quality_flag TEXT,
			qa_result TEXT,
			qa_error TEXT,
			qa_started_at TIMESTAMP NULL,
			qa_finished_at TIMESTAMP NULL,
			created_by TEXT,
			updated_by TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (episode_id, kind),
			UNIQUE (orbit_submission_id)
		)`,
		`CREATE TABLE stereo_split_image_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_ref TEXT,
			previous_image_ref TEXT,
			max_concurrent INTEGER NOT NULL DEFAULT 1,
			previous_max_concurrent INTEGER,
			created_by TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE bulk_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL UNIQUE,
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			materialize_cursor INTEGER,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE bulk_run_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			bulk_run_id TEXT NOT NULL,
			episode_id INTEGER NOT NULL,
			derivative_id INTEGER,
			derivative_generation INTEGER,
			admission_status TEXT NOT NULL DEFAULT 'pending',
			result_reason TEXT,
			result_snapshot TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (bulk_run_id, episode_id)
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("create test schema: %v", err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertTestEpisode(t *testing.T, db *sqlx.DB, id int64, backend, metadata, cloudSource string) {
	t.Helper()
	var source any
	if cloudSource != "" {
		source = cloudSource
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, storage_backend, mcap_path, checksum, file_size_bytes,
			metadata, cloud_publish_source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, "episode-test", backend, "raw/source.mcap", "", 100, metadata, source); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}
