// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestTransferIgnoresUploadFailedFromReplacedConnection(t *testing.T) {
	db := newTransferTakeoverDB(t)
	defer db.Close()
	seedTransferTakeoverTask(t, db, "task-old-upload", "in_progress")

	hub := services.NewTransferHub(10)
	handler := NewTransferHandler(hub, &config.TransferConfig{}, db, nil, "", "", nil, 0)
	oldConn := hub.NewTransferConn(&websocket.Conn{}, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", oldConn) {
		t.Fatalf("initial connect failed")
	}
	newConn := hub.NewTransferConn(&websocket.Conn{}, "robot-001", "127.0.0.1")
	hub.ConnectReplacingExisting("robot-001", newConn)

	handler.handleMessage(context.Background(), oldConn, map[string]interface{}{
		"type": "upload_failed",
		"data": map[string]interface{}{
			"task_id": "task-old-upload",
			"reason":  "old connection message",
		},
	})

	assertTransferTakeoverTaskStatus(t, db, "task-old-upload", "in_progress")
}

func TestTransferProcessesUploadFailedFromCurrentConnection(t *testing.T) {
	db := newTransferTakeoverDB(t)
	defer db.Close()
	seedTransferTakeoverTask(t, db, "task-current-upload", "in_progress")

	hub := services.NewTransferHub(10)
	handler := NewTransferHandler(hub, &config.TransferConfig{}, db, nil, "", "", nil, 0)
	dc := hub.NewTransferConn(&websocket.Conn{}, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatalf("connect failed")
	}

	handler.handleMessage(context.Background(), dc, map[string]interface{}{
		"type": "upload_failed",
		"data": map[string]interface{}{
			"task_id": "task-current-upload",
			"reason":  "current connection message",
		},
	})

	assertTransferTakeoverTaskStatus(t, db, "task-current-upload", "failed")
}

func newTransferTakeoverDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		batch_id INTEGER NOT NULL DEFAULT 0,
		workstation_id INTEGER NULL,
		status TEXT NOT NULL,
		completed_at TIMESTAMP NULL,
		error_message TEXT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		deleted_at TIMESTAMP NULL
	)`); err != nil {
		t.Fatalf("create tasks schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE robots (
		id INTEGER PRIMARY KEY,
		device_id TEXT NOT NULL,
		deleted_at TIMESTAMP NULL
	)`); err != nil {
		t.Fatalf("create robots schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE workstations (
		id INTEGER PRIMARY KEY,
		robot_id INTEGER NOT NULL,
		deleted_at TIMESTAMP NULL
	)`); err != nil {
		t.Fatalf("create workstations schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO robots (id, device_id) VALUES (1, 'robot-001')`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id) VALUES (10, 1)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	return db
}

func seedTransferTakeoverTask(t *testing.T, db *sqlx.DB, taskID string, status string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO tasks (task_id, workstation_id, status, created_at, updated_at) VALUES (?, 10, ?, ?, ?)`,
		taskID,
		status,
		now,
		now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func assertTransferTakeoverTaskStatus(t *testing.T, db *sqlx.DB, taskID string, want string) {
	t.Helper()
	var got string
	if err := db.Get(&got, `SELECT status FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query task status: %v", err)
	}
	if got != want {
		t.Fatalf("task status=%q want=%q", got, want)
	}
}
