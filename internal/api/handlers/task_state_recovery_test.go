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
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestRecorderStateUpdateReadyRestoresPendingTask(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-ready", "pending")

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

	handler.handleStateUpdate(rc, map[string]interface{}{
		"data": map[string]interface{}{
			"current": "ready",
			"task_id": "task-ready",
		},
	})

	assertTaskStateRecoveryStatus(t, db, "task-ready", "ready")
	assertTaskStateRecoveryTimestampSet(t, db, "task-ready", "ready_at")
}

func TestRecorderStateUpdateRecordingAdvancesPendingTask(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-recording", "pending")

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

	handler.handleStateUpdate(rc, map[string]interface{}{
		"data": map[string]interface{}{
			"current": "recording",
			"task_id": "task-recording",
		},
	})

	assertTaskStateRecoveryStatus(t, db, "task-recording", "in_progress")
	assertTaskStateRecoveryTimestampSet(t, db, "task-recording", "started_at")
}

func TestRecorderStateUpdatePausedAdvancesPendingTask(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-paused", "pending")

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

	handler.handleStateUpdate(rc, map[string]interface{}{
		"data": map[string]interface{}{
			"current": "paused",
			"task_id": "task-paused",
		},
	})

	assertTaskStateRecoveryStatus(t, db, "task-paused", "in_progress")
	assertTaskStateRecoveryTimestampSet(t, db, "task-paused", "started_at")
}

func TestRecorderGetStateSnapshotReadyRestoresPendingTask(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-rpc-ready", "pending")

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	state := recorderStateFromRPCData(map[string]interface{}{
		"state": "ready",
		"task_config": map[string]interface{}{
			"task_id": "task-rpc-ready",
		},
	})

	handler.applyRecorderStateSnapshot(rc, state, "get_state")

	assertTaskStateRecoveryStatus(t, db, "task-rpc-ready", "ready")
	assertTaskStateRecoveryTimestampSet(t, db, "task-rpc-ready", "ready_at")
}

func TestRecordingStartCallbackAdvancesPendingTask(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-start", "pending")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewTaskHandler(db, nil, nil, 0).RegisterCallbackRoutes(router.Group("/callbacks"))

	body, err := json.Marshal(RecordingStartCallback{
		TaskID:    "task-start",
		DeviceID:  "robot-001",
		Status:    "recording",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/callbacks/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertTaskStateRecoveryStatus(t, db, "task-start", "in_progress")
	assertTaskStateRecoveryTimestampSet(t, db, "task-start", "started_at")
}

func newTaskStateRecoveryDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		status TEXT NOT NULL,
		ready_at TIMESTAMP NULL,
		started_at TIMESTAMP NULL,
		completed_at TIMESTAMP NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		deleted_at TIMESTAMP NULL
	)`); err != nil {
		t.Fatalf("create tasks schema: %v", err)
	}
	return db
}

func seedTaskStateRecoveryTask(t *testing.T, db *sqlx.DB, taskID string, status string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO tasks (task_id, status, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		taskID,
		status,
		now,
		now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func assertTaskStateRecoveryStatus(t *testing.T, db *sqlx.DB, taskID string, want string) {
	t.Helper()
	var got string
	if err := db.Get(&got, `SELECT status FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query task status: %v", err)
	}
	if got != want {
		t.Fatalf("task status=%q want=%q", got, want)
	}
}

func assertTaskStateRecoveryTimestampSet(t *testing.T, db *sqlx.DB, taskID string, column string) {
	t.Helper()
	if column != "ready_at" && column != "started_at" {
		t.Fatalf("unexpected timestamp column %q", column)
	}
	var got int
	if err := db.Get(&got, `SELECT CASE WHEN `+column+` IS NULL THEN 0 ELSE 1 END FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query task timestamp %s: %v", column, err)
	}
	if got != 1 {
		t.Fatalf("task %s was not set", column)
	}
}
