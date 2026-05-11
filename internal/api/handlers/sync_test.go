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
