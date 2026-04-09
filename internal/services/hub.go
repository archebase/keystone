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

// connect registers conn under deviceID. If another connection is already
// registered for the same device, the new connection is rejected (caller must
// close it) and false is returned. Callers must pass a non-nil conn.
func (h *Hub[T]) connect(deviceID string, conn T) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if old, exists := h.connections[deviceID]; exists {
		if old.GetWSConn() != nil && old.GetWSConn() != conn.GetWSConn() {
			logger.Printf("[%s] Hub: rejecting new connection for device %s (already connected)", h.label, deviceID)
			return false
		}
	}
	h.connections[deviceID] = conn
	logger.Printf("[%s] Hub: device %s registered, total connections=%d", h.label, deviceID, len(h.connections))
	return true
}

// disconnect removes the connection for deviceID only if it matches conn.
// This avoids a stale handler goroutine deleting a newer connection after
// rejecting takeover. Returns true if an entry was removed.
func (h *Hub[T]) disconnect(deviceID string, conn T) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	current, exists := h.connections[deviceID]
	if !exists {
		logger.Printf("[%s] Hub: Disconnect called for unknown device %s", h.label, deviceID)
		return false
	}
	// Compare underlying websocket connections; type parameter T is not necessarily comparable.
	cw := current.GetWSConn()
	nw := conn.GetWSConn()
	if cw == nil || nw == nil || cw != nw {
		logger.Printf("[%s] Hub: Disconnect ignored for device %s (connection not current)", h.label, deviceID)
		return false
	}
	delete(h.connections, deviceID)
	logger.Printf("[%s] Hub: device %s disconnected", h.label, deviceID)
	return true
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
