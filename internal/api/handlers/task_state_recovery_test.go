// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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

func TestRecorderConfigDoesNotAdvanceTaskWhenRPCResponseUnsuccessful(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-config-false", "pending")

	hub := services.NewRecorderHub()
	attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
		return services.RPCResponse{Success: false, Message: "device rejected config"}
	})
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/recorder/:device_id/config", handler.Config)
	body := []byte(`{"task_config":{"task_id":"task-config-false"}}`)
	req := httptest.NewRequest(http.MethodPost, "/recorder/robot-001/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertTaskStateRecoveryStatus(t, db, "task-config-false", "pending")
}

func TestRecorderBeginDoesNotAdvanceTaskWhenRPCResponseUnsuccessful(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-begin-false", "pending")

	hub := services.NewRecorderHub()
	attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
		return services.RPCResponse{Success: false, Message: "device rejected begin"}
	})
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/recorder/:device_id/begin", handler.Begin)
	body := []byte(`{"task_id":"task-begin-false"}`)
	req := httptest.NewRequest(http.MethodPost, "/recorder/robot-001/begin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertTaskStateRecoveryStatus(t, db, "task-begin-false", "pending")
}

func TestRecorderIgnoresStateChangingMessagesFromReplacedConnection(t *testing.T) {
	tests := []struct {
		name string
		msg  map[string]interface{}
	}{
		{
			name: "state_update",
			msg: map[string]interface{}{
				"type": "state_update",
				"data": map[string]interface{}{
					"current": "ready",
					"task_id": "task-replaced",
				},
			},
		},
		{
			name: "config_applied",
			msg: map[string]interface{}{
				"type": "config_applied",
				"data": map[string]interface{}{
					"task_id": "task-replaced",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTaskStateRecoveryDB(t)
			defer db.Close()
			seedTaskStateRecoveryTask(t, db, "task-replaced", "pending")

			hub := services.NewRecorderHub()
			handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
			oldConn := hub.NewRecorderConn(&websocket.Conn{}, "robot-001", "127.0.0.1")
			if !hub.Connect("robot-001", oldConn) {
				t.Fatalf("initial connect failed")
			}
			newConn := hub.NewRecorderConn(&websocket.Conn{}, "robot-001", "127.0.0.1")
			hub.ConnectReplacingExisting("robot-001", newConn)

			handler.handleMessage("robot-001", oldConn, tt.msg)

			assertTaskStateRecoveryStatus(t, db, "task-replaced", "pending")
		})
	}
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

func attachRecorderRPCResponder(t *testing.T, hub *services.RecorderHub, deviceID string, respond func(services.RPCRequest) services.RPCResponse) {
	t.Helper()
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	rc := hub.NewRecorderConn(serverConn, deviceID, "127.0.0.1")
	if !hub.Connect(deviceID, rc) {
		t.Fatalf("connect recorder: initial connect failed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			var req services.RPCRequest
			if err := wsjson.Read(ctx, clientConn, &req); err != nil {
				return
			}
			resp := respond(req)
			if resp.Type == "" {
				resp.Type = "rpc_response"
			}
			if resp.RequestID == "" {
				resp.RequestID = req.RequestID
			}
			hub.HandleRPCResponse(deviceID, &resp)
		}
	}()
}

func newRecorderHandlerTestWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	serverConnC := make(chan *websocket.Conn, 1)
	errC := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			errC <- err
			return
		}
		serverConnC <- conn
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	clientConn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	select {
	case serverConn := <-serverConnC:
		t.Cleanup(func() {
			_ = clientConn.CloseNow()
			_ = serverConn.CloseNow()
		})
		return serverConn, clientConn
	case err := <-errC:
		t.Fatalf("accept websocket: %v", err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for websocket accept")
	}
	return nil, nil
}
