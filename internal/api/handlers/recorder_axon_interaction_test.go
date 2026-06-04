// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestRecorderStateSnapshotReconcilesAdditionalStates(t *testing.T) {
	tests := []struct {
		name       string
		initial    string
		state      services.RecorderState
		wantStatus string
		wantColumn string
	}{
		{
			name:       "get_state recording with top-level task_id advances to in_progress",
			initial:    "pending",
			state:      recorderStateFromRPCData(map[string]interface{}{"state": "recording", "task_id": "task-state"}),
			wantStatus: "in_progress",
			wantColumn: "started_at",
		},
		{
			name:       "get_state paused with nested task_config advances to in_progress",
			initial:    "ready",
			state:      recorderStateFromRPCData(map[string]interface{}{"state": "paused", "task_config": map[string]interface{}{"task_id": "task-state"}}),
			wantStatus: "in_progress",
			wantColumn: "started_at",
		},
		{
			name:       "idle with task_id does not change pending task",
			initial:    "pending",
			state:      services.RecorderState{CurrentState: "idle", TaskID: "task-state"},
			wantStatus: "pending",
		},
		{
			name:       "ready without task_id does not change pending task",
			initial:    "pending",
			state:      services.RecorderState{CurrentState: "ready"},
			wantStatus: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTaskStateRecoveryDB(t)
			defer db.Close()
			seedTaskStateRecoveryTask(t, db, "task-state", tt.initial)

			hub := services.NewRecorderHub()
			handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
			rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

			_ = handler.applyRecorderStateSnapshot(rc, tt.state, "get_state")

			assertTaskStateRecoveryStatus(t, db, "task-state", tt.wantStatus)
			if tt.wantColumn != "" {
				assertTaskStateRecoveryTimestampSet(t, db, "task-state", tt.wantColumn)
			}
		})
	}
}

func TestRecorderAuthoritativeNonIdleWithoutTaskIDKeepsUnsyncedWithoutDBChange(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-no-id", "pending")

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, db)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", rc) {
		t.Fatalf("connect recorder: initial connect failed")
	}

	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "ready"}, "state_update"); err == nil {
		t.Fatalf("expected missing task_id error for authoritative state_update")
	}

	if rc.IsStateSynced() {
		t.Fatalf("authoritative non-idle state without task_id marked connection synced")
	}
	assertTaskStateRecoveryStatus(t, db, "task-no-id", "pending")
}

func TestRecorderEmptyStateSnapshotKeepsConnectionUnsynced(t *testing.T) {
	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{}, "state_update")

	if rc.IsStateSynced() {
		t.Fatalf("empty state snapshot marked connection as synced")
	}
}

func TestRecorderListDevicesIncludesStateSynced(t *testing.T) {
	hub := services.NewRecorderHub()
	unsynced := hub.NewRecorderConn(nil, "robot-unsynced", "127.0.0.1")
	if !hub.Connect("robot-unsynced", unsynced) {
		t.Fatalf("connect unsynced recorder failed")
	}
	synced := hub.NewRecorderConn(nil, "robot-synced", "127.0.0.1")
	synced.UpdateState(services.RecorderState{CurrentState: "idle"})
	if !hub.Connect("robot-synced", synced) {
		t.Fatalf("connect synced recorder failed")
	}
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/recorder/devices", handler.ListDevices)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/devices", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var body struct {
		Devices []struct {
			DeviceID    string `json:"device_id"`
			StateSynced bool   `json:"state_synced"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list devices response: %v", err)
	}
	got := map[string]bool{}
	for _, device := range body.Devices {
		got[device.DeviceID] = device.StateSynced
	}
	if got["robot-unsynced"] {
		t.Fatalf("unsynced recorder reported state_synced=true: %#v", body.Devices)
	}
	if !got["robot-synced"] {
		t.Fatalf("synced recorder reported state_synced=false: %#v", body.Devices)
	}
}

func TestRecorderConfigRejectsAdditionalBusyStates(t *testing.T) {
	for _, state := range []string{"recording", "paused", "unknown", "maintenance"} {
		t.Run(state, func(t *testing.T) {
			db := newTaskStateRecoveryDB(t)
			defer db.Close()
			seedTaskStateRecoveryTask(t, db, "task-config-busy", "pending")

			hub := services.NewRecorderHub()
			rpcCalled := make(chan services.RPCRequest, 1)
			rc := attachRecorderRPCResponderWithConn(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
				rpcCalled <- req
				return services.RPCResponse{Success: true}
			})
			handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
			_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: state, TaskID: "current-task"}, "state_update")

			router := newRecorderInteractionRouter(handler)
			w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-config-busy"}}`)

			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
			}
			select {
			case req := <-rpcCalled:
				t.Fatalf("unexpected RPC sent while recorder state=%s: %#v", state, req)
			default:
			}
			assertTaskStateRecoveryStatus(t, db, "task-config-busy", "pending")
		})
	}
}

func TestRecorderConfigTimeoutAndDisconnectedKeepTaskPending(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-config-timeout", "pending")

		hub := services.NewRecorderHub()
		rc, requests := attachRecorderRPCObserverWithConn(t, hub, "robot-001")
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "idle"}, "state_update")

		router := newRecorderInteractionRouter(handler)
		w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-config-timeout"}}`)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusGatewayTimeout, w.Body.String())
		}
		req := receiveRecorderRPCRequest(t, requests, "config")
		if req.Action != "config" {
			t.Fatalf("action=%q want=config", req.Action)
		}
		assertTaskStateRecoveryStatus(t, db, "task-config-timeout", "pending")
	})

	t.Run("disconnected", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-config-disconnected", "pending")

		hub := services.NewRecorderHub()
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)
		w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-config-disconnected"}}`)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-config-disconnected", "pending")
	})
}

