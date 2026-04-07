// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package services provides business logic services for Keystone Edge
package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

var (
	// ErrRecorderNotConnected indicates the target recorder is not connected.
	ErrRecorderNotConnected = errors.New("recorder not connected")
	// ErrRecorderRPCTimeout indicates a recorder RPC timed out.
	ErrRecorderRPCTimeout = errors.New("recorder rpc timeout")
)

// RecorderState stores the latest state snapshot reported by a recorder.
type RecorderState struct {
	CurrentState string                 `json:"current_state"`
	TaskID       string                 `json:"task_id,omitempty"`
	UpdatedAt    time.Time              `json:"updated_at"`
	Raw          map[string]interface{} `json:"raw,omitempty"`
}

// RecorderInfo is a read-only snapshot of a connected recorder.
type RecorderInfo struct {
	DeviceID    string        `json:"device_id"`
	RemoteIP    string        `json:"remote_ip"`
	ConnectedAt time.Time     `json:"connected_at"`
	LastSeenAt  time.Time     `json:"last_seen_at"`
	State       RecorderState `json:"state"`
}

// RPCRequest represents a command sent from Keystone to Axon Recorder.
type RPCRequest struct {
	Type      string                 `json:"type"`
	RequestID string                 `json:"request_id"`
	Action    string                 `json:"action"`
	Params    map[string]interface{} `json:"params,omitempty"`
}

// RPCResponse represents the command result returned by Axon Recorder.
type RPCResponse struct {
	Type      string                 `json:"type,omitempty"`
	RequestID string                 `json:"request_id,omitempty"`
	Success   bool                   `json:"success"`
	Message   string                 `json:"message,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// PendingRPC tracks an in-flight RPC waiting for a response.
type PendingRPC struct {
	RequestID string
	ResponseC chan *RPCResponse
	CreatedAt time.Time
}

// RecorderConn holds a recorder connection and its runtime state.
type RecorderConn struct {
	Conn        *websocket.Conn
	DeviceID    string
	RemoteIP    string
	ConnectedAt time.Time
	LastSeenAt  time.Time
	State       RecorderState
	WriteMu     sync.Mutex

	PendingMu sync.Mutex
	Pending   map[string]*PendingRPC
	StateMu   sync.RWMutex
}

// GetDeviceID implements Connection.
func (r *RecorderConn) GetDeviceID() string { return r.DeviceID }

// GetWSConn implements Connection.
func (r *RecorderConn) GetWSConn() *websocket.Conn { return r.Conn }

// GetConnectedAt implements Connection.
func (r *RecorderConn) GetConnectedAt() time.Time { return r.ConnectedAt }

// GetState returns a copy of the recorder state.
func (r *RecorderConn) GetState() RecorderState {
	r.StateMu.RLock()
	defer r.StateMu.RUnlock()
	return r.State
}

// UpdateState updates the recorder state snapshot.
func (r *RecorderConn) UpdateState(state RecorderState) {
	r.StateMu.Lock()
	defer r.StateMu.Unlock()
	state.UpdatedAt = time.Now()
	r.State = state
}

// RecorderHub manages all active Axon Recorder WebSocket connections.
// It embeds the generic Hub[*RecorderConn] for connection lifecycle and
// adds the RPC request/response matching layer on top.
type RecorderHub struct {
	*Hub[*RecorderConn]
}

// NewRecorderHub creates a new RecorderHub.
func NewRecorderHub() *RecorderHub {
	return &RecorderHub{
		Hub: newHub[*RecorderConn]("RECORDER"),
	}
}

// NewRecorderConn creates a RecorderConn with default values.
func (h *RecorderHub) NewRecorderConn(conn *websocket.Conn, deviceID, remoteIP string) *RecorderConn {
	return &RecorderConn{
		Conn:        conn,
		DeviceID:    deviceID,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		LastSeenAt:  time.Now(),
		State: RecorderState{
			CurrentState: "unknown",
			UpdatedAt:    time.Now(),
		},
		Pending: make(map[string]*PendingRPC),
	}
}

// Connect registers a recorder connection, replacing any previous one for the same device.
func (h *RecorderHub) Connect(deviceID string, rc *RecorderConn) {
	h.connect(deviceID, rc)
}

// Disconnect removes a recorder connection and drains any pending RPC waiters.
func (h *RecorderHub) Disconnect(deviceID string) {
	rc, found := h.disconnect(deviceID)
	if !found {
		return
	}

	// Unblock any goroutines waiting for an RPC response from this device.
	rc.PendingMu.Lock()
	for requestID, pending := range rc.Pending {
		delete(rc.Pending, requestID)
		// Non-blocking send: the waiter may have already timed out.
		select {
		case pending.ResponseC <- &RPCResponse{
			RequestID: requestID,
			Success:   false,
			Message:   ErrRecorderNotConnected.Error(),
		}:
		default:
		}
	}
	rc.PendingMu.Unlock()
}

// Get returns the recorder connection for a device, or nil if not connected.
func (h *RecorderHub) Get(deviceID string) *RecorderConn {
	return h.get(deviceID)
}

// ListDevices returns a snapshot of all connected recorders.
func (h *RecorderHub) ListDevices() []RecorderInfo {
	conns := h.list()
	result := make([]RecorderInfo, 0, len(conns))
	for _, rc := range conns {
		result = append(result, RecorderInfo{
			DeviceID:    rc.DeviceID,
			RemoteIP:    rc.RemoteIP,
			ConnectedAt: rc.ConnectedAt,
			LastSeenAt:  rc.LastSeenAt,
			State:       rc.GetState(),
		})
	}
	return result
}

// HandleRPCResponse matches a recorder response back to the waiting request.
func (h *RecorderHub) HandleRPCResponse(deviceID string, response *RPCResponse) bool {
	rc := h.Get(deviceID)
	if rc == nil || response == nil || response.RequestID == "" {
		return false
	}

	rc.PendingMu.Lock()
	pending, ok := rc.Pending[response.RequestID]
	if ok {
		delete(rc.Pending, response.RequestID)
	}
	rc.PendingMu.Unlock()

	if !ok {
		return false
	}

	pending.ResponseC <- response
	close(pending.ResponseC)
	return true
}

// SendRPC writes an RPC request to a recorder and waits for the response.
func (h *RecorderHub) SendRPC(ctx context.Context, deviceID, action string, params map[string]interface{}, timeout time.Duration) (*RPCResponse, error) {
	rc := h.Get(deviceID)
	if rc == nil {
		return nil, ErrRecorderNotConnected
	}

	requestID := uuid.New().String()
	pending := &PendingRPC{
		RequestID: requestID,
		ResponseC: make(chan *RPCResponse, 1),
		CreatedAt: time.Now(),
	}

	rc.PendingMu.Lock()
	rc.Pending[requestID] = pending
	rc.PendingMu.Unlock()

	req := RPCRequest{
		Type:      "rpc_request",
		RequestID: requestID,
		Action:    action,
		Params:    params,
	}

	rc.WriteMu.Lock()
	writeErr := wsjson.Write(ctx, rc.Conn, req)
	rc.WriteMu.Unlock()
	if writeErr != nil {
		rc.PendingMu.Lock()
		delete(rc.Pending, requestID)
		rc.PendingMu.Unlock()
		return nil, fmt.Errorf("write rpc request: %w", writeErr)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case response := <-pending.ResponseC:
		return response, nil
	case <-waitCtx.Done():
		rc.PendingMu.Lock()
		delete(rc.Pending, requestID)
		rc.PendingMu.Unlock()
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrRecorderRPCTimeout
		}
		return nil, waitCtx.Err()
	}
}
