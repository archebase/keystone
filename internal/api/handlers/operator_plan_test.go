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
		item.DCProjectName != "Project A" || item.DCProjectDescription != "Kitchen collection project" || item.DCTaskID != 41 || item.DCTaskName != "Task A" || item.DCTaskDescription != "Stack each item in order" ||
		item.DCDeviceID != 456 || item.DCDeviceName != "Phone A" ||
		item.CurCount != 4 || item.TargetCount != 10 || item.CurDuration != 190 || item.TargetDuration != 3600 ||
		item.CloudCurCount != 2 || item.LocalCurCount != 2 || item.CloudCurDuration != 120 || item.LocalCurDuration != 70 ||
		item.LocalPendingCount != 1 || item.LocalPendingDuration != 40 ||
		item.LocalApprovedCount != 1 || item.LocalApprovedDuration != 30 ||
		item.LocalFailedCount != 2 || item.LocalFailedDuration != 30 ||
		item.CommittedCount != 5 || item.RemainingCount != 5 {
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

func TestRefreshOperatorPlansDoesNotCountCompletedTaskAfterSyncedEpisodeDeletion(t *testing.T) {
	db := newTestOperatorPlanDB(t)
	defer db.Close()
	seedOperatorPlanFixture(t, db)

	if _, err := db.Exec(`DELETE FROM tasks WHERE id <> 1`); err != nil {
		t.Fatalf("remove unrelated tasks: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM episodes`); err != nil {
		t.Fatalf("delete synced episode: %v", err)
	}

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
	if len(response.Items) != 1 {
		t.Fatalf("items=%d want=1 response=%#v", len(response.Items), response)
	}
	item := response.Items[0]
	if item.CloudCurCount != 2 || item.LocalCurCount != 0 || item.CurCount != 2 {
		t.Fatalf("progress cloud=%d local=%d total=%d want=2/0/2", item.CloudCurCount, item.LocalCurCount, item.CurCount)
	}
	if item.CloudCurDuration != 120 || item.LocalCurDuration != 0 || item.CurDuration != 120 {
		t.Fatalf("duration cloud=%d local=%d total=%d want=120/0/120", item.CloudCurDuration, item.LocalCurDuration, item.CurDuration)
	}
}

func TestRefreshOperatorPlansExcludesFailedLocalEpisodes(t *testing.T) {
	db := newTestOperatorPlanDB(t)
	defer db.Close()
	seedOperatorPlanFixture(t, db)

	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'failed' WHERE id = 2`); err != nil {
		t.Fatalf("mark episode QA failed: %v", err)
	}

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
	if len(response.Items) != 1 {
		t.Fatalf("items=%d want=1 response=%#v", len(response.Items), response)
	}
	item := response.Items[0]
	if item.LocalPendingCount != 0 || item.LocalApprovedCount != 1 || item.LocalFailedCount != 3 {
		t.Fatalf("unexpected local QA buckets: %#v", item)
	}
	if item.LocalCurCount != 1 || item.CurCount != 3 {
		t.Fatalf("progress local=%d total=%d want=1/3", item.LocalCurCount, item.CurCount)
	}
	if item.LocalCurDuration != 30 || item.CurDuration != 150 || item.LocalFailedDuration != 70 {
		t.Fatalf("duration local=%d total=%d failed=%d want=30/150/70", item.LocalCurDuration, item.CurDuration, item.LocalFailedDuration)
	}
}

func TestRefreshOperatorPlansLocalDurationMatchesDisplayedBuckets(t *testing.T) {
	db := newTestOperatorPlanDB(t)
	defer db.Close()
	seedOperatorPlanFixture(t, db)

	if _, err := db.Exec(`UPDATE episodes SET duration_sec = 40.6 WHERE id = 2`); err != nil {
		t.Fatalf("update pending episode duration: %v", err)
	}
	if _, err := db.Exec(`UPDATE episodes SET duration_sec = 30.6 WHERE id = 3`); err != nil {
		t.Fatalf("update approved episode duration: %v", err)
	}

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
	if len(response.Items) != 1 {
		t.Fatalf("items=%d want=1 response=%#v", len(response.Items), response)
	}
	item := response.Items[0]
	if item.LocalPendingDuration != 41 || item.LocalApprovedDuration != 31 ||
		item.LocalCurDuration != 72 || item.CurDuration != 192 {
		t.Fatalf("duration pending=%d approved=%d local=%d total=%d want=41/31/72/192",
			item.LocalPendingDuration, item.LocalApprovedDuration, item.LocalCurDuration, item.CurDuration)
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
			dc_project_id INTEGER, dc_project_name TEXT, dc_project_description TEXT, dc_task_id INTEGER, dc_task_name TEXT, dc_task_description TEXT,
			dc_device_id INTEGER, dc_device_name TEXT, dc_type TEXT, target_count INTEGER, cur_count INTEGER,
			target_duration INTEGER, cur_duration INTEGER,
			last_synced_at TIMESTAMP, deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY, dc_plan_id INTEGER, status TEXT, deleted_at TIMESTAMP
		);
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY, task_id INTEGER, dc_plan_id INTEGER, duration_sec REAL,
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			qa_status TEXT NOT NULL DEFAULT 'pending_qa', deleted_at TIMESTAMP
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
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_project_description, dc_task_id, dc_task_name, dc_task_description, dc_device_id, dc_device_name, dc_type, target_count, cur_count, target_duration, cur_duration, last_synced_at) VALUES (1001, 123, 'Plan A', 'collector-a', 31, 'Project A', 'Kitchen collection project', 41, 'Task A', 'Stack each item in order', 456, 'Phone A', 'ego', 10, 2, 3600, 120, '2026-07-21 01:00:00')`,
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, target_count, cur_count, target_duration, cur_duration, last_synced_at) VALUES (1002, 123, 'Other operator', 'collector-b', 32, 'Project B', 42, 'Task B', 456, 'Phone A', 'ego', 5, 0, 3600, 0, '2026-07-21 01:00:00')`,
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_project_id, dc_project_name, dc_task_id, dc_task_name, dc_device_id, dc_device_name, dc_type, target_count, cur_count, target_duration, cur_duration, last_synced_at) VALUES (1003, 123, 'Other robot', 'collector-a', 33, 'Project C', 43, 'Task C', 999, 'Phone C', 'ego', 5, 0, 3600, 0, '2026-07-21 01:00:00')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (1, 1001, 'completed')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (2, 1001, 'completed')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (3, 1001, 'uploading')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (4, 1001, 'pending')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (5, 1001, 'completed')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (6, 1001, 'completed')`,
		`INSERT INTO tasks (id, dc_plan_id, status) VALUES (7, 1001, 'completed')`,
		`INSERT INTO episodes (id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status) VALUES (1, 1, 1001, 60, TRUE, 'approved')`,
		`INSERT INTO episodes (id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status) VALUES (2, 2, 1001, 40, FALSE, 'pending_qa')`,
		`INSERT INTO episodes (id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status) VALUES (3, 5, 1001, 30, FALSE, 'approved')`,
		`INSERT INTO episodes (id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status) VALUES (4, 6, 1001, 20, FALSE, 'failed')`,
		`INSERT INTO episodes (id, task_id, dc_plan_id, duration_sec, cloud_synced, qa_status) VALUES (5, 7, 1001, 10, FALSE, 'manual_review_failed')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}
