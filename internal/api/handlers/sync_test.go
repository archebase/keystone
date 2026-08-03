// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/middleware"
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

func TestSyncWriteRoutesRequireAdminWhileReadsAllowCollectors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authConfig := config.AuthConfig{
		JWTSecret:      "sync-handler-test-secret-at-least-32-bytes",
		Issuer:         "sync-handler-test",
		JWTExpiryHours: 1,
	}
	adminToken, err := auth.GenerateToken(auth.NewAdminClaims(), &authConfig)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	collectorToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "collector-7"), &authConfig)
	if err != nil {
		t.Fatalf("generate collector token: %v", err)
	}

	router := gin.New()
	handler := NewSyncHandler(nil, nil)
	jwtMiddleware := middleware.JWTAuth(&authConfig)
	handler.RegisterReadRoutes(router.Group("/api/v1", jwtMiddleware, middleware.RequireAnyRole("admin", "data_collector")))
	handler.RegisterAdminRoutes(router.Group("/api/v1", jwtMiddleware, middleware.RequireRole("admin")))

	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		wantStatus int
	}{
		{name: "anonymous read", method: http.MethodGet, path: "/api/v1/sync/config", wantStatus: http.StatusUnauthorized},
		{name: "collector read", method: http.MethodGet, path: "/api/v1/sync/config", token: collectorToken, wantStatus: http.StatusOK},
		{name: "anonymous write", method: http.MethodPost, path: "/api/v1/sync/episodes/1", wantStatus: http.StatusUnauthorized},
		{name: "collector write", method: http.MethodPost, path: "/api/v1/sync/episodes/1", token: collectorToken, wantStatus: http.StatusForbidden},
		{name: "admin write reaches handler", method: http.MethodPost, path: "/api/v1/sync/episodes/1", token: adminToken, wantStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGetSyncConfigReturnsWorkerState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	worker := services.NewSyncWorker(nil, nil, nil, "", services.SyncWorkerConfig{
		MaxRetries: 5,
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
		WorkerRunning bool `json:"worker_running"`
		MaxRetries    int  `json:"max_retries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.WorkerRunning {
		t.Fatal("worker_running = true, want false for not-started test worker")
	}
	if got.MaxRetries != 5 {
		t.Fatalf("max_retries = %d, want 5", got.MaxRetries)
	}
	var fields map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode response fields: %v", err)
	}
	if _, exists := fields["auto_scan_enabled"]; exists {
		t.Fatal("auto_scan_enabled should be removed from the sync config response")
	}
}

func TestEnqueueSyncErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{
			name:       "source unavailable",
			err:        fmt.Errorf("%w: stereo split is still running", services.ErrSyncSourceUnavailable),
			wantStatus: "source_unavailable",
		},
		{
			name:       "source locked",
			err:        fmt.Errorf("%w: source already claimed", services.ErrCloudPublishSourceLocked),
			wantStatus: "source_locked",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			h := NewSyncHandler(nil, nil)

			h.enqueueSyncErrorResponse(c, 41, test.err)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
			}
			var got struct {
				EpisodeID int64  `json:"episode_id"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.EpisodeID != 41 || got.Status != test.wantStatus {
				t.Fatalf("response = %+v, want episode 41 %s", got, test.wantStatus)
			}
		})
	}
}

func TestTriggerEpisodeResyncRejectsAlreadySyncedEpisode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)

	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, qa_status, cloud_synced, deleted_at)
		VALUES (4181, 'episode-synced', 'approved', TRUE, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, started_at, completed_at)
		VALUES (1, 4181, 'completed', 1, ?, ?)
	`, time.Now().Add(-time.Minute), time.Now()); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	worker := services.NewSyncWorker(db, nil, nil, "", services.SyncWorkerConfig{}, nil)
	router := gin.New()
	handler := NewSyncHandler(db, worker)
	handler.RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/episodes/4181/resync", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		EpisodeID int64  `json:"episode_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.EpisodeID != 4181 || got.Status != "already_synced" {
		t.Fatalf("response = %+v, want episode 4181 already_synced", got)
	}

	var syncLogCount int
	if err := db.Get(&syncLogCount, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", 4181); err != nil {
		t.Fatalf("count sync logs: %v", err)
	}
	if syncLogCount != 1 {
		t.Fatalf("sync log count = %d, want unchanged count 1", syncLogCount)
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

func TestListSyncStatusesReturnsItemsInRequestOrderAndErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)

	syncedAt := time.Date(2026, 5, 9, 11, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, cloud_synced, cloud_processed, cloud_synced_at, deleted_at)
		VALUES
			(1, 'episode-a', FALSE, FALSE, NULL, NULL),
			(2, 'episode-b', TRUE, FALSE, ?, NULL)
	`, syncedAt); err != nil {
		t.Fatalf("insert episodes: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, started_at, completed_at, bytes_transferred)
		VALUES
			(10, 1, 'pending', 0, ?, NULL, NULL),
			(11, 2, 'completed', 1, ?, ?, 4096)
	`, syncedAt.Add(-time.Hour), syncedAt.Add(-2*time.Hour), syncedAt); err != nil {
		t.Fatalf("insert sync logs: %v", err)
	}

	router := gin.New()
	handler := NewSyncHandler(db, nil)
	handler.RegisterRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/episode-statuses?ids=2,999,1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got EpisodeSyncStatusListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(got.Items))
	}
	if got.Items[0].EpisodeID != 2 || got.Items[0].Status != "completed" {
		t.Fatalf("first item = %+v, want episode 2 completed", got.Items[0])
	}
	if !got.Items[0].CloudSynced || got.Items[0].CloudSyncedAt == nil {
		t.Fatalf("first item cloud fields = synced %t at %v, want synced with timestamp", got.Items[0].CloudSynced, got.Items[0].CloudSyncedAt)
	}
	if got.Items[1].EpisodeID != 1 || got.Items[1].Status != "pending" {
		t.Fatalf("second item = %+v, want episode 1 pending", got.Items[1])
	}
	if len(got.Errors) != 1 || got.Errors[0].EpisodeID != 999 || got.Errors[0].Error != "episode not found" {
		t.Fatalf("errors = %+v, want missing episode 999", got.Errors)
	}
}

func TestListSyncStatusesRejectsInvalidIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupSyncHandlerTestDB(t)
	router := gin.New()
	handler := NewSyncHandler(db, nil)
	handler.RegisterRoutes(router.Group("/api/v1"))

	tests := []string{
		"/api/v1/sync/episode-statuses",
		"/api/v1/sync/episode-statuses?ids=",
		"/api/v1/sync/episode-statuses?ids=1,,2",
		"/api/v1/sync/episode-statuses?ids=0",
		"/api/v1/sync/episode-statuses?ids=abc",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
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
			qa_status TEXT DEFAULT '',
			cloud_synced BOOLEAN DEFAULT FALSE,
			cloud_processed BOOLEAN DEFAULT FALSE,
			cloud_synced_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY,
			episode_id INTEGER NOT NULL,
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
