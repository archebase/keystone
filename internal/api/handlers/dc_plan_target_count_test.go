// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type targetCountTestClient struct {
	called      bool
	workspaceID int64
	planID      int64
	targetCount int64
}

func (c *targetCountTestClient) PatchDCPlanTargetCount(
	_ context.Context,
	workspaceID int64,
	planID int64,
	targetCount int64,
) (bool, error) {
	c.called = true
	c.workspaceID = workspaceID
	c.planID = planID
	c.targetCount = targetCount
	return true, nil
}

func TestUpdateTargetCountRejectsCountBelowUploadedEpisodes(t *testing.T) {
	db := newTargetCountTestDB(t)
	defer db.Close()
	seedTargetCountTestData(t, db)
	if _, err := db.Exec(`
		INSERT INTO episodes (id, dc_plan_id, deleted_at) VALUES
			(1, 1001, NULL),
			(2, 1001, NULL),
			(3, 1001, NULL),
			(4, 1001, '2026-09-10 00:00:00')
	`); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
	client := &targetCountTestClient{}
	router := newTargetCountTestRouter(db, client)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/operator/dc-plans/1001/target-count",
		strings.NewReader(`{"target_count":2}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if client.called {
		t.Fatal("Hilbert client was called for a target below uploaded episode count")
	}
}

func TestUpdateTargetCountUpdatesHilbertAndLocalProjection(t *testing.T) {
	db := newTargetCountTestDB(t)
	defer db.Close()
	seedTargetCountTestData(t, db)
	if _, err := db.Exec(`INSERT INTO episodes (id, dc_plan_id, deleted_at) VALUES (1, 1001, NULL)`); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	client := &targetCountTestClient{}
	router := newTargetCountTestRouter(db, client)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/operator/dc-plans/1001/target-count",
		strings.NewReader(`{"target_count":5}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if !client.called || client.workspaceID != 123 || client.planID != 1001 || client.targetCount != 5 {
		t.Fatalf("Hilbert call = called:%v workspace:%d plan:%d target:%d", client.called, client.workspaceID, client.planID, client.targetCount)
	}
	var targetCount int64
	if err := db.Get(&targetCount, `SELECT target_count FROM dc_plan WHERE id = 1001`); err != nil {
		t.Fatalf("query target count: %v", err)
	}
	if targetCount != 5 {
		t.Fatalf("target_count=%d want=5", targetCount)
	}
}

func TestUpdateTargetCountCancelsPendingTasksAboveNewTarget(t *testing.T) {
	db := newTargetCountTestDB(t)
	defer db.Close()
	seedTargetCountTestData(t, db)
	if _, err := db.Exec(`
		INSERT INTO tasks (id, task_id, dc_plan_id, status) VALUES
			(1, 'task-1', 1001, 'pending'),
			(2, 'task-2', 1001, 'pending'),
			(3, 'task-3', 1001, 'pending')
	`); err != nil {
		t.Fatalf("seed pending tasks: %v", err)
	}
	client := &targetCountTestClient{}
	router := newTargetCountTestRouter(db, client)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/operator/dc-plans/1001/target-count",
		strings.NewReader(`{"target_count":1}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"tasks_cancelled":2`) {
		t.Fatalf("response=%s want tasks_cancelled=2", response.Body.String())
	}
	var pendingCount, cancelledCount int
	if err := db.Get(&pendingCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = 1001 AND status = 'pending' AND deleted_at IS NULL`); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if err := db.Get(&cancelledCount, `SELECT COUNT(*) FROM tasks WHERE dc_plan_id = 1001 AND status = 'cancelled'`); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if pendingCount != 1 || cancelledCount != 2 {
		t.Fatalf("pending=%d cancelled=%d want=1/2", pendingCount, cancelledCount)
	}
}

func newTargetCountTestRouter(db *sqlx.DB, client dcPlanTargetCountClient) *gin.Engine {
	testingClaims := auth.NewCollectorWorkstationClaims(7, "alice", 11, 9, 123)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewDCPlanHandler(db, nil, client)
	router.POST("/api/v1/operator/dc-plans/:plan_id/target-count", func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, testingClaims)
		handler.UpdateTargetCount(c)
	})
	return router
}

func newTargetCountTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			robot_id INTEGER NOT NULL,
			data_collector_id INTEGER NOT NULL,
			is_current BOOLEAN NOT NULL,
			deleted_at TIMESTAMP
		);
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			operator TEXT NOT NULL,
			status TEXT,
			dc_project_description TEXT,
			dc_task_description TEXT,
			dc_type TEXT NOT NULL DEFAULT 'ego',
			dc_device_id INTEGER,
			target_count INTEGER NOT NULL,
			cur_count INTEGER NOT NULL DEFAULT 0,
			target_duration INTEGER NOT NULL DEFAULT 0,
			local_updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			dc_plan_id INTEGER,
			status TEXT,
			updated_at TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			task_id INTEGER DEFAULT 0,
			dc_plan_id INTEGER,
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			qa_status TEXT NOT NULL DEFAULT 'pending_qa',
			deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func seedTargetCountTestData(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id) VALUES (9, '15');
		INSERT INTO workstations (id, workspace_id, robot_id, data_collector_id, is_current)
		VALUES (11, 123, 9, 7, TRUE);
		INSERT INTO dc_plan (id, workspace_id, operator, status, dc_device_id, target_count)
		VALUES (1001, 123, 'alice', 'collecting', 15, 20);
	`); err != nil {
		t.Fatalf("seed target count data: %v", err)
	}
}
