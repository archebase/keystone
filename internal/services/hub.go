// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package services provides business logic services for Keystone Edge
package services

import (
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/coder/websocket"
)

// Connection is implemented by every concrete WebSocket connection type
// managed by a Hub. It exposes the minimal identity and lifecycle surface
// needed by the generic Hub without leaking type-specific fields.
type Connection interface {
	// GetDeviceID returns the unique device identifier for this connection.
	GetDeviceID() string
	// GetWSConn returns the underlying websocket connection.
	GetWSConn() *websocket.Conn
	// GetConnectedAt returns the time the connection was established.
	GetConnectedAt() time.Time
}

// Hub is a generic, concurrency-safe registry of WebSocket connections keyed
// by device ID. It handles the common Connect / Disconnect / Get / List
// operations so that concrete hub types (TransferHub, RecorderHub) only need
// to add their domain-specific behaviour.
//
// T must be a pointer type that implements Connection.
type Hub[T Connection] struct {
	connections map[string]T
	mu          sync.RWMutex
	label       string // component label for log lines, e.g. "TRANSFER"
}

// newHub allocates a Hub with the given log label.
func newHub[T Connection](label string) *Hub[T] {
	return &Hub[T]{
		connections: make(map[string]T),
		label:       label,
	}
}

// connect registers conn under deviceID. If a different connection for the
// same device already exists it is forcibly closed before the new one is
// stored. Callers must pass a non-nil conn.
func (h *Hub[T]) connect(deviceID string, conn T) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if old, exists := h.connections[deviceID]; exists {
		// Only close when it is genuinely a different object to avoid closing
		// a connection that was just re-registered after an in-place upgrade.
		if old.GetWSConn() != nil && old.GetWSConn() != conn.GetWSConn() {
			logger.Printf("[%s] Hub: closing previous connection for device %s (replaced by new)", h.label, deviceID)
			_ = old.GetWSConn().Close(websocket.StatusPolicyViolation, "replaced by newer connection")
		}
	}
	h.connections[deviceID] = conn
	logger.Printf("[%s] Hub: device %s registered, total connections=%d", h.label, deviceID, len(h.connections))
}

// disconnect removes the connection for deviceID and returns it (or the zero
// value of T if the device was not registered). The caller is responsible for
// any type-specific teardown (e.g. draining pending RPCs).
func (h *Hub[T]) disconnect(deviceID string) (conn T, found bool) {
	h.mu.Lock()
	conn, found = h.connections[deviceID]
	delete(h.connections, deviceID)
	h.mu.Unlock()

	if !found {
		logger.Printf("[%s] Hub: Disconnect called for unknown device %s", h.label, deviceID)
	} else {
		logger.Printf("[%s] Hub: device %s disconnected", h.label, deviceID)
	}
	return conn, found
}

// get returns the connection for deviceID, or the zero value of T if not found.
func (h *Hub[T]) get(deviceID string) T {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connections[deviceID]
}

// list returns a snapshot of all current connections.
func (h *Hub[T]) list() []T {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]T, 0, len(h.connections))
	for _, c := range h.connections {
		result = append(result, c)
	}
	return result
}
