// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestEnsureNextPlanTaskUsesAuthenticatedWorkstation(t *testing.T) {
	db := newTestNextPlanTaskDB(t)
	defer db.Close()
	seedNextPlanTaskFixture(t, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, auth.NewCollectorWorkstationClaims(7, "collector-a", 11, 9, 123))
		c.Next()
	})
	handler := NewTaskHandler(db, nil, nil, 0)
	handler.RegisterCollectorRoutes(router.Group("/api/v1"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/dc-plans/1001/tasks/next", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var result services.DCPlanTaskSupplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !result.Created || result.Task.DCPlanID != 1001 || result.Task.WorkstationID != 11 || result.Task.Status != "pending" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func newTestNextPlanTaskDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY, workspace_id INTEGER, name TEXT, operator TEXT,
			dc_project_description TEXT,
			dc_task_description TEXT,
			dc_device_id INTEGER, dc_type TEXT, target_count INTEGER, cur_count INTEGER DEFAULT 0, target_duration INTEGER,
			deleted_at TIMESTAMP
		);
		CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY, operator_id TEXT, deleted_at TIMESTAMP
		);
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY, device_id TEXT, device_type TEXT, deleted_at TIMESTAMP
		);
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY, robot_id INTEGER, data_collector_id INTEGER,
			workspace_id INTEGER, deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT, workstation_id INTEGER,
			organization_id INTEGER, dc_plan_id INTEGER, local_dc_plan_id INTEGER,
			status TEXT, assigned_at TIMESTAMP, metadata TEXT, created_at TIMESTAMP,
			updated_at TIMESTAMP, deleted_at TIMESTAMP
		);
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id TEXT, task_id INTEGER,
			dc_plan_id INTEGER, cloud_synced BOOLEAN DEFAULT FALSE,
			qa_status TEXT DEFAULT 'pending_qa', deleted_at TIMESTAMP
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func seedNextPlanTaskFixture(t *testing.T, db *sqlx.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_device_id, dc_type, target_count, target_duration) VALUES (1001, 123, 'Plan A', 'collector-a', 456, 'ego', 10, 3600)`,
		`INSERT INTO data_collectors (id, operator_id) VALUES (7, 'collector-a')`,
		`INSERT INTO robots (id, device_id, device_type) VALUES (9, '456', 'Axon')`,
		`INSERT INTO workstations (id, robot_id, data_collector_id, workspace_id) VALUES (11, 9, 7, 123)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}