func TestRecorderBeginSuccessTimeoutAndDisconnectedTaskState(t *testing.T) {
	t.Run("success advances pending to in_progress", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-begin-success", "pending")

		hub := services.NewRecorderHub()
		attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
			return services.RPCResponse{Success: true}
		})
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/begin", `{"task_id":"task-begin-success"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-begin-success", "in_progress")
		assertTaskStateRecoveryTimestampSet(t, db, "task-begin-success", "started_at")
	})

	t.Run("timeout keeps pending", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-begin-timeout", "pending")

		hub := services.NewRecorderHub()
		_, requests := attachRecorderRPCObserverWithConn(t, hub, "robot-001")
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/begin", `{"task_id":"task-begin-timeout"}`)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusGatewayTimeout, w.Body.String())
		}
		receiveRecorderRPCRequest(t, requests, "begin")
		assertTaskStateRecoveryStatus(t, db, "task-begin-timeout", "pending")
	})

	t.Run("disconnected keeps pending", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-begin-disconnected", "pending")

		hub := services.NewRecorderHub()
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/begin", `{"task_id":"task-begin-disconnected"}`)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-begin-disconnected", "pending")
	})
}

func TestRecorderBeginSuccessAdvancesReadyAndLeavesInProgress(t *testing.T) {
	for _, tt := range []struct {
		name       string
		initial    string
		wantStatus string
	}{
		{name: "ready advances to in_progress", initial: "ready", wantStatus: "in_progress"},
		{name: "in_progress stays in_progress", initial: "in_progress", wantStatus: "in_progress"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newTaskStateRecoveryDB(t)
			defer db.Close()
			seedTaskStateRecoveryTask(t, db, "task-begin-idempotent", tt.initial)

			hub := services.NewRecorderHub()
			attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
				return services.RPCResponse{Success: true}
			})
			handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
			router := newRecorderInteractionRouter(handler)

			w := recorderInteractionPost(t, router, "/recorder/robot-001/begin", `{"task_id":"task-begin-idempotent"}`)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			assertTaskStateRecoveryStatus(t, db, "task-begin-idempotent", tt.wantStatus)
			if tt.initial == "ready" {
				assertTaskStateRecoveryTimestampSet(t, db, "task-begin-idempotent", "started_at")
			}
		})
	}
}

func TestRecorderForwardOnlyActionsDoNotMutateTaskState(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		body    string
		initial string
	}{
		{name: "finish", path: "/recorder/robot-001/finish", body: `{"task_id":"task-forward-only"}`, initial: "in_progress"},
		{name: "pause", path: "/recorder/robot-001/pause", body: `{}`, initial: "in_progress"},
		{name: "resume", path: "/recorder/robot-001/resume", body: `{}`, initial: "in_progress"},
		{name: "quit", path: "/recorder/robot-001/quit", body: `{}`, initial: "ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newTaskStateRecoveryDB(t)
			defer db.Close()
			seedTaskStateRecoveryTask(t, db, "task-forward-only", tt.initial)

			hub := services.NewRecorderHub()
			attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
				return services.RPCResponse{Success: true}
			})
			handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
			router := newRecorderInteractionRouter(handler)

			w := recorderInteractionPost(t, router, tt.path, tt.body)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			assertTaskStateRecoveryStatus(t, db, "task-forward-only", tt.initial)
		})
	}
}

func TestRecorderRPCPauseResumeWithoutTaskIDPreservesBoundTask(t *testing.T) {
	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", rc) {
		t.Fatalf("connect recorder: initial connect failed")
	}

	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "recording", TaskID: "task-live"}, "state_update")
	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "paused"}, "rpc_response:pause")

	st := rc.GetState()
	if st.CurrentState != "paused" || st.TaskID != "task-live" {
		t.Fatalf("state=(%q,%q), want paused/task-live", st.CurrentState, st.TaskID)
	}

	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "recording"}, "rpc_response:resume")
	st = rc.GetState()
	if st.CurrentState != "recording" || st.TaskID != "task-live" {
		t.Fatalf("state=(%q,%q), want recording/task-live", st.CurrentState, st.TaskID)
	}

	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "idle", TaskID: "task-live"}, "rpc_response:finish")
	st = rc.GetState()
	if st.CurrentState != "idle" || st.TaskID != "" {
		t.Fatalf("idle state=(%q,%q), want idle with empty task", st.CurrentState, st.TaskID)
	}
}

func TestRecorderAuthoritativeStateWithoutTaskIDMarksUnsynced(t *testing.T) {
	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", rc) {
		t.Fatalf("connect recorder: initial connect failed")
	}

	_ = handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "recording", TaskID: "task-live"}, "state_update")
	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "paused"}, "state_update"); err == nil {
		t.Fatalf("expected missing task_id error for authoritative state_update")
	}

	st := rc.GetState()
	if st.CurrentState != "recording" || st.TaskID != "task-live" {
		t.Fatalf("state=(%q,%q), want previous recording/task-live", st.CurrentState, st.TaskID)
	}
	if rc.IsStateSynced() {
		t.Fatalf("state should be marked unsynced after authoritative snapshot without task_id")
	}
}

func TestRecorderFinishUsesCachedTaskIDWhenBodyEmpty(t *testing.T) {
	hub := services.NewRecorderHub()
	reqC := make(chan services.RPCRequest, 1)
	rc := attachRecorderRPCResponderWithConn(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
		reqC <- req
		return services.RPCResponse{Success: true, Data: map[string]interface{}{"state": "idle"}}
	})
	rc.UpdateState(services.RecorderState{CurrentState: "paused", TaskID: "task-live"})
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, nil)
	router := newRecorderInteractionRouter(handler)

	w := recorderInteractionPost(t, router, "/recorder/robot-001/finish", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	select {
	case req := <-reqC:
		if got := strings.TrimSpace(stringValue(req.Params, "task_id")); got != "task-live" {
			t.Fatalf("finish params task_id=%q want task-live; params=%#v", got, req.Params)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for finish RPC")
	}

	st := rc.GetState()
	if st.CurrentState != "idle" || st.TaskID != "" {
		t.Fatalf("finish response state=(%q,%q), want idle with empty task", st.CurrentState, st.TaskID)
	}
}

func TestRecorderCancelAndClearTaskStateSideEffects(t *testing.T) {
	t.Run("cancel success reverts ready and in_progress", func(t *testing.T) {
		for _, initial := range []string{"ready", "in_progress"} {
			t.Run(initial, func(t *testing.T) {
				db := newTaskStateRecoveryDB(t)
				defer db.Close()
				seedTaskStateRecoveryTask(t, db, "task-cancel", initial)

				hub := services.NewRecorderHub()
				attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
					return services.RPCResponse{Success: true}
				})
				handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
				router := newRecorderInteractionRouter(handler)

				w := recorderInteractionPost(t, router, "/recorder/robot-001/cancel", `{"task_id":"task-cancel"}`)

				if w.Code != http.StatusOK {
					t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
				}
				assertTaskStateRecoveryStatus(t, db, "task-cancel", "pending")
			})
		}
	})

	t.Run("cancel failure keeps in_progress", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-cancel-false", "in_progress")

		hub := services.NewRecorderHub()
		attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
			return services.RPCResponse{Success: false, Message: "device rejected cancel"}
		})
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/cancel", `{"task_id":"task-cancel-false"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-cancel-false", "in_progress")
	})

	t.Run("clear success reverts only ready and strips task payload", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-clear-ready", "ready")
		seedTaskStateRecoveryTask(t, db, "task-clear-progress", "in_progress")

		hub := services.NewRecorderHub()
		rpcRequests := make(chan services.RPCRequest, 1)
		attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
			rpcRequests <- req
			return services.RPCResponse{Success: true}
		})
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/clear", `{"task_id":"task-clear-ready"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		req := receiveRecorderRPCRequest(t, rpcRequests, "clear")
		if len(req.Params) != 0 {
			t.Fatalf("clear RPC params=%#v want empty", req.Params)
		}
		assertTaskStateRecoveryStatus(t, db, "task-clear-ready", "pending")
		assertTaskStateRecoveryStatus(t, db, "task-clear-progress", "in_progress")
	})
}

