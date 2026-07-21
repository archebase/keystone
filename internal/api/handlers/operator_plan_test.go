// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type fakeOperatorPlanSyncer struct {
	err error
}

func (s *fakeOperatorPlanSyncer) Configured() bool {
	return true
}

func (s *fakeOperatorPlanSyncer) SyncWorkspace(
	context.Context,
	int64,
) (*services.DCPlanSyncResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &services.DCPlanSyncResult{LastSyncedAt: time.Now().UTC()}, nil
}

func TestRefreshOperatorPlansFiltersAssignmentAndReportsProgress(t *testing.T) {
	db := newTestOperatorPlanDB(t)
	defer db.Close()
	seedOperatorPlanFixture(t, db)

	router := newTestOperatorPlanRouter(db, &fakeOperatorPlanSyncer{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/plans/refresh", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var response OperatorPlanRefreshResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.Stale || len(response.Items) != 1 {
		t.Fatalf("unexpected response: %#v", response)
	}
	item := response.Items[0]
	if item.ID != 1001 || item.WorkspaceID != 123 || item.DCProjectID != 31 ||
		item.DCProjectName != "Project A" || item.DCTaskID != 41 || item.DCTaskName != "Task A" ||
		item.DCDeviceID != 456 || item.DCDeviceName != "Phone A" ||
		item.CommittedCount != 2 || item.RemainingCount != 3 {
		t.Fatalf("unexpected item: %#v", item)
	}
}

func TestRefreshOperatorPlansFallsBackToStaleProjection(t *testing.T) {
	db := newTestOperatorPlanDB(t)
	defer db.Close()
	seedOperatorPlanFixture(t, db)

	router := newTestOperatorPlanRouter(db, &fakeOperatorPlanSyncer{err: errors.New("hilbert down")})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/plans/refresh", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var response OperatorPlanRefreshResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !response.Stale || len(response.Items) != 1 || response.LastSyncedAt == "" {
		t.Fatalf("unexpected stale response: %#v", response)
	}
}

func newTestOperatorPlanRouter(db *sqlx.DB, syncer dcPlanWorkspaceSyncer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, auth.NewCollectorWorkstationClaims(7, "collector-a", 11, 9, 123))
		c.Next()
	})
	handler := NewDCPlanHandler(db, syncer)
	handler.RegisterReadRoutes(router.Group("/api/v1"))
	return router
}

func newTestOperatorPlanDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE robots (id INTEGER PRIMARY KEY, device_id TEXT, deleted_at TIMESTAMP);
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY, workspace_id INTEGER, name TEXT, operator TEXT,
			dc_project_id INTEGER, dc_project_name TEXT, dc_task_id INTEGER, dc_task_name TEXT,
			dc_device_id INTEGER, dc_device_name TEXT, dc_type TEXT, target_count INTEGER,
			last_synced_at TIMESTAMP, deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY, dc_plan_id INTEGER, status TEXT, deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func seedOperatorPlanFixture(t *testing.T, db *sqlx.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO robots (id, device_id) VALUES (9, '456')`,
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, target_count, last_synced_at) VALUES (1001, 123, 'Plan A', 'collector-a', 31, 'Project A', 41, 'Task A', 456, 'Phone A', 'ego', 5, '2026-07-21 01:00:00')`,
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, target_count, last_synced_at) VALUES (1002, 123, 'Other operator', 'collector-b', 32, 'Project B', 42, 'Task B', 456, 'Phone A', 'ego', 5, '2026-07-21 01:00:00')`,
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, target_count, last_synced_at) VALUES (1003, 123, 'Other robot', 'collector-a', 33, 'Project C', 43, 'Task C', 999, 'Phone C', 'ego', 5, '2026-07-21 01:00:00')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (1, 1001, 'completed')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (2, 1001, 'uploading')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (3, 1001, 'pending')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}
