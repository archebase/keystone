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

func TestGetTaskConfigUsesConfiguredCallbackPublicBaseURL(t *testing.T) {
	db := newTestTaskConfigCallbackDB(t)
	defer db.Close()

	handler := NewTaskHandler(db, nil, nil, 0)
	handler.SetCallbackPublicBaseURL("http://192.168.1.20:9999")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/tasks/:id/config", handler.GetTaskConfig)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/1/config", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp TaskConfig
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.StartCallbackURL != "http://192.168.1.20:9999/api/v1/callbacks/start" {
		t.Fatalf("start_callback_url=%q", resp.StartCallbackURL)
	}
	if resp.FinishCallbackURL != "http://192.168.1.20:9999/api/v1/callbacks/finish" {
		t.Fatalf("finish_callback_url=%q", resp.FinishCallbackURL)
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
			order_id INTEGER,
			factory_id INTEGER,
			sop_id INTEGER,
			scene_name TEXT,
			subscene_name TEXT,
			initial_scene_layout TEXT,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE factories (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			robot_serial TEXT NOT NULL,
			robot_id INTEGER NOT NULL,
			collector_name TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE robot_types (
			id INTEGER PRIMARY KEY,
			ros_topics TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE sops (
			id INTEGER PRIMARY KEY,
			slug TEXT NOT NULL,
			skill_sequence TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE skills (
			id INTEGER PRIMARY KEY,
			slug TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	now := time.Now().UTC()
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO factories (id, name) VALUES (30, '上海一厂')`, nil},
		{`INSERT INTO orders (id, name) VALUES (10, 'order-a')`, nil},
		{`INSERT INTO robot_types (id, ros_topics) VALUES (12, '["/camera","/tf"]')`, nil},
		{`INSERT INTO robots (id, robot_type_id) VALUES (20, 12)`, nil},
		{`INSERT INTO workstations (id, name, robot_serial, robot_id, collector_name) VALUES (40, 'station-a', 'robot-001', 20, 'collector-a')`, nil},
		{`INSERT INTO sops (id, slug, skill_sequence) VALUES (50, 'sop-a', '["1"]')`, nil},
		{`INSERT INTO skills (id, slug) VALUES (1, 'pick')`, nil},
		{`INSERT INTO tasks (id, task_id, workstation_id, order_id, factory_id, sop_id, scene_name, subscene_name, initial_scene_layout, status) VALUES (1, 'task-a', 40, 10, 30, 50, 'scene-a', 'sub-a', '{}', 'pending')`, []any{now}},
	}
	for _, stmt := range seed {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed data: %v", err)
		}
	}
	return db
}