func TestRecorderCancelTimeoutAndDisconnectedKeepTaskState(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-cancel-timeout", "in_progress")

		hub := services.NewRecorderHub()
		_, requests := attachRecorderRPCObserverWithConn(t, hub, "robot-001")
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/cancel", `{"task_id":"task-cancel-timeout"}`)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusGatewayTimeout, w.Body.String())
		}
		receiveRecorderRPCRequest(t, requests, "cancel")
		assertTaskStateRecoveryStatus(t, db, "task-cancel-timeout", "in_progress")
	})

	t.Run("disconnected", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-cancel-disconnected", "ready")

		hub := services.NewRecorderHub()
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/cancel", `{"task_id":"task-cancel-disconnected"}`)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-cancel-disconnected", "ready")
	})
}

func TestRecorderClearFailureTimeoutAndDisconnectedKeepTaskState(t *testing.T) {
	t.Run("success false", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-clear-false", "ready")

		hub := services.NewRecorderHub()
		attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
			return services.RPCResponse{Success: false, Message: "device rejected clear"}
		})
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/clear", `{"task_id":"task-clear-false"}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-clear-false", "ready")
	})

	t.Run("timeout", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-clear-timeout", "ready")

		hub := services.NewRecorderHub()
		_, requests := attachRecorderRPCObserverWithConn(t, hub, "robot-001")
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/clear", `{"task_id":"task-clear-timeout"}`)

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusGatewayTimeout, w.Body.String())
		}
		receiveRecorderRPCRequest(t, requests, "clear")
		assertTaskStateRecoveryStatus(t, db, "task-clear-timeout", "ready")
	})

	t.Run("disconnected", func(t *testing.T) {
		db := newTaskStateRecoveryDB(t)
		defer db.Close()
		seedTaskStateRecoveryTask(t, db, "task-clear-disconnected", "ready")

		hub := services.NewRecorderHub()
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
		router := newRecorderInteractionRouter(handler)

		w := recorderInteractionPost(t, router, "/recorder/robot-001/clear", `{"task_id":"task-clear-disconnected"}`)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
		}
		assertTaskStateRecoveryStatus(t, db, "task-clear-disconnected", "ready")
	})
}

func TestRecorderGetStatsContracts(t *testing.T) {
	t.Run("disconnected returns connected false", func(t *testing.T) {
		hub := services.NewRecorderHub()
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, nil)
		router := newRecorderInteractionRouter(handler)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/stats", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode stats response: %v", err)
		}
		if body["connected"] != false {
			t.Fatalf("connected=%v want=false body=%v", body["connected"], body)
		}
	})

	t.Run("axon failure returns connected true with error", func(t *testing.T) {
		hub := services.NewRecorderHub()
		attachRecorderRPCResponder(t, hub, "robot-001", func(req services.RPCRequest) services.RPCResponse {
			return services.RPCResponse{Success: false, Message: "stats unavailable", Data: map[string]interface{}{"written": float64(3)}}
		})
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, nil)
		router := newRecorderInteractionRouter(handler)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/stats", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode stats response: %v", err)
		}
		if body["connected"] != true || body["error"] != "stats unavailable" {
			t.Fatalf("unexpected stats body=%v", body)
		}
	})

	t.Run("timeout returns gateway timeout", func(t *testing.T) {
		hub := services.NewRecorderHub()
		_, requests := attachRecorderRPCObserverWithConn(t, hub, "robot-001")
		handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, nil)
		router := newRecorderInteractionRouter(handler)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/stats", nil))

		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusGatewayTimeout, w.Body.String())
		}
		receiveRecorderRPCRequest(t, requests, "get_stats")
	})
}

func TestRecorderDisconnectRevertsOnlyRunnableTasksForDevice(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionDevice(t, db, "robot-002", 2, 102)
	seedRecorderInteractionTask(t, db, 101, "task-ready", "ready", false)
	seedRecorderInteractionTask(t, db, 101, "task-progress", "in_progress", false)
	seedRecorderInteractionTask(t, db, 101, "task-pending", "pending", false)
	seedRecorderInteractionTask(t, db, 101, "task-completed", "completed", false)
	seedRecorderInteractionTask(t, db, 101, "task-deleted", "ready", true)
	seedRecorderInteractionTask(t, db, 102, "task-other-device", "ready", false)

	revertRunnableTasksOnDeviceDisconnect(db, "robot-001", nil, 0, false)

	assertRecorderInteractionTaskStatus(t, db, "task-ready", "pending")
	assertRecorderInteractionTaskStatus(t, db, "task-progress", "pending")
	assertRecorderInteractionTaskStatus(t, db, "task-pending", "pending")
	assertRecorderInteractionTaskStatus(t, db, "task-completed", "completed")
	assertRecorderInteractionTaskStatus(t, db, "task-deleted", "ready")
	assertRecorderInteractionTaskStatus(t, db, "task-other-device", "ready")
	assertRecorderInteractionTaskRuntimeCleared(t, db, "task-ready")
	assertRecorderInteractionTaskRuntimeCleared(t, db, "task-progress")
}

