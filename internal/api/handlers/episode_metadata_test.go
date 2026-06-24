// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	handler := NewEpisodeHandler(db, nil, "", nil)
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
}

func TestListEpisodesOmitsMetadata(t *testing.T) {
	db := openEpisodeMetadataTestDB(t)
	defer db.Close()
	seedEpisodeMetadataTestRow(t, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewEpisodeHandler(db, nil, "", nil)
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
			sop_id INTEGER,
			scene_name TEXT,
			subscene_name TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE sops (
			id INTEGER PRIMARY KEY,
			slug TEXT,
			version TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			data_collector_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			operator_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE inspections (
			episode_id INTEGER,
			inspector_id INTEGER,
			decision TEXT,
			inspected_at TIMESTAMP NULL
		)`,
		`CREATE TABLE inspectors (
			id INTEGER PRIMARY KEY,
			inspector_id TEXT
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
	metadata := `{"asset_id":"asset-1","recorder":{"writer_health":{"state":"warning","writer_stall_state":"normal","writer_stall_suspected":false,"writer_partial_failures":0,"writer_queue_overflows":0,"error":null}}}`
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, sop_id, scene_name, subscene_name, deleted_at)
		VALUES (10, 'task-public-1', NULL, 'scene', 'subscene', NULL)
	`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, task_id, workstation_id, mcap_path, sidecar_path,
			checksum, file_size_bytes, duration_sec, qa_status, qa_score,
			quality_flag, auto_approved, cloud_synced, cloud_processed,
			cloud_synced_at, created_at, labels, metadata, deleted_at
		) VALUES (
			1, 'episode-public-1', 10, NULL, 'bucket/a.mcap', 'bucket/a.json',
			'abc', 1024, 12.5, 'pending_qa', NULL,
			NULL, FALSE, FALSE, FALSE,
			NULL, '2026-06-24T00:00:00Z', '[]', ?, NULL
		)
	`, metadata); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
}
