// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/middleware"
)

func TestCollectorCaptureStateTransitions(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			workstation_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			started_at TEXT,
			updated_at TEXT,
			deleted_at TEXT
		);
		INSERT INTO tasks (id, task_id, workstation_id, status, deleted_at) VALUES
			(101, 'task-101', 11, 'pending', NULL),
			(102, 'task-102', 12, 'pending', NULL),
			(103, 'task-103', 11, 'completed', NULL),
			(104, 'task-104', 11, 'pending', '2026-07-21');
	`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	handler := NewTaskHandler(db, nil, nil, time.Second)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, auth.NewCollectorWorkstationClaims(7, "dc01", 11, 9, 10))
		c.Next()
	})
	router.POST("/tasks/:id/capture/start", handler.StartCollectorCapture)
	router.POST("/tasks/:id/capture/finish", handler.FinishCollectorCapture)

	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/tasks/101/capture/start", nil))
	if start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	var status string
	if err := db.Get(&status, `SELECT status FROM tasks WHERE id = 101`); err != nil {
		t.Fatalf("query start status: %v", err)
	}
	if status != "in_progress" {
		t.Fatalf("status=%s want in_progress", status)
	}
	idempotentStart := httptest.NewRecorder()
	router.ServeHTTP(idempotentStart, httptest.NewRequest(http.MethodPost, "/tasks/101/capture/start", nil))
	if idempotentStart.Code != http.StatusOK {
		t.Fatalf("idempotent start status=%d body=%s", idempotentStart.Code, idempotentStart.Body.String())
	}

	finish := httptest.NewRecorder()
	router.ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/tasks/101/capture/finish", nil))
	if finish.Code != http.StatusOK {
		t.Fatalf("finish status=%d body=%s", finish.Code, finish.Body.String())
	}
	if err := db.Get(&status, `SELECT status FROM tasks WHERE id = 101`); err != nil {
		t.Fatalf("query finish status: %v", err)
	}
	if status != "uploading" {
		t.Fatalf("status=%s want uploading", status)
	}
	idempotentFinish := httptest.NewRecorder()
	router.ServeHTTP(idempotentFinish, httptest.NewRequest(http.MethodPost, "/tasks/101/capture/finish", nil))
	if idempotentFinish.Code != http.StatusOK {
		t.Fatalf("idempotent finish status=%d body=%s", idempotentFinish.Code, idempotentFinish.Body.String())
	}

	otherWorkstation := httptest.NewRecorder()
	router.ServeHTTP(otherWorkstation, httptest.NewRequest(http.MethodPost, "/tasks/102/capture/start", nil))
	if otherWorkstation.Code != http.StatusNotFound {
		t.Fatalf("other workstation status=%d body=%s", otherWorkstation.Code, otherWorkstation.Body.String())
	}

	invalidState := httptest.NewRecorder()
	router.ServeHTTP(invalidState, httptest.NewRequest(http.MethodPost, "/tasks/103/capture/start", nil))
	if invalidState.Code != http.StatusConflict {
		t.Fatalf("invalid state status=%d body=%s", invalidState.Code, invalidState.Body.String())
	}

	deleted := httptest.NewRecorder()
	router.ServeHTTP(deleted, httptest.NewRequest(http.MethodPost, "/tasks/104/capture/start", nil))
	if deleted.Code != http.StatusNotFound {
		t.Fatalf("deleted status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}