func TestOldRecorderDisconnectAfterReplacementDoesNotRevertTasks(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-current-ready", "ready", false)

	hub := services.NewRecorderHub()
	oldConn := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", oldConn) {
		t.Fatalf("connect old recorder failed")
	}
	newConn := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	hub.ConnectReplacingExisting("robot-001", newConn)
	if hub.Disconnect("robot-001", oldConn) {
		revertRunnableTasksOnDeviceDisconnect(db, "robot-001", nil, 0, false)
	}

	assertRecorderInteractionTaskStatus(t, db, "task-current-ready", "ready")
}

func newRecorderInteractionRouter(handler *RecorderHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/recorder/devices", handler.ListDevices)
	router.GET("/recorder/:device_id/state", handler.GetState)
	router.GET("/recorder/:device_id/stats", handler.GetStats)
	router.POST("/recorder/:device_id/config", handler.Config)
	router.POST("/recorder/:device_id/begin", handler.Begin)
	router.POST("/recorder/:device_id/finish", handler.Finish)
	router.POST("/recorder/:device_id/pause", handler.Pause)
	router.POST("/recorder/:device_id/resume", handler.Resume)
	router.POST("/recorder/:device_id/cancel", handler.Cancel)
	router.POST("/recorder/:device_id/clear", handler.Clear)
	router.POST("/recorder/:device_id/quit", handler.Quit)
	return router
}

func recorderInteractionPost(t *testing.T, router *gin.Engine, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

func attachRecorderRPCObserverWithConn(t *testing.T, hub *services.RecorderHub, deviceID string) (*services.RecorderConn, <-chan services.RPCRequest) {
	t.Helper()
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	rc := hub.NewRecorderConn(serverConn, deviceID, "127.0.0.1")
	if !hub.Connect(deviceID, rc) {
		t.Fatalf("connect recorder: initial connect failed")
	}

	requests := make(chan services.RPCRequest, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			var req services.RPCRequest
			if err := wsjson.Read(ctx, clientConn, &req); err != nil {
				return
			}
			requests <- req
		}
	}()
	return rc, requests
}

func receiveRecorderRPCRequest(t *testing.T, requests <-chan services.RPCRequest, wantAction string) services.RPCRequest {
	t.Helper()
	select {
	case req := <-requests:
		if wantAction != "" && req.Action != wantAction {
			t.Fatalf("RPC action=%q want=%q req=%#v", req.Action, wantAction, req)
		}
		return req
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for recorder RPC action %q", wantAction)
	}
	return services.RPCRequest{}
}

func newRecorderInteractionDB(t *testing.T) *sqlx.DB {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name())
	db, err := sqlx.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })
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
	if _, err := db.Exec(`CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		batch_id INTEGER NOT NULL DEFAULT 0,
		workstation_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		ready_at TIMESTAMP NULL,
		started_at TIMESTAMP NULL,
		completed_at TIMESTAMP NULL,
		error_message TEXT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL,
		deleted_at TIMESTAMP NULL
	)`); err != nil {
		t.Fatalf("create tasks schema: %v", err)
	}
	return db
}

func seedRecorderInteractionDevice(t *testing.T, db *sqlx.DB, deviceID string, robotID int64, workstationID int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO robots (id, device_id) VALUES (?, ?)`, robotID, deviceID); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id) VALUES (?, ?)`, workstationID, robotID); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
}

func seedRecorderInteractionTask(t *testing.T, db *sqlx.DB, workstationID int64, taskID string, status string, deleted bool) {
	t.Helper()
	now := time.Now().UTC()
	var deletedAt interface{}
	if deleted {
		deletedAt = now
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (task_id, batch_id, workstation_id, status, ready_at, started_at, completed_at, error_message, created_at, updated_at, deleted_at)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID,
		workstationID,
		status,
		now,
		now,
		now,
		"previous error",
		now,
		now,
		deletedAt,
	); err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
}

func assertRecorderInteractionTaskStatus(t *testing.T, db *sqlx.DB, taskID string, want string) {
	t.Helper()
	var got string
	if err := db.Get(&got, `SELECT status FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query task status: %v", err)
	}
	if got != want {
		t.Fatalf("task %s status=%q want=%q", taskID, got, want)
	}
}

func assertRecorderInteractionTaskRuntimeCleared(t *testing.T, db *sqlx.DB, taskID string) {
	t.Helper()
	var count int
	if err := db.Get(&count, `SELECT CASE WHEN ready_at IS NULL AND started_at IS NULL AND completed_at IS NULL AND error_message IS NULL THEN 1 ELSE 0 END FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query runtime columns for %s: %v", taskID, err)
	}
	if count != 1 {
		t.Fatalf("runtime columns for task %s were not cleared", taskID)
	}
}

