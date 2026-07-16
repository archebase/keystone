// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDCPlanListRequiresWorkspaceID(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	router := newTestDCPlanRouter(db, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dc-plans", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestDCPlanListFiltersByWorkspaceAndFields(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	seedDCPlanHandlerPlan(t, db, 1001, 123, "Ego Kitchen", "ego", "alice", "2026-07-09")
	seedDCPlanHandlerPlan(t, db, 1002, 123, "UMI Lab", "umi", "bob", "2026-07-09")
	seedDCPlanHandlerPlan(t, db, 1003, 456, "Ego Other", "ego", "alice", "2026-07-09")
	router := newTestDCPlanRouter(db, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dc-plans?workspace_id=123&name=Ego&dc_type=ego&operator=alice&dc_date=2026-07-09&limit=20&offset=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp DCPlanListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp.Items[0].ID != 1001 || resp.Items[0].WorkspaceID != 123 || resp.Items[0].Name != "Ego Kitchen" {
		t.Fatalf("unexpected item: %#v", resp.Items[0])
	}
	if resp.Items[0].DCProjectName != "Project 1001" || resp.Items[0].DCTaskName != "Task 1001" {
		t.Fatalf("unexpected project/task names: %#v", resp.Items[0])
	}
	if resp.Items[0].DCDeviceName != "Device 1001" || resp.Items[0].OperatorDisplayName != "Collector 1001" {
		t.Fatalf("unexpected device/operator names: %#v", resp.Items[0])
	}
}

func TestDCPlanListUsesLocalEpisodeProgress(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	seedDCPlanHandlerPlanWithProgress(t, db, 1001, 123, "Ego Kitchen", "ego", "alice", "2026-07-09", 0, 0)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, dc_plan_id, status, deleted_at) VALUES
			(1, 'task-1', 1001, 'completed', NULL),
			(2, 'task-2', 1001, 'completed', NULL),
			(3, 'task-3', 1001, 'pending', NULL)
	`); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, task_id, dc_plan_id, duration_sec, deleted_at) VALUES
			(1, 'episode-1', 1, 1001, 6.4, NULL),
			(2, 'episode-2', 2, 1001, 8.6, NULL)
	`); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
	router := newTestDCPlanRouter(db, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dc-plans?workspace_id=123&limit=20&offset=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp DCPlanListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d want=1 response=%#v", len(resp.Items), resp)
	}
	if resp.Items[0].CurCount != 2 || resp.Items[0].CurDuration != 15 {
		t.Fatalf("progress=(%d,%d) want=(2,15)", resp.Items[0].CurCount, resp.Items[0].CurDuration)
	}
}

func TestDCPlanListRejectsInvalidDCDate(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	router := newTestDCPlanRouter(db, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dc-plans?workspace_id=123&dc_date=2026-99-99", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestDCPlanSyncRejectsDefaultWorkspace(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	seedDCPlanHandlerWorkspace(t, db, 0, workspaceSourceDefault)
	service := services.NewDCPlanSyncService(db, testDCPlanHandlerHilbertConfig(), &fakeDCPlanHandlerHilbertClient{configured: true})
	router := newTestDCPlanRouter(db, service)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/0/dc-plans/sync", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestDCPlanSyncReturnsResult(t *testing.T) {
	db := newTestDCPlanHandlerDB(t)
	defer db.Close()
	seedDCPlanHandlerWorkspace(t, db, 123, workspaceSourceHilbert)
	service := services.NewDCPlanSyncService(db, testDCPlanHandlerHilbertConfig(), &fakeDCPlanHandlerHilbertClient{
		pages: []*auth.HilbertDCPlanPage{
			{Records: []auth.HilbertDCPlan{testDCPlanHandlerHilbertPlan(2001, 123)}, Total: 1, PageNum: 1, PageSize: 200},
		},
	})
	router := newTestDCPlanRouter(db, service)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/123/dc-plans/sync", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp DCPlanSyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.WorkspaceID != 123 || resp.SyncedCount != 1 || resp.PageCount != 1 || resp.LastSyncedAt == "" {
		t.Fatalf("unexpected response: %#v", resp)
	}
}

type fakeDCPlanHandlerHilbertClient struct {
	configured bool
	pages      []*auth.HilbertDCPlanPage
}

func (f *fakeDCPlanHandlerHilbertClient) Configured() bool {
	return f.configured || len(f.pages) > 0
}

