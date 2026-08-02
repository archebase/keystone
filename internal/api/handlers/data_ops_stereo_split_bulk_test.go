// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/services/stereosplit"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestPreviewBulkStereoSplitUsesStableEligibilityReasons(t *testing.T) {
	db := setupDataOpsStereoSplitBulkTestDB(t)
	insertStereoSplitBulkEpisode(t, db, 1, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/1.mcap"}`, "")
	insertStereoSplitBulkEpisode(t, db, 2, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/2.mcap"}`, "")
	insertStereoSplitBulkEpisode(t, db, 3, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/3.mcap"}`, stereosplit.CloudSourceOriginal)
	insertStereoSplitBulkEpisode(t, db, 4, "minio", `{}`, "")
	insertStereoSplitBulkEpisode(t, db, 5, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/5.mcap"}`, "")
	insertStereoSplitBulkEpisode(t, db, 6, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/6.mcap"}`, "")
	insertStereoSplitBulkEpisode(t, db, 7, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/7.mcap"}`, "")
	insertStereoSplitBulkDerivative(t, db, 2, stereosplit.ProcessingSucceeded, stereosplit.DeleteCompleted)
	insertStereoSplitBulkDerivative(t, db, 5, stereosplit.ProcessingRunning, stereosplit.DeleteNotRequired)
	insertStereoSplitBulkDerivative(t, db, 6, stereosplit.ProcessingFailed, stereosplit.DeletePending)
	insertStereoSplitBulkDerivative(t, db, 7, stereosplit.ProcessingFailed, stereosplit.DeleteCompleted)

	manager := &fakeDataOpsStereoSplitManager{
		imageConfig: stereosplit.ImageConfig{ID: 1, ImageRef: testHandlerImageDigest},
	}
	handler := NewDataOpsHandler(db)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/api/v1/data-ops"))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-stereo-split/preview", bytes.NewBufferString(`{"filters":{},"selection":{"mode":"all_matching"}}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var preview DataOpsBulkEpisodePreviewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Action != stereosplit.Kind || preview.MatchedCount != 7 || preview.EligibleCount != 2 || preview.SkippedCount != 5 {
		t.Fatalf("preview = %+v", preview)
	}
	wantReasons := map[string]int{
		stereosplit.BulkReasonAlreadyDerived:     1,
		stereosplit.BulkReasonCloudSourceLocked:  1,
		stereosplit.BulkReasonSourceUnavailable:  1,
		stereosplit.BulkReasonProcessingActive:   1,
		stereosplit.BulkReasonOrbitDeletePending: 1,
	}
	for _, item := range preview.SkippedBreakdown {
		if wantReasons[item.Reason] != item.Count {
			t.Fatalf("unexpected skipped breakdown item %+v in %+v", item, preview.SkippedBreakdown)
		}
		delete(wantReasons, item.Reason)
	}
	if len(wantReasons) != 0 {
		t.Fatalf("missing skipped reasons: %+v", wantReasons)
	}
}

func TestBulkStereoSplitMaterializesItemsAndFreezesCounts(t *testing.T) {
	db := setupDataOpsStereoSplitBulkTestDB(t)
	insertStereoSplitBulkEpisode(t, db, 11, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/11.mcap"}`, "")
	insertStereoSplitBulkEpisode(t, db, 12, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/12.mcap"}`, "")
	manager := &fakeDataOpsStereoSplitManager{
		db:          db,
		imageConfig: stereosplit.ImageConfig{ID: 1, ImageRef: testHandlerImageDigest},
		bulkAdmissions: map[int64]stereosplit.BulkAdmission{
			11: {
				EpisodeID:            11,
				DerivativeID:         101,
				DerivativeGeneration: 1,
				AdmissionStatus:      stereosplit.BulkAdmissionAdmitted,
				Reason:               stereosplit.BulkReasonEligible,
			},
			12: {
				EpisodeID:       12,
				AdmissionStatus: stereosplit.BulkAdmissionSkipped,
				Reason:          stereosplit.BulkReasonAlreadyDerived,
			},
		},
	}
	handler := NewDataOpsHandler(db)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/api/v1/data-ops"))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-stereo-split", bytes.NewBufferString(`{
		"confirm":true,
		"filters":{},
		"selection":{"mode":"explicit","episode_ids":[11,12]}
	}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("execute status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var accepted DataOpsBulkEpisodeActionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accepted run: %v", err)
	}
	completed := waitForBulkRunStatus(t, router, accepted.Run.RunID, dataOpsBulkRunStatusCompleted)
	waitForStereoSplitBulkRunner(t, handler, accepted.Run.RunID)
	if completed.Action != stereosplit.Kind || completed.PassedCount != 1 || completed.SkippedCount != 1 || completed.ProcessedCount != 2 {
		t.Fatalf("completed run = %+v", completed)
	}
	if completed.FinalCounts == nil || completed.FinalCounts["materialized_items"] != 2 || completed.FinalCounts["admitted"] != 1 {
		t.Fatalf("completed final counts = %+v", completed.FinalCounts)
	}

	itemsRecorder := httptest.NewRecorder()
	router.ServeHTTP(itemsRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/"+accepted.Run.RunID+"/items", nil))
	if itemsRecorder.Code != http.StatusOK || !bytes.Contains(itemsRecorder.Body.Bytes(), []byte(`"result_reason":"eligible"`)) {
		t.Fatalf("items status = %d, body = %s", itemsRecorder.Code, itemsRecorder.Body.String())
	}
}

func TestCancelStereoSplitBulkRunCancelsEveryAdmittedGeneration(t *testing.T) {
	db := setupDataOpsStereoSplitBulkTestDB(t)
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO bulk_runs (
			run_id, action, status, total_count, request, preview_counts,
			snapshot_max_episode_id, materialize_cursor, created_at, updated_at
		) VALUES ('stereo_cancel_1', ?, 'cancel_requested', 2, '{}', '{"matched":2}', 62, 62, ?, ?)
	`, stereosplit.Kind, now, now); err != nil {
		t.Fatalf("insert bulk run: %v", err)
	}
	for index, episodeID := range []int64{61, 62} {
		if _, err := db.Exec(`
			INSERT INTO bulk_run_items (
				bulk_run_id, episode_id, derivative_id, derivative_generation,
				admission_status, result_reason, created_at, updated_at
			) VALUES ('stereo_cancel_1', ?, ?, 1, ?, ?, ?, ?)
		`, episodeID, 700+index, stereosplit.BulkAdmissionAdmitted,
			stereosplit.BulkReasonEligible, now, now); err != nil {
			t.Fatalf("insert bulk item: %v", err)
		}
	}
	manager := &fakeDataOpsStereoSplitManager{
		db:          db,
		imageConfig: stereosplit.ImageConfig{ID: 1, ImageRef: testHandlerImageDigest},
	}
	handler := NewDataOpsHandler(db)
	handler.SetStereoSplitManager(manager)

	if err := handler.cancelStereoSplitBulkRun(context.Background(), "stereo_cancel_1"); err != nil {
		t.Fatalf("cancelStereoSplitBulkRun() error = %v", err)
	}
	manager.mu.Lock()
	canceledEpisodes := append([]int64(nil), manager.canceledEpisodes...)
	manager.mu.Unlock()
	if len(canceledEpisodes) != 2 || canceledEpisodes[0] != 61 || canceledEpisodes[1] != 62 {
		t.Fatalf("canceled episodes = %v, want [61 62]", canceledEpisodes)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var row struct {
			Status         string         `db:"status"`
			FinalCounts    sql.NullString `db:"final_counts"`
			CountsFrozenAt sql.NullTime   `db:"counts_frozen_at"`
		}
		if err := db.Get(&row, `
			SELECT status, final_counts, counts_frozen_at
			FROM bulk_runs WHERE run_id = 'stereo_cancel_1'
		`); err != nil {
			t.Fatalf("load canceled run: %v", err)
		}
		if row.Status == dataOpsBulkRunStatusCanceled {
			if !row.FinalCounts.Valid || !row.CountsFrozenAt.Valid ||
				!bytes.Contains([]byte(row.FinalCounts.String), []byte(`"processing_canceled":2`)) {
				t.Fatalf("canceled run not frozen atomically: %+v", row)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q, want canceled", row.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForStereoSplitBulkRunner(t, handler, "stereo_cancel_1")
}

func TestCancelStereoSplitBulkRunStopsMaterializationAfterCurrentAdmission(t *testing.T) {
	db := setupDataOpsStereoSplitBulkTestDB(t)
	for _, episodeID := range []int64{71, 72, 73} {
		insertStereoSplitBulkEpisode(t, db, episodeID, "keystone_tos",
			fmt.Sprintf(`{"bucket":"source-bucket","object_key":"raw/%d.mcap"}`, episodeID), "")
	}
	manager := &fakeDataOpsStereoSplitManager{
		db:              db,
		imageConfig:     stereosplit.ImageConfig{ID: 1, ImageRef: testHandlerImageDigest},
		admitCommitted:  make(chan int64, 1),
		admitContinue:   make(chan struct{}),
		deferBulkResult: true,
		bulkAdmissions: map[int64]stereosplit.BulkAdmission{
			71: {EpisodeID: 71, DerivativeID: 801, DerivativeGeneration: 1, AdmissionStatus: stereosplit.BulkAdmissionAdmitted, Reason: stereosplit.BulkReasonEligible},
			72: {EpisodeID: 72, DerivativeID: 802, DerivativeGeneration: 1, AdmissionStatus: stereosplit.BulkAdmissionAdmitted, Reason: stereosplit.BulkReasonEligible},
			73: {EpisodeID: 73, DerivativeID: 803, DerivativeGeneration: 1, AdmissionStatus: stereosplit.BulkAdmissionAdmitted, Reason: stereosplit.BulkReasonEligible},
		},
	}
	handler := NewDataOpsHandler(db)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/api/v1/data-ops"))

	execute := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-stereo-split", bytes.NewBufferString(`{
		"confirm":true,
		"filters":{},
		"selection":{"mode":"explicit","episode_ids":[71,72,73]}
	}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(execute, request)
	if execute.Code != http.StatusAccepted {
		t.Fatalf("execute status = %d, body = %s", execute.Code, execute.Body.String())
	}
	var accepted DataOpsBulkEpisodeActionResponse
	if err := json.Unmarshal(execute.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accepted run: %v", err)
	}

	select {
	case episodeID := <-manager.admitCommitted:
		if episodeID != 71 {
			t.Fatalf("first admitted episode = %d, want 71", episodeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first materialized item")
	}
	cancel := httptest.NewRecorder()
	router.ServeHTTP(cancel, httptest.NewRequest(http.MethodPost,
		"/api/v1/data-ops/bulk-runs/"+accepted.Run.RunID+"/cancel", nil))
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancel.Code, cancel.Body.String())
	}
	close(manager.admitContinue)
	completed := waitForBulkRunStatus(t, router, accepted.Run.RunID, dataOpsBulkRunStatusCanceled)
	waitForStereoSplitBulkRunner(t, handler, accepted.Run.RunID)
	if completed.FinalCounts["materialized_items"] != 1 || completed.FinalCounts["processing_canceled"] != 1 {
		t.Fatalf("canceled final counts = %+v", completed.FinalCounts)
	}
	manager.mu.Lock()
	admittedEpisodes := append([]int64(nil), manager.admittedEpisodes...)
	canceledEpisodes := append([]int64(nil), manager.canceledEpisodes...)
	manager.mu.Unlock()
	if len(admittedEpisodes) != 1 || admittedEpisodes[0] != 71 {
		t.Fatalf("admitted episodes = %v, want [71]", admittedEpisodes)
	}
	if len(canceledEpisodes) != 1 || canceledEpisodes[0] != 71 {
		t.Fatalf("canceled episodes = %v, want [71]", canceledEpisodes)
	}
}

func TestResumeStereoSplitBulkRunContinuesAfterPersistedCursor(t *testing.T) {
	db := setupDataOpsStereoSplitBulkTestDB(t)
	for _, episodeID := range []int64{81, 82, 83} {
		insertStereoSplitBulkEpisode(t, db, episodeID, "keystone_tos",
			fmt.Sprintf(`{"bucket":"source-bucket","object_key":"raw/%d.mcap"}`, episodeID), "")
	}
	now := time.Now().UTC()
	request := `{"confirm":true,"filters":{},"selection":{"mode":"explicit","episode_ids":[81,82,83]}}`
	if _, err := db.Exec(`
		INSERT INTO bulk_runs (
			run_id, action, status, total_count, request, preview_counts,
			snapshot_max_episode_id, materialize_cursor, started_at, created_at, updated_at
		) VALUES ('stereo_resume_1', ?, 'running', 3, ?, '{"matched":3}', 83, 81, ?, ?, ?)
	`, stereosplit.Kind, request, now, now, now); err != nil {
		t.Fatalf("insert resumable run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO bulk_run_items (
			bulk_run_id, episode_id, admission_status, result_reason, created_at, updated_at
		) VALUES ('stereo_resume_1', 81, ?, ?, ?, ?)
	`, stereosplit.BulkAdmissionSkipped, stereosplit.BulkReasonAlreadyDerived, now, now); err != nil {
		t.Fatalf("insert materialized item: %v", err)
	}
	manager := &fakeDataOpsStereoSplitManager{
		db:          db,
		imageConfig: stereosplit.ImageConfig{ID: 1, ImageRef: testHandlerImageDigest},
		bulkAdmissions: map[int64]stereosplit.BulkAdmission{
			82: {EpisodeID: 82, DerivativeID: 902, DerivativeGeneration: 1, AdmissionStatus: stereosplit.BulkAdmissionAdmitted, Reason: stereosplit.BulkReasonEligible},
			83: {EpisodeID: 83, AdmissionStatus: stereosplit.BulkAdmissionSkipped, Reason: stereosplit.BulkReasonAlreadyDerived},
		},
	}
	handler := NewDataOpsHandler(db)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/api/v1/data-ops"))

	if err := handler.ResumeStereoSplitBulkRuns(context.Background()); err != nil {
		t.Fatalf("ResumeStereoSplitBulkRuns() error = %v", err)
	}
	completed := waitForBulkRunStatus(t, router, "stereo_resume_1", dataOpsBulkRunStatusCompleted)
	waitForStereoSplitBulkRunner(t, handler, "stereo_resume_1")
	if completed.FinalCounts["materialized_items"] != 3 || completed.FinalCounts["admitted"] != 1 ||
		completed.FinalCounts["skipped"] != 2 {
		t.Fatalf("resumed final counts = %+v", completed.FinalCounts)
	}
	manager.mu.Lock()
	admittedEpisodes := append([]int64(nil), manager.admittedEpisodes...)
	manager.mu.Unlock()
	if len(admittedEpisodes) != 2 || admittedEpisodes[0] != 82 || admittedEpisodes[1] != 83 {
		t.Fatalf("resumed admissions = %v, want [82 83]", admittedEpisodes)
	}
	var firstEpisodeItems int
	if err := db.Get(&firstEpisodeItems, `
		SELECT COUNT(*) FROM bulk_run_items
		WHERE bulk_run_id = 'stereo_resume_1' AND episode_id = 81
	`); err != nil {
		t.Fatalf("count first episode items: %v", err)
	}
	if firstEpisodeItems != 1 {
		t.Fatalf("first episode item count = %d, want 1", firstEpisodeItems)
	}
}

func waitForStereoSplitBulkRunner(t *testing.T, handler *DataOpsHandler, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		handler.stereoBulkMu.Lock()
		_, running := handler.stereoBulkRuns[runID]
		handler.stereoBulkMu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stereo split bulk runner %q did not stop", runID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func setupDataOpsStereoSplitBulkTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY, episode_id TEXT NOT NULL, task_id INTEGER NOT NULL DEFAULT 0,
			workstation_id INTEGER, qa_status TEXT, cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			storage_backend TEXT, mcap_path TEXT, metadata TEXT, cloud_publish_source TEXT,
			deleted_at TIMESTAMP NULL, created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE tasks (id INTEGER PRIMARY KEY, task_id TEXT, dc_plan_id INTEGER, organization_id INTEGER, workstation_id INTEGER, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE dc_plan (id INTEGER PRIMARY KEY, dc_project_id INTEGER, dc_project_name TEXT, dc_task_id INTEGER, dc_task_name TEXT, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE workstations (id INTEGER PRIMARY KEY, robot_id INTEGER, data_collector_id INTEGER, workspace_id INTEGER, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE robots (id INTEGER PRIMARY KEY, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE data_collectors (id INTEGER PRIMARY KEY, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE sync_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL, status TEXT NOT NULL)`,
		`CREATE TABLE episode_derivatives (
			id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL, kind TEXT NOT NULL,
			generation INTEGER NOT NULL, processing_status TEXT NOT NULL, orbit_delete_status TEXT NOT NULL,
			qa_status TEXT NOT NULL DEFAULT 'not_started'
		)`,
		`CREATE TABLE bulk_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL UNIQUE, action TEXT NOT NULL,
			status TEXT NOT NULL, total_count INTEGER NOT NULL DEFAULT 0, processed_count INTEGER NOT NULL DEFAULT 0,
			passed_count INTEGER NOT NULL DEFAULT 0, qa_failed_count INTEGER NOT NULL DEFAULT 0,
			processing_failed_count INTEGER NOT NULL DEFAULT 0, skipped_count INTEGER NOT NULL DEFAULT 0,
			canceled_count INTEGER NOT NULL DEFAULT 0, error_message TEXT, request TEXT, preview_counts TEXT,
			snapshot_max_episode_id INTEGER, materialize_cursor INTEGER, materialized_at TIMESTAMP NULL,
			final_counts TEXT, counts_frozen_at TIMESTAMP NULL, started_at TIMESTAMP NULL,
			cancel_requested_at TIMESTAMP NULL, finished_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE bulk_run_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT, bulk_run_id TEXT NOT NULL, episode_id INTEGER NOT NULL,
			derivative_id INTEGER, derivative_generation INTEGER, admission_status TEXT NOT NULL DEFAULT 'pending',
			result_reason TEXT, result_snapshot TEXT, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL,
			UNIQUE (bulk_run_id, episode_id)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func insertStereoSplitBulkEpisode(t *testing.T, db *sqlx.DB, id int64, backend, metadata, cloudSource string) {
	t.Helper()
	var source any
	if cloudSource != "" {
		source = cloudSource
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, task_id, qa_status, cloud_synced, storage_backend,
			mcap_path, metadata, cloud_publish_source, created_at
		) VALUES (?, ?, 0, 'approved', 0, ?, ?, ?, ?, ?)
	`, id, "episode", backend, "raw/source.mcap", metadata, source, time.Now().UTC()); err != nil {
		t.Fatalf("insert episode %d: %v", id, err)
	}
}

func insertStereoSplitBulkDerivative(t *testing.T, db *sqlx.DB, episodeID int64, processingStatus, deleteStatus string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, orbit_delete_status, qa_status
		) VALUES (?, ?, 1, ?, ?, ?)
	`, episodeID, stereosplit.Kind, processingStatus, deleteStatus, stereosplit.QAApproved); err != nil {
		t.Fatalf("insert derivative for episode %d: %v", episodeID, err)
	}
}

func (f *fakeDataOpsStereoSplitManager) AdmitBulk(_ context.Context, runID string, episodeID int64, _ string) (stereosplit.BulkAdmission, error) {
	admission := f.bulkAdmissions[episodeID]
	if f.db == nil {
		return admission, nil
	}
	now := time.Now().UTC()
	var derivativeID any
	var generation any
	var snapshot any
	if admission.DerivativeID > 0 {
		derivativeID = admission.DerivativeID
	}
	if admission.DerivativeGeneration > 0 {
		generation = admission.DerivativeGeneration
	}
	if admission.AdmissionStatus == stereosplit.BulkAdmissionAdmitted && !f.deferBulkResult {
		snapshot = `{"generation":1,"processing_status":"succeeded","qa_status":"approved","orbit_delete_status":"completed"}`
	}
	if _, err := f.db.Exec(`
		INSERT INTO bulk_run_items (
			bulk_run_id, episode_id, derivative_id, derivative_generation,
			admission_status, result_reason, result_snapshot, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, runID, episodeID, derivativeID, generation, admission.AdmissionStatus, admission.Reason, snapshot, now, now); err != nil {
		return stereosplit.BulkAdmission{}, err
	}
	if _, err := f.db.Exec(`UPDATE bulk_runs SET materialize_cursor = ?, updated_at = ? WHERE run_id = ?`, episodeID, now, runID); err != nil {
		return stereosplit.BulkAdmission{}, err
	}
	f.mu.Lock()
	f.admittedEpisodes = append(f.admittedEpisodes, episodeID)
	f.mu.Unlock()
	if f.admitCommitted != nil {
		f.admitCommitted <- episodeID
	}
	if f.admitContinue != nil {
		<-f.admitContinue
	}
	return admission, nil
}