func TestRecorderIgnoresRPCResponseFromReplacedConnection(t *testing.T) {
	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)
	oldConn := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", oldConn) {
		t.Fatalf("connect old recorder failed")
	}

	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	newConn := hub.NewRecorderConn(serverConn, "robot-001", "127.0.0.1")
	hub.ConnectReplacingExisting("robot-001", newConn)

	type rpcResult struct {
		resp *services.RPCResponse
		err  error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resultC := make(chan rpcResult, 1)
	go func() {
		resp, err := hub.SendRPC(ctx, "robot-001", "begin", nil, time.Second)
		resultC <- rpcResult{resp: resp, err: err}
	}()

	var req services.RPCRequest
	if err := wsjson.Read(ctx, clientConn, &req); err != nil {
		t.Fatalf("read current recorder request: %v", err)
	}
	handler.handleMessage("robot-001", oldConn, map[string]interface{}{
		"type":       "rpc_response",
		"request_id": req.RequestID,
		"success":    true,
	})

	select {
	case result := <-resultC:
		t.Fatalf("old connection rpc_response completed current RPC: resp=%+v err=%v", result.resp, result.err)
	case <-time.After(150 * time.Millisecond):
	}

	if !hub.HandleRPCResponse("robot-001", &services.RPCResponse{RequestID: req.RequestID, Success: false, Message: "current response"}) {
		t.Fatalf("current rpc_response was not matched")
	}
	select {
	case result := <-resultC:
		if result.err != nil {
			t.Fatalf("current response returned error: %v", result.err)
		}
		if result.resp == nil || result.resp.Success {
			t.Fatalf("current response=%+v want success=false", result.resp)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for current rpc_response")
	}
}

func TestRecorderUnmatchedRPCResponseDoesNotUnblockPendingRequest(t *testing.T) {
	hub := services.NewRecorderHub()
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	rc := hub.NewRecorderConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", rc) {
		t.Fatalf("connect recorder failed")
	}

	type rpcResult struct {
		resp *services.RPCResponse
		err  error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resultC := make(chan rpcResult, 1)
	go func() {
		resp, err := hub.SendRPC(ctx, "robot-001", "begin", nil, time.Second)
		resultC <- rpcResult{resp: resp, err: err}
	}()

	var req services.RPCRequest
	if err := wsjson.Read(ctx, clientConn, &req); err != nil {
		t.Fatalf("read recorder request: %v", err)
	}
	if hub.HandleRPCResponse("robot-001", &services.RPCResponse{RequestID: "bogus", Success: true}) {
		t.Fatalf("unmatched rpc_response returned matched=true")
	}
	select {
	case result := <-resultC:
		t.Fatalf("unmatched rpc_response completed pending RPC: resp=%+v err=%v", result.resp, result.err)
	case <-time.After(150 * time.Millisecond):
	}

	if !hub.HandleRPCResponse("robot-001", &services.RPCResponse{RequestID: req.RequestID, Success: true}) {
		t.Fatalf("matched rpc_response returned false")
	}
	select {
	case result := <-resultC:
		if result.err != nil || result.resp == nil || !result.resp.Success {
			t.Fatalf("matched response result resp=%+v err=%v", result.resp, result.err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for matched rpc_response")
	}
}

func TestRecorderWebSocketEmptyStateUpdateDoesNotMarkSynced(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)
	axon.receiveRPC(t, "get_state")

	axon.sendStateUpdate(t, "", "")
	waitForRecorderCachedRawState(t, hub, "robot-001", false, "", "")

	router := newRecorderInteractionRouter(handler)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode state response: %v", err)
	}
	if body["state_synced"] != false || body["syncing"] != true {
		t.Fatalf("empty state_update should keep syncing; body=%v", body)
	}
}

func TestRecorderWebSocketReplacementIgnoresOldConnectionLateMessages(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-old-late", "pending", false)
	seedRecorderInteractionTask(t, db, 101, "task-current-config", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	oldAxon := connectFakeRecorderAxon(t, wsURL)
	oldAxon.receiveRPC(t, "get_state")
	oldConn := hub.Get("robot-001")
	if oldConn == nil {
		t.Fatalf("old recorder connection was not registered")
	}

	newAxon := connectFakeRecorderAxon(t, wsURL)
	getState := newAxon.receiveRPC(t, "get_state")
	newAxon.respondRPC(t, getState, true, "", map[string]interface{}{"state": "idle"})
	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")

	handler.handleMessage("robot-001", oldConn, map[string]interface{}{
		"type": "state_update",
		"data": map[string]interface{}{"current": "ready", "task_id": "task-old-late"},
	})
	handler.handleMessage("robot-001", oldConn, map[string]interface{}{
		"type": "config_applied",
		"data": map[string]interface{}{"task_id": "task-old-late"},
	})
	assertRecorderInteractionTaskStatus(t, db, "task-old-late", "pending")

	router := newRecorderInteractionRouter(handler)
	resultC := recorderInteractionPostAsync(router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-current-config"}}`)
	configReq := newAxon.receiveRPC(t, "config")
	handler.handleMessage("robot-001", oldConn, map[string]interface{}{
		"type":       "rpc_response",
		"request_id": configReq.RequestID,
		"success":    true,
	})
	select {
	case w := <-resultC:
		t.Fatalf("old rpc_response completed current config request: status=%d body=%s", w.Code, w.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	newAxon.respondRPC(t, configReq, true, "", nil)
	w := receiveRecorderHTTPResponse(t, resultC)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertRecorderInteractionTaskStatus(t, db, "task-current-config", "ready")
}

func TestRecorderWebSocketGetStateTimeoutLeavesUnsyncedAndBlocksConfig(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-timeout-blocked", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)
	axon.receiveRPC(t, "get_state")

	waitForRecorderConnectionSyncedFlagAfter(t, hub, "robot-001", false, 1200*time.Millisecond)

	router := newRecorderInteractionRouter(handler)
	stateW := httptest.NewRecorder()
	router.ServeHTTP(stateW, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/state", nil))
	if stateW.Code != http.StatusOK {
		t.Fatalf("state status=%d want=%d body=%s", stateW.Code, http.StatusOK, stateW.Body.String())
	}
	var stateBody map[string]interface{}
	if err := json.Unmarshal(stateW.Body.Bytes(), &stateBody); err != nil {
		t.Fatalf("decode state response: %v", err)
	}
	if stateBody["state_synced"] != false || stateBody["syncing"] != true {
		t.Fatalf("timed-out get_state should keep syncing; body=%v", stateBody)
	}

	configW := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-timeout-blocked"}}`)
	if configW.Code != http.StatusConflict {
		t.Fatalf("config status=%d want=%d body=%s", configW.Code, http.StatusConflict, configW.Body.String())
	}
	axon.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-timeout-blocked", "pending")

	axon.sendStateUpdate(t, "idle", "")
	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")
	resultC := recorderInteractionPostAsync(router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-timeout-blocked"}}`)
	configReq := axon.receiveRPC(t, "config")
	axon.respondRPC(t, configReq, true, "", nil)
	configW = receiveRecorderHTTPResponse(t, resultC)
	if configW.Code != http.StatusOK {
		t.Fatalf("config after state_update status=%d want=%d body=%s", configW.Code, http.StatusOK, configW.Body.String())
	}
	assertRecorderInteractionTaskStatus(t, db, "task-timeout-blocked", "ready")
}

func TestRecorderWebSocketGetStateFailureLeavesUnsyncedAndBlocksConfig(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-blocked", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)

	req := axon.receiveRPC(t, "get_state")
	axon.respondRPC(t, req, false, "state unavailable", nil)
	waitForRecorderConnectionSyncedFlag(t, hub, "robot-001", false)

	router := newRecorderInteractionRouter(handler)
	w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-blocked"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	axon.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-blocked", "pending")
}

func TestRecorderWebSocketRecordingReconnectRestoresSameTaskWithoutDuplicateConfig(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-recording-reconnect", "in_progress", false)
	seedRecorderInteractionTask(t, db, 101, "task-next-recording", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")

	first := connectFakeRecorderAxon(t, wsURL)
	first.receiveRPC(t, "get_state")
	first.closeNow()
	waitForRecorderHubDisconnected(t, hub, "robot-001")
	waitForRecorderInteractionTaskStatus(t, db, "task-recording-reconnect", "pending")

	second := connectFakeRecorderAxon(t, wsURL)
	req := second.receiveRPC(t, "get_state")
	second.respondRPC(t, req, true, "", map[string]interface{}{"state": "recording", "task_id": "task-recording-reconnect"})
	waitForRecorderInteractionTaskStatus(t, db, "task-recording-reconnect", "in_progress")
	waitForRecorderCachedState(t, hub, "robot-001", true, "recording", "task-recording-reconnect")

	router := newRecorderInteractionRouter(handler)
	w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-next-recording"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status after recording reconnect=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	second.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-next-recording", "pending")
}

func TestRecorderWebSocketRPCActionProtocol(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-protocol", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)
	getState := axon.receiveRPC(t, "get_state")
	axon.respondRPC(t, getState, true, "", map[string]interface{}{"state": "idle"})
	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")
	router := newRecorderInteractionRouter(handler)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantAction string
		check      func(t *testing.T, req services.RPCRequest)
	}{
		{
			name:       "config forwards task_config payload",
			method:     http.MethodPost,
			path:       "/recorder/robot-001/config",
			body:       `{"task_config":{"task_id":"task-protocol","device_id":"robot-001"}}`,
			wantAction: "config",
			check: func(t *testing.T, req services.RPCRequest) {
				t.Helper()
				tc, ok := req.Params["task_config"].(map[string]interface{})
				if !ok || tc["task_id"] != "task-protocol" || tc["device_id"] != "robot-001" {
					t.Fatalf("config params=%#v missing task_config", req.Params)
				}
			},
		},
		{
			name:       "begin forwards task_id",
			method:     http.MethodPost,
			path:       "/recorder/robot-001/begin",
			body:       `{"task_id":"task-protocol"}`,
			wantAction: "begin",
			check: func(t *testing.T, req services.RPCRequest) {
				t.Helper()
				if req.Params["task_id"] != "task-protocol" {
					t.Fatalf("begin params=%#v missing task_id", req.Params)
				}
			},
		},
		{
			name:       "cancel forwards task_id",
			method:     http.MethodPost,
			path:       "/recorder/robot-001/cancel",
			body:       `{"task_id":"task-protocol"}`,
			wantAction: "cancel",
			check: func(t *testing.T, req services.RPCRequest) {
				t.Helper()
				if req.Params["task_id"] != "task-protocol" {
					t.Fatalf("cancel params=%#v missing task_id", req.Params)
				}
			},
		},
		{
			name:       "clear strips http task_id payload",
			method:     http.MethodPost,
			path:       "/recorder/robot-001/clear",
			body:       `{"task_id":"task-protocol"}`,
			wantAction: "clear",
			check: func(t *testing.T, req services.RPCRequest) {
				t.Helper()
				if len(req.Params) != 0 {
					t.Fatalf("clear params=%#v want empty", req.Params)
				}
			},
		},
		{name: "finish forwards task_id", method: http.MethodPost, path: "/recorder/robot-001/finish", body: `{"task_id":"task-protocol"}`, wantAction: "finish"},
		{name: "pause action", method: http.MethodPost, path: "/recorder/robot-001/pause", body: `{}`, wantAction: "pause"},
		{name: "resume action", method: http.MethodPost, path: "/recorder/robot-001/resume", body: `{}`, wantAction: "resume"},
		{name: "quit action", method: http.MethodPost, path: "/recorder/robot-001/quit", body: `{}`, wantAction: "quit"},
		{name: "get_stats action", method: http.MethodGet, path: "/recorder/robot-001/stats", wantAction: "get_stats"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultC := recorderInteractionRequestAsync(router, tc.method, tc.path, tc.body)
			req := axon.receiveRPC(t, tc.wantAction)
			if tc.check != nil {
				tc.check(t, req)
			}
			axon.respondRPC(t, req, true, "", map[string]interface{}{"ok": true})
			w := receiveRecorderHTTPResponse(t, resultC)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
}

func TestRecorderWebSocketConnectGetStateIdleMarksStateSynced(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)

	req := axon.receiveRPC(t, "get_state")
	axon.respondRPC(t, req, true, "", map[string]interface{}{"state": "idle"})

	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")

	router := newRecorderInteractionRouter(handler)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/recorder/robot-001/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode state response: %v", err)
	}
	if body["connected"] != true || body["state_synced"] != true || body["syncing"] != false || body["current_state"] != "idle" {
		t.Fatalf("unexpected state response: %v", body)
	}
}

func TestRecorderWebSocketStateUpdateBeforeGetStateResponseIsIdempotent(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-ready-ws", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)

	axon.sendStateUpdate(t, "ready", "task-ready-ws")
	waitForRecorderInteractionTaskStatus(t, db, "task-ready-ws", "ready")
	readyAt := recorderInteractionTaskTimestamp(t, db, "task-ready-ws", "ready_at")
	if readyAt == "" {
		t.Fatalf("ready_at was not set after state_update")
	}

	req := axon.receiveRPC(t, "get_state")
	axon.respondRPC(t, req, true, "", map[string]interface{}{"state": "ready", "task_id": "task-ready-ws"})

	waitForRecorderCachedState(t, hub, "robot-001", true, "ready", "task-ready-ws")
	assertRecorderInteractionTaskStatus(t, db, "task-ready-ws", "ready")
	if got := recorderInteractionTaskTimestamp(t, db, "task-ready-ws", "ready_at"); got != readyAt {
		t.Fatalf("ready_at changed after duplicate get_state: got=%q want=%q", got, readyAt)
	}
}

func TestRecorderStateSnapshotLogsOnlyStateChanges(t *testing.T) {
	var buf bytes.Buffer
	previousLogger := logger.Get()
	logger.Set(log.New(&buf, "", 0))
	defer logger.Set(previousLogger)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{}, nil)
	rc := hub.NewRecorderConn(nil, "robot-001", "127.0.0.1")

	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "idle"}, "get_state"); err != nil {
		t.Fatalf("apply idle snapshot: %v", err)
	}
	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "idle"}, "state_update"); err != nil {
		t.Fatalf("apply duplicate idle snapshot: %v", err)
	}
	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "ready", TaskID: "task-ready"}, "state_update"); err != nil {
		t.Fatalf("apply ready snapshot: %v", err)
	}
	if err := handler.applyRecorderStateSnapshot(rc, services.RecorderState{CurrentState: "ready", TaskID: "task-ready"}, "rpc_response:config"); err != nil {
		t.Fatalf("apply duplicate ready snapshot: %v", err)
	}

	output := buf.String()
	if got := strings.Count(output, "state changed:"); got != 2 {
		t.Fatalf("state change log count=%d want=2 output=%q", got, output)
	}
	if !strings.Contains(output, "[RECORDER][robot-001] state changed: unknown -> idle source=get_state") {
		t.Fatalf("initial state change log missing: %q", output)
	}
	if !strings.Contains(output, "[RECORDER][robot-001][task-ready] state changed: idle -> ready source=state_update") {
		t.Fatalf("ready state change log missing: %q", output)
	}
	if strings.Contains(output, "rpc_response:config") {
		t.Fatalf("duplicate rpc_response snapshot should not be logged: %q", output)
	}
}