func (f *fakeDCPlanHandlerHilbertClient) ServiceAuthConfigured() bool {
	return f.Configured()
}

func (f *fakeDCPlanHandlerHilbertClient) QueryDCPlans(_ context.Context, _ int64, pageNum int64, _ int64) (*auth.HilbertDCPlanPage, error) {
	index := int(pageNum - 1)
	if index < 0 || index >= len(f.pages) {
		return &auth.HilbertDCPlanPage{}, nil
	}
	return f.pages[index], nil
}

func newTestDCPlanRouter(db *sqlx.DB, syncService *services.DCPlanSyncService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewDCPlanHandler(db, syncService).RegisterRoutes(router.Group("/api/v1"))
	return router
}

func testDCPlanHandlerHilbertConfig() *config.HilbertConfig {
	return &config.HilbertConfig{
		BaseURL:        "http://hilbert",
		TimeoutSeconds: 2,
		AccessKey:      "hilbert-ak",
		SecretKey:      "hilbert-sk",
	}
}

func testDCPlanHandlerHilbertPlan(id int64, workspaceID int64) auth.HilbertDCPlan {
	return auth.HilbertDCPlan{
		ID:                  id,
		WorkspaceID:         workspaceID,
		Name:                "Plan",
		DCFactoryID:         11,
		DCServiceProviderID: 12,
		Operator:            "alice",
		DCProjectID:         13,
		DCTaskID:            14,
		DCDeviceID:          15,
		DCType:              "ego",
		DCDate:              "2026-07-09",
		TargetCount:         20,
		CurCount:            0,
		TargetDuration:      3600,
		CurDuration:         0,
		CreatedBy:           "planner",
		CreatedTime:         time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC),
	}
}

func seedDCPlanHandlerWorkspace(t *testing.T, db *sqlx.DB, id int64, source string) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO workspaces (id, name, source) VALUES (?, ?, ?)", id, "Workspace", source); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

func seedDCPlanHandlerPlan(t *testing.T, db *sqlx.DB, id int64, workspaceID int64, name string, dcType string, operator string, dcDate string) {
	t.Helper()
	seedDCPlanHandlerPlanWithProgress(t, db, id, workspaceID, name, dcType, operator, dcDate, 2, 120)
}

func seedDCPlanHandlerPlanWithProgress(t *testing.T, db *sqlx.DB, id int64, workspaceID int64, name string, dcType string, operator string, dcDate string, curCount int64, curDuration int64) {
	t.Helper()
	now := time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO dc_plan (
			id, workspace_id, name, dc_factory_id, dc_service_provider_id, operator, operator_display_name,
			dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by,
			created_time, last_synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, workspaceID, name, 11, 12, operator, "Collector "+strconv.FormatInt(id, 10), 13, "Project "+strconv.FormatInt(id, 10), 14, "Task "+strconv.FormatInt(id, 10), 15, "Device "+strconv.FormatInt(id, 10), dcType, dcDate, 20, curCount, 3600, curDuration, "planner", now, now); err != nil {
		t.Fatalf("seed dc_plan: %v", err)
	}
}

func newTestDCPlanHandlerDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE workspaces (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			source TEXT NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			dc_factory_id INTEGER NOT NULL,
			dc_service_provider_id INTEGER NOT NULL,
			operator TEXT NOT NULL,
			operator_display_name TEXT,
			dc_project_id INTEGER NOT NULL,
			dc_project_name TEXT,
			dc_task_id INTEGER NOT NULL,
			dc_task_name TEXT,
			dc_device_id INTEGER NOT NULL,
			dc_device_name TEXT,
			dc_type TEXT NOT NULL,
			dc_date TEXT NOT NULL,
			target_count INTEGER NOT NULL,
			cur_count INTEGER NOT NULL DEFAULT 0,
			target_duration INTEGER NOT NULL,
			cur_duration INTEGER NOT NULL DEFAULT 0,
			created_by TEXT NOT NULL,
			created_time TIMESTAMP NOT NULL,
			updated_by TEXT,
			updated_time TIMESTAMP,
			raw_payload TEXT,
			last_synced_at TIMESTAMP,
			sync_error TEXT,
			local_created_at TIMESTAMP,
			local_updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			dc_plan_id INTEGER,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			dc_plan_id INTEGER,
			duration_sec REAL,
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}
