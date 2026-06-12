// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestSyncHandlerRegisterRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	api := router.Group("/api/v1")
	handler := NewSyncHandler(nil, nil)

	handler.RegisterRoutes(api)
}

func TestGetSyncConfigIncludesAutoScanEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	worker := services.NewSyncWorker(nil, nil, nil, "", services.SyncWorkerConfig{
		MaxRetries:      5,
		AutoScanEnabled: true,
	}, nil)
	router := gin.New()
	handler := NewSyncHandler(nil, worker)
	handler.RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		WorkerRunning   bool `json:"worker_running"`
		AutoScanEnabled bool `json:"auto_scan_enabled"`
		MaxRetries      int  `json:"max_retries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.WorkerRunning {
		t.Fatal("worker_running = true, want false for not-started test worker")
	}
	if !got.AutoScanEnabled {
		t.Fatal("auto_scan_enabled = false, want true")
	}
	if got.MaxRetries != 5 {
		t.Fatalf("max_retries = %d, want 5", got.MaxRetries)
	}
}

func TestListEpisodeSyncSummariesGroupsByEpisode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)

	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	_, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, deleted_at)
		VALUES (1, 'episode-a', NULL), (2, 'episode-b', NULL)
	`)
	if err != nil {
		t.Fatalf("insert episodes: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO sync_logs
			(id, episode_id, source_path, status, attempt_count, next_retry_at, started_at, completed_at, error_message)
		VALUES
			(1, 1, 'local/a.mcap', 'failed', 1, ?, ?, ?, 'first failure'),
			(2, 1, 'local/a.mcap', 'failed', 2, ?, ?, ?, 'latest failure'),
			(3, 2, 'local/b.mcap', 'completed', 1, NULL, ?, ?, NULL)
	`, now.Add(5*time.Minute), now, now.Add(time.Second), now.Add(15*time.Minute), now.Add(time.Minute), now.Add(time.Minute+time.Second), now.Add(2*time.Minute), now.Add(2*time.Minute+time.Second))
	if err != nil {
		t.Fatalf("insert sync logs: %v", err)
	}

	router := gin.New()
	handler := NewSyncHandler(db, nil)
	handler.RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/episodes/summary?status=failed", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got SyncEpisodeSummaryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Total != 1 || len(got.Items) != 1 {
		t.Fatalf("got total=%d len=%d, want one failed episode", got.Total, len(got.Items))
	}

	item := got.Items[0]
	if item.EpisodeID != 1 || item.Status != "failed" {
		t.Fatalf("got episode_id=%d status=%q, want episode 1 failed", item.EpisodeID, item.Status)
	}
	if item.TotalAttemptCount != 3 {
		t.Fatalf("total_attempt_count = %d, want 3", item.TotalAttemptCount)
	}
	if item.LatestAttemptCount != 2 {
		t.Fatalf("latest_attempt_count = %d, want 2", item.LatestAttemptCount)
	}
	if item.SyncLogCount != 2 {
		t.Fatalf("sync_log_count = %d, want 2", item.SyncLogCount)
	}
	if item.EpisodePublicID == nil || *item.EpisodePublicID != "episode-a" {
		t.Fatalf("episode_public_id = %v, want episode-a", item.EpisodePublicID)
	}
}

func TestGetSyncStatusReturnsNotStartedWhenEpisodeHasNoSyncLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)

	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, deleted_at)
		VALUES (4181, 'episode-no-sync', NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	router := gin.New()
	handler := NewSyncHandler(db, nil)
	handler.RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/episodes/4181/status", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got SyncJobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != 0 {
		t.Fatalf("id = %d, want 0 for virtual status", got.ID)
	}
	if got.EpisodeID != 4181 {
		t.Fatalf("episode_id = %d, want 4181", got.EpisodeID)
	}
	if got.EpisodePublicID == nil || *got.EpisodePublicID != "episode-no-sync" {
		t.Fatalf("episode_public_id = %v, want episode-no-sync", got.EpisodePublicID)
	}
	if got.Status != "not_started" {
		t.Fatalf("status = %q, want not_started", got.Status)
	}
	if got.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0", got.AttemptCount)
	}
}

func TestGetSyncStatusReturnsNotFoundWhenEpisodeDoesNotExist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)

	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, deleted_at)
		VALUES (42, 'episode-deleted', ?)
	`, time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert deleted episode: %v", err)
	}

	router := gin.New()
	handler := NewSyncHandler(db, nil)
	handler.RegisterRoutes(router.Group("/api/v1"))

	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: "/api/v1/sync/episodes/404/status"},
		{name: "soft deleted", path: "/api/v1/sync/episodes/42/status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var got map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got["error"] != "episode not found" {
				t.Fatalf("error = %q, want episode not found", got["error"])
			}
		})
	}
}

func setupSyncHandlerTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY,
			episode_id INTEGER NOT NULL,
			source_factory_id TEXT,
			source_path TEXT,
			destination_path TEXT,
			status TEXT,
			bytes_transferred INTEGER,
			duration_sec INTEGER,
			error_message TEXT,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at TIMESTAMP NULL,
			started_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	return db
}