func TestRecorderWebSocketGetStateReadyRejectsConfigWithoutSendingRPC(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-current", "pending", false)
	seedRecorderInteractionTask(t, db, 101, "task-next", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)

	req := axon.receiveRPC(t, "get_state")
	axon.respondRPC(t, req, true, "", map[string]interface{}{"state": "ready", "task_id": "task-current"})
	waitForRecorderInteractionTaskStatus(t, db, "task-current", "ready")
	waitForRecorderCachedState(t, hub, "robot-001", true, "ready", "task-current")

	router := newRecorderInteractionRouter(handler)
	w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-next"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	axon.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-next", "pending")
}

func TestRecorderWebSocketUnsyncedConfigRejectedUntilValidStateUpdate(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-after-sync", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")
	axon := connectFakeRecorderAxon(t, wsURL)

	axon.receiveRPC(t, "get_state")
	router := newRecorderInteractionRouter(handler)
	w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-after-sync"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	axon.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-after-sync", "pending")

	axon.sendStateUpdate(t, "idle", "")
	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")

	resultC := recorderInteractionPostAsync(router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-after-sync"}}`)
	configReq := axon.receiveRPC(t, "config")
	axon.respondRPC(t, configReq, true, "", nil)
	w = receiveRecorderHTTPResponse(t, resultC)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertRecorderInteractionTaskStatus(t, db, "task-after-sync", "ready")
	axon.assertNoRPC(t, 150*time.Millisecond)
}

