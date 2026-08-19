// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestGetEpisodeReturnsMetadata(t *testing.T) {
	db := openEpisodeMetadataTestDB(t)
	defer db.Close()
	seedEpisodeMetadataTestRow(t, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEpisodeHandler(db, "", nil)
	router.GET("/episodes/:id", handler.GetEpisode)

	req := httptest.NewRequest(http.MethodGet, "/episodes/1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata type=%T body=%#v", body["metadata"], body)
	}
	recorder, ok := metadata["recorder"].(map[string]any)
	if !ok {
		t.Fatalf("recorder type=%T metadata=%#v", metadata["recorder"], metadata)
	}
	writerHealth, ok := recorder["writer_health"].(map[string]any)
	if !ok {
		t.Fatalf("writer_health type=%T recorder=%#v", recorder["writer_health"], recorder)
	}
	if writerHealth["state"] != "warning" {
		t.Fatalf("writer_health.state=%v want warning", writerHealth["state"])
	}
	for _, removedField := range []string{"inspector_id", "inspection_decision", "inspected_at"} {
		if _, ok := body[removedField]; ok {
			t.Fatalf("response unexpectedly contains removed field %q", removedField)
		}
	}
	recording, ok := recorder["recording"].(map[string]any)
	if !ok {
		t.Fatalf("recording type=%T recorder=%#v", recorder["recording"], recorder)
	}
	if recording["recorder_version"] != "axon_recorder 0.5.0" {
		t.Fatalf("recorder.recording.recorder_version=%v want axon_recorder 0.5.0", recording["recorder_version"])
	}
	assertEpisodePlanFields(t, body)
}

