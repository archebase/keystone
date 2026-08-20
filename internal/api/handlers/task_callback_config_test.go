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

func TestGetTaskConfigUsesConfiguredCallbackPublicBaseURL(t *testing.T) {
	db := newTestTaskConfigCallbackDB(t)
	defer db.Close()

	handler := NewTaskHandler(db, nil, nil, 0, nil)
	handler.SetCallbackPublicBaseURL("http://192.168.1.20:9999")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/tasks/:id/config", handler.GetTaskConfig)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/1/config", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp["start_callback_url"] != "http://192.168.1.20:9999/api/v1/callbacks/start" {
		t.Fatalf("start_callback_url=%q", resp["start_callback_url"])
	}
	if resp["finish_callback_url"] != "http://192.168.1.20:9999/api/v1/callbacks/finish" {
		t.Fatalf("finish_callback_url=%q", resp["finish_callback_url"])
	}
	if _, ok := resp["skills"]; ok {
		t.Fatalf("task config unexpectedly contains skills: %#v", resp["skills"])
	}
	if resp["workspace_id"] != float64(123) || resp["dc_plan_id"] != float64(1001) || resp["dc_type"] != "ego" {
		t.Fatalf("unexpected plan config fields: %#v", resp)
	}
}

func TestGetTaskConfigUsesTaskPlanSnapshot(t *testing.T) {
	db := newTestTaskConfigCallbackDB(t)
	defer db.Close()
	if _, err := db.Exec(`
		UPDATE tasks
		SET metadata = '{"dc_plan_name":"Snapshot Plan","dc_project_description":"Snapshot project instructions","dc_task_description":"Snapshot task instructions","operator":"snapshot-collector","dc_type":"snapshot-type","dc_device_id":654,"target_count":8,"target_duration":90,"execution_config":{"topics":[]}}'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("update task snapshot: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE dc_plan
		SET operator = 'new-collector', dc_type = 'new-type', dc_device_id = 999,
			target_count = 20, target_duration = 180
		WHERE id = 1001
	`); err != nil {
		t.Fatalf("update live plan: %v", err)
	}

	handler := NewTaskHandler(db, nil, nil, 0, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/tasks/:id/config", handler.GetTaskConfig)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/1/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var response TaskConfig
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.OperatorName != "snapshot-collector" || response.DCType != "snapshot-type" ||
		response.DCPlanName != "Snapshot Plan" ||
		response.DCProjectDescription != "Snapshot project instructions" ||
		response.DCTaskDescription != "Snapshot task instructions" ||
		response.DCDeviceID == nil || *response.DCDeviceID != 654 ||
		response.PlanTargetCount == nil || *response.PlanTargetCount != 8 ||
		response.PlanTargetDuration == nil || *response.PlanTargetDuration != 90 {
		t.Fatalf("task config did not preserve snapshot: %#v", response)
	}
}

func TestTaskListItemUsesTaskPlanSnapshot(t *testing.T) {
	livePlanName := "Live Plan"
	item := TaskListItem{DCPlanName: &livePlanName}

	applyTaskPlanSnapshot(&item, `{
		"dc_plan_name":"Snapshot Plan",
		"dc_project_description":"Snapshot project instructions",
		"dc_task_description":"Snapshot task instructions"
	}`)

	if item.DCPlanName == nil || *item.DCPlanName != "Snapshot Plan" ||
		item.DCProjectDescription != "Snapshot project instructions" ||
		item.DCTaskDescription != "Snapshot task instructions" {
		t.Fatalf("task list item did not preserve snapshot: %#v", item)
	}
}

func newTestTaskConfigCallbackDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			workstation_id INTEGER,
			dc_plan_id INTEGER,
			organization_id INTEGER,
			metadata TEXT,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			robot_serial TEXT NOT NULL,
			robot_id INTEGER NOT NULL,
			collector_name TEXT NOT NULL,
			workspace_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER,
			name TEXT,
			operator TEXT,
			dc_type TEXT,
			dc_device_id INTEGER, status TEXT,
			target_count INTEGER,
			target_duration INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workstations (id, name, robot_serial, robot_id, collector_name) VALUES (40, 'station-a', 'robot-001', 20, 'collector-a')`, nil},
		{`INSERT INTO dc_plan (id, workspace_id, name, operator, dc_type, dc_device_id, target_count, target_duration) VALUES (1001, 123, 'Plan A', 'collector-a', 'ego', 456, 10, 60)`, nil},
		{`INSERT INTO tasks (id, task_id, workstation_id, dc_plan_id, organization_id, metadata, status) VALUES (1, 'task-a', 40, 1001, 123, '{"execution_config":{"topics":[]}}', 'pending')`, nil},
	}
	for _, stmt := range seed {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed data: %v", err)
		}
	}
	return db
}