func TestRecorderWebSocketReadyReconnectRestoresSameTaskWithoutDuplicateConfig(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-ready-reconnect", "ready", false)
	seedRecorderInteractionTask(t, db, 101, "task-next-reconnect", "pending", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")

	first := connectFakeRecorderAxon(t, wsURL)
	first.receiveRPC(t, "get_state")
	first.closeNow()
	waitForRecorderHubDisconnected(t, hub, "robot-001")
	waitForRecorderInteractionTaskStatus(t, db, "task-ready-reconnect", "pending")

	router := newRecorderInteractionRouter(handler)
	w := recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-ready-reconnect"}}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status during disconnect=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}

	second := connectFakeRecorderAxon(t, wsURL)
	req := second.receiveRPC(t, "get_state")
	second.respondRPC(t, req, true, "", map[string]interface{}{"state": "ready", "task_id": "task-ready-reconnect"})
	waitForRecorderInteractionTaskStatus(t, db, "task-ready-reconnect", "ready")
	waitForRecorderCachedState(t, hub, "robot-001", true, "ready", "task-ready-reconnect")

	w = recorderInteractionPost(t, router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-next-reconnect"}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status after ready reconnect=%d want=%d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	second.assertNoRPC(t, 150*time.Millisecond)
	assertRecorderInteractionTaskStatus(t, db, "task-next-reconnect", "pending")
}

func TestRecorderWebSocketIdleReconnectAllowsSingleConfig(t *testing.T) {
	db := newRecorderInteractionDB(t)
	seedRecorderInteractionDevice(t, db, "robot-001", 1, 101)
	seedRecorderInteractionTask(t, db, 101, "task-idle-reconnect", "ready", false)

	hub := services.NewRecorderHub()
	handler := NewRecorderHandler(hub, &config.RecorderConfig{ResponseTimeout: 1}, db)
	wsURL := newRecorderWebSocketTestServer(t, handler, "robot-001")

	first := connectFakeRecorderAxon(t, wsURL)
	first.receiveRPC(t, "get_state")
	first.closeNow()
	waitForRecorderHubDisconnected(t, hub, "robot-001")
	waitForRecorderInteractionTaskStatus(t, db, "task-idle-reconnect", "pending")

	second := connectFakeRecorderAxon(t, wsURL)
	req := second.receiveRPC(t, "get_state")
	second.respondRPC(t, req, true, "", map[string]interface{}{"state": "idle"})
	waitForRecorderCachedState(t, hub, "robot-001", true, "idle", "")

	router := newRecorderInteractionRouter(handler)
	resultC := recorderInteractionPostAsync(router, "/recorder/robot-001/config", `{"task_config":{"task_id":"task-idle-reconnect"}}`)
	configReq := second.receiveRPC(t, "config")
	second.respondRPC(t, configReq, true, "", nil)
	w := receiveRecorderHTTPResponse(t, resultC)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	assertRecorderInteractionTaskStatus(t, db, "task-idle-reconnect", "ready")
	second.assertNoRPC(t, 150*time.Millisecond)
}

type fakeRecorderAxon struct {
	conn     *websocket.Conn
	requests chan services.RPCRequest
	cancel   context.CancelFunc
}

func newRecorderWebSocketTestServer(t *testing.T, handler *RecorderHandler, deviceID string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.HandleWebSocket(w, r, deviceID)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func connectFakeRecorderAxon(t *testing.T, wsURL string) *fakeRecorderAxon {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial fake recorder websocket: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	axon := &fakeRecorderAxon{
		conn:     conn,
		requests: make(chan services.RPCRequest, 16),
		cancel:   cancel,
	}
	t.Cleanup(func() {
		cancel()
		_ = conn.CloseNow()
	})
	go func() {
		for {
			var req services.RPCRequest
			if err := wsjson.Read(ctx, conn, &req); err != nil {
				return
			}
			axon.requests <- req
		}
	}()
	return axon
}

func (f *fakeRecorderAxon) receiveRPC(t *testing.T, wantAction string) services.RPCRequest {
	t.Helper()
	return receiveRecorderRPCRequest(t, f.requests, wantAction)
}

func (f *fakeRecorderAxon) respondRPC(t *testing.T, req services.RPCRequest, success bool, message string, data map[string]interface{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp := services.RPCResponse{
		Type:      "rpc_response",
		RequestID: req.RequestID,
		Success:   success,
		Message:   message,
		Data:      data,
	}
	if err := wsjson.Write(ctx, f.conn, resp); err != nil {
		t.Fatalf("write fake recorder rpc response: %v", err)
	}
}

func (f *fakeRecorderAxon) sendStateUpdate(t *testing.T, current string, taskID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data := map[string]interface{}{"current": current}
	if taskID != "" {
		data["task_id"] = taskID
	}
	msg := map[string]interface{}{
		"type": "state_update",
		"data": data,
	}
	if err := wsjson.Write(ctx, f.conn, msg); err != nil {
		t.Fatalf("write fake recorder state_update: %v", err)
	}
}

func (f *fakeRecorderAxon) assertNoRPC(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case req := <-f.requests:
		t.Fatalf("unexpected recorder RPC: %#v", req)
	case <-time.After(duration):
	}
}

func (f *fakeRecorderAxon) closeNow() {
	f.cancel()
	_ = f.conn.CloseNow()
}

func recorderInteractionPostAsync(router *gin.Engine, path string, body string) <-chan *httptest.ResponseRecorder {
	return recorderInteractionRequestAsync(router, http.MethodPost, path, body)
}

func recorderInteractionRequestAsync(router *gin.Engine, method string, path string, body string) <-chan *httptest.ResponseRecorder {
	resultC := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
		}
		router.ServeHTTP(w, req)
		resultC <- w
	}()
	return resultC
}

func receiveRecorderHTTPResponse(t *testing.T, resultC <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-resultC:
		return w
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for recorder HTTP response")
	}
	return nil
}

