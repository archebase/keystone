// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/middleware"
)

func TestTaskHandlerCompleteTasksScopesToCurrentWorkstationAndPlanGroup(t *testing.T) {
	db := newTaskCompleteTestDB(t)
	defer db.Close()

	handler := NewTaskHandler(db, nil, nil, 0)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/tasks/complete", func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, auth.NewCollectorClaims(100, "collector-100"))
		handler.CompleteTasks(c)
	})

	body := bytes.NewBufferString(`{"dc_plan_id":10,"quantity":2}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/tasks/complete", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response CompleteTasksResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.DCPlanID != 10 || response.CompletedCount != 2 || len(response.Tasks) != 2 {
		t.Fatalf("unexpected response: %#v", response)
	}

	assertTaskStatusCount(t, db, "dc_plan_id = 10 AND workstation_id = 1", "completed", 2)
	assertTaskStatusCount(t, db, "dc_plan_id = 10 AND workstation_id = 2", "pending", 1)
	assertTaskStatusCount(t, db, "dc_plan_id = 11 AND workstation_id = 1", "pending", 1)
	assertTaskStatusCount(t, db, "dc_plan_id = 10 AND workstation_id = 1", "pending", 1)
}

func newTaskCompleteTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:task-complete?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`CREATE TABLE workstations (id INTEGER PRIMARY KEY, data_collector_id INTEGER, is_current BOOLEAN, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE dc_plan (id INTEGER PRIMARY KEY, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			dc_plan_id INTEGER,
			workstation_id INTEGER,
			sop_id INTEGER,
			subscene_id INTEGER,
			status TEXT NOT NULL,
			assigned_at TIMESTAMP NULL,
			started_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL,
			updated_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`INSERT INTO workstations (id, data_collector_id, is_current) VALUES (1, 100, TRUE), (2, 101, TRUE)`,
		`INSERT INTO dc_plan (id) VALUES (10), (11)`,
		`INSERT INTO tasks (id, task_id, dc_plan_id, workstation_id, sop_id, subscene_id, status) VALUES
			(1, 'task-1', 10, 1, 20, 30, 'pending'),
			(2, 'task-2', 10, 1, 20, 30, 'pending'),
			(3, 'task-other-workstation', 10, 2, 20, 30, 'pending'),
			(4, 'task-other-plan', 11, 1, 20, 30, 'pending'),
			(5, 'task-other-group', 10, 1, 20, 31, 'pending')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("execute fixture: %v\nquery=%s", err, statement)
		}
	}
	return db
}

func assertTaskStatusCount(t *testing.T, db *sqlx.DB, condition string, status string, want int) {
	t.Helper()
	var got int
	if err := db.Get(&got, "SELECT COUNT(*) FROM tasks WHERE "+condition+" AND status = ?", status); err != nil {
		t.Fatalf("count task statuses: %v", err)
	}
	if got != want {
		t.Fatalf("condition=%q status=%q count=%d want=%d", condition, status, got, want)
	}
}