func TestGetEpisodeReturnsDefaultWorkspaceFromTask(t *testing.T) {
	db := openEpisodeMetadataTestDB(t)
	defer db.Close()
	seedEpisodeMetadataTestRow(t, db)
	if _, err := db.Exec("UPDATE tasks SET organization_id = 0 WHERE id = 10"); err != nil {
		t.Fatalf("update task workspace: %v", err)
	}
	if _, err := db.Exec("UPDATE episodes SET dc_plan_id = NULL WHERE id = 1"); err != nil {
		t.Fatalf("clear episode plan: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEpisodeHandler(db, "", nil)
	router.GET("/episodes/:id", handler.GetEpisode)

	req := httptest.NewRequest(http.MethodGet, "/episodes/1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := body["workspace_id"]; got != float64(0) {
		t.Fatalf("workspace_id=%v want 0", got)
	}
}

func TestListEpisodesOmitsMetadata(t *testing.T) {
	db := openEpisodeMetadataTestDB(t)
	defer db.Close()
	seedEpisodeMetadataTestRow(t, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEpisodeHandler(db, "", nil)
	router.GET("/episodes", handler.ListEpisodes)

	req := httptest.NewRequest(http.MethodGet, "/episodes", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items=%d want 1", len(body.Items))
	}
	if _, ok := body.Items[0]["metadata"]; ok {
		t.Fatalf("list item unexpectedly contains metadata: %#v", body.Items[0]["metadata"])
	}
	if got := body.Items[0]["robot_device_name"]; got != "Ego iPhone 01" {
		t.Fatalf("robot_device_name=%v want Ego iPhone 01", got)
	}
	assertEpisodePlanFields(t, body.Items[0])
}

func TestGetEpisodePresignedURLUsesTOSBucketWithoutS3(t *testing.T) {
	db := openEpisodeMetadataTestDB(t)
	defer db.Close()
	seedEpisodeMetadataTestRow(t, db)
	if _, err := db.Exec(`
		UPDATE episodes
		SET mcap_path = 'device-uploads/capture.mcap',
			metadata = '{"source":"dgwcompat","object_store_backend":"volcengine_tos","bucket":"tos-bucket","object_key":"device-uploads/capture.mcap"}'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("update episode TOS metadata: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEpisodeHandler(db, "edge-mercury", nil)
	router.GET("/episodes/:id/presign", handler.GetEpisodePresignedURL)

	req := httptest.NewRequest(http.MethodGet, "/episodes/1/presign?kind=mcap", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body presignResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	parsed, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("parse presign url: %v", err)
	}
	values := parsed.Query()
	if got := values.Get("bucket"); got != "tos-bucket" {
		t.Fatalf("bucket = %q, want tos-bucket url=%s", got, body.URL)
	}
	if got := values.Get("object"); got != "device-uploads/capture.mcap" {
		t.Fatalf("object = %q, want device-uploads/capture.mcap url=%s", got, body.URL)
	}
}

func assertEpisodePlanFields(t *testing.T, episode map[string]any) {
	t.Helper()

	wantNumbers := map[string]float64{
		"dc_plan_id":       1001,
		"local_dc_plan_id": 2001,
		"workspace_id":     123,
	}
	for field, want := range wantNumbers {
		if got := episode[field]; got != want {
			t.Fatalf("%s=%v want %v episode=%#v", field, got, want, episode)
		}
	}
	if got := episode["dc_plan_name"]; got != "Ego Plan A" {
		t.Fatalf("dc_plan_name=%v want Ego Plan A", got)
	}
	if got := episode["dc_type"]; got != "ego" {
		t.Fatalf("dc_type=%v want ego", got)
	}
}

func openEpisodeMetadataTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			workstation_id INTEGER,
			mcap_path TEXT NOT NULL,
			sidecar_path TEXT NOT NULL,
			checksum TEXT,
			file_size_bytes INTEGER,
			duration_sec REAL,
			qa_status TEXT,
			qa_score REAL,
			quality_flag TEXT,
			auto_approved BOOLEAN DEFAULT FALSE,
			cloud_synced BOOLEAN DEFAULT FALSE,
			cloud_publish_source TEXT,
			cloud_processed BOOLEAN DEFAULT FALSE,
			cloud_synced_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL,
			labels TEXT,
			metadata TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT,
			organization_id INTEGER,
			workstation_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			data_collector_id INTEGER,
			workspace_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT,
			device_name TEXT,
			device_type TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			operator_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			dc_type TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func seedEpisodeMetadataTestRow(t *testing.T, db *sqlx.DB) {
	t.Helper()
	metadata := `{"asset_id":"asset-1","recorder":{"recording":{"recorder_version":"axon_recorder 0.5.0"},"writer_health":{"state":"warning","writer_stall_state":"normal","writer_stall_suspected":false,"writer_partial_failures":0,"writer_queue_overflows":0,"error":null}}}`
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, organization_id, workstation_id, deleted_at)
		VALUES (10, 'task-public-1', 123, NULL, NULL)
	`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO dc_plan (id, workspace_id, name, dc_type, deleted_at)
		VALUES (1001, 123, 'Ego Plan A', 'ego', NULL)
	`); err != nil {
		t.Fatalf("seed dc plan: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, device_name, device_type, deleted_at)
		VALUES (30, 'device-01', 'Ego iPhone 01', NULL, NULL)
	`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workstations (id, robot_id, data_collector_id, workspace_id, deleted_at)
		VALUES (20, 30, NULL, 123, NULL)
	`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, task_id, dc_plan_id, local_dc_plan_id, workstation_id, mcap_path, sidecar_path,
			checksum, file_size_bytes, duration_sec, qa_status, qa_score,
			quality_flag, auto_approved, cloud_synced, cloud_processed,
			cloud_synced_at, created_at, labels, metadata, deleted_at
		) VALUES (
			1, 'episode-public-1', 10, 1001, 2001, 20, 'bucket/a.mcap', 'bucket/a.json',
			'abc', 1024, 12.5, 'pending_qa', NULL,
			NULL, FALSE, FALSE, FALSE,
			NULL, '2026-06-24T00:00:00Z', '[]', ?, NULL
		)
	`, metadata); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
}