func waitForRecorderConnectionSyncedFlagAfter(t *testing.T, hub *services.RecorderHub, deviceID string, wantSynced bool, minWait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	readyAt := time.Now().Add(minWait)
	for time.Now().Before(deadline) {
		rc := hub.Get(deviceID)
		if rc != nil && rc.IsStateSynced() == wantSynced && time.Now().After(readyAt) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rc := hub.Get(deviceID)
	if rc == nil {
		t.Fatalf("recorder %s not connected while waiting after %s for synced=%v", deviceID, minWait, wantSynced)
	}
	t.Fatalf("recorder synced=%v want=%v after %s", rc.IsStateSynced(), wantSynced, minWait)
}

func waitForRecorderCachedRawState(t *testing.T, hub *services.RecorderHub, deviceID string, wantSynced bool, wantState string, wantTaskID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rc := hub.Get(deviceID)
		if rc != nil && rc.IsStateSynced() == wantSynced {
			st := rc.GetState()
			if strings.TrimSpace(st.CurrentState) == wantState && strings.TrimSpace(st.TaskID) == wantTaskID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	rc := hub.Get(deviceID)
	if rc == nil {
		t.Fatalf("recorder %s not connected while waiting for raw state=%q task=%q synced=%v", deviceID, wantState, wantTaskID, wantSynced)
	}
	st := rc.GetState()
	t.Fatalf("recorder raw state current=%q task=%q synced=%v, want current=%q task=%q synced=%v", st.CurrentState, st.TaskID, rc.IsStateSynced(), wantState, wantTaskID, wantSynced)
}

func waitForRecorderConnectionSyncedFlag(t *testing.T, hub *services.RecorderHub, deviceID string, wantSynced bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rc := hub.Get(deviceID)
		if rc != nil && rc.IsStateSynced() == wantSynced {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rc := hub.Get(deviceID)
	if rc == nil {
		t.Fatalf("recorder %s not connected while waiting for synced=%v", deviceID, wantSynced)
	}
	t.Fatalf("recorder synced=%v want=%v", rc.IsStateSynced(), wantSynced)
}

func waitForRecorderCachedState(t *testing.T, hub *services.RecorderHub, deviceID string, wantSynced bool, wantState string, wantTaskID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rc := hub.Get(deviceID)
		if rc != nil && rc.IsStateSynced() == wantSynced {
			st := rc.GetState()
			if strings.EqualFold(strings.TrimSpace(st.CurrentState), wantState) && strings.TrimSpace(st.TaskID) == wantTaskID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	rc := hub.Get(deviceID)
	if rc == nil {
		t.Fatalf("recorder %s not connected while waiting for state=%s task=%s synced=%v", deviceID, wantState, wantTaskID, wantSynced)
	}
	st := rc.GetState()
	t.Fatalf("recorder state current=%q task=%q synced=%v, want current=%q task=%q synced=%v", st.CurrentState, st.TaskID, rc.IsStateSynced(), wantState, wantTaskID, wantSynced)
}

func waitForRecorderHubDisconnected(t *testing.T, hub *services.RecorderHub, deviceID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Get(deviceID) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("recorder %s remained connected", deviceID)
}

func waitForRecorderInteractionTaskStatus(t *testing.T, db *sqlx.DB, taskID string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		if err := db.Get(&got, `SELECT status FROM tasks WHERE task_id = ?`, taskID); err != nil {
			t.Fatalf("query task status: %v", err)
		}
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertRecorderInteractionTaskStatus(t, db, taskID, want)
}

func recorderInteractionTaskTimestamp(t *testing.T, db *sqlx.DB, taskID string, column string) string {
	t.Helper()
	if column != "ready_at" && column != "started_at" && column != "completed_at" {
		t.Fatalf("unexpected timestamp column %q", column)
	}
	var got string
	if err := db.Get(&got, `SELECT COALESCE(CAST(`+column+` AS TEXT), '') FROM tasks WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("query %s for %s: %v", column, taskID, err)
	}
	return got
}
