// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package services provides business logic services for Keystone Edge
package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// DeviceEvent represents a single event recorded for a device
type DeviceEvent struct {
	Direction string                 `json:"direction"` // "inbound" or "outbound"
	Timestamp time.Time              `json:"timestamp"`
	Payload   map[string]interface{} `json:"payload"`
}

// DeviceStatus holds the latest status snapshot reported by the device
type DeviceStatus struct {
	Version           string    `json:"version"`
	PendingCount      int       `json:"pending_count"`
	UploadingCount    int       `json:"uploading_count"`
	WaitingACKCount   int       `json:"waiting_ack_count"`
	WaitingACKTaskIDs []string  `json:"waiting_ack_task_ids"`
	CompletedCount    int       `json:"completed_count"`
	FailedCount       int       `json:"failed_count"`
	PendingBytes      int64     `json:"pending_bytes"`
	BytesPerSec       int64     `json:"bytes_per_sec"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ringBuffer is a fixed-size circular buffer for DeviceEvent
type ringBuffer struct {
	buf  []DeviceEvent
	size int
	head int
	tail int
	full bool
	mu   sync.Mutex
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		buf:  make([]DeviceEvent, size),
		size: size,
	}
}

// Push adds an event to the ring buffer, overwriting the oldest if full
func (r *ringBuffer) Push(e DeviceEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.tail] = e
	r.tail = (r.tail + 1) % r.size
	if r.full {
		r.head = (r.head + 1) % r.size
	}
	r.full = r.tail == r.head
}

// Slice returns up to n most recent events (oldest first)
func (r *ringBuffer) Slice(n int) []DeviceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	var count int
	if r.full {
		count = r.size
	} else if r.tail >= r.head {
		count = r.tail - r.head
	} else {
		count = r.size - r.head + r.tail
	}

	if n > count {
		n = count
	}
	if n <= 0 {
		return nil
	}

	result := make([]DeviceEvent, n)
	start := (r.head + count - n + r.size) % r.size
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.size]
	}
	return result
}

// DeviceConn holds the WebSocket connection and metadata for a connected device
type DeviceConn struct {
	Conn        *websocket.Conn
	DeviceID    string
	RemoteIP    string
	ConnectedAt time.Time
	LastSeenAt  time.Time
	Status      DeviceStatus
	events      *ringBuffer
	WriteMu     sync.Mutex
	StatusMu    sync.RWMutex
}

// RecordEvent appends an event to the device's ring buffer
func (d *DeviceConn) RecordEvent(direction string, payload map[string]interface{}) {
	d.events.Push(DeviceEvent{
		Direction: direction,
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// Events returns up to limit recent events
func (d *DeviceConn) Events(limit int) []DeviceEvent {
	return d.events.Slice(limit)
}

// UpdateStatus updates the device status snapshot (thread-safe)
func (d *DeviceConn) UpdateStatus(s DeviceStatus) {
	d.StatusMu.Lock()
	defer d.StatusMu.Unlock()
	s.UpdatedAt = time.Now()
	d.Status = s
}

// GetStatus returns a copy of the current device status (thread-safe)
func (d *DeviceConn) GetStatus() DeviceStatus {
	d.StatusMu.RLock()
	defer d.StatusMu.RUnlock()
	return d.Status
}

// TransferHub manages all active WebSocket device connections
type TransferHub struct {
	connections     map[string]*DeviceConn
	mu              sync.RWMutex
	maxEventsPerDev int
}

// NewTransferHub creates a new TransferHub
func NewTransferHub(maxEventsPerDevice int) *TransferHub {
	return &TransferHub{
		connections:     make(map[string]*DeviceConn),
		maxEventsPerDev: maxEventsPerDevice,
	}
}

// NewDeviceConn creates a DeviceConn with a ring buffer sized by hub config
func (h *TransferHub) NewDeviceConn(conn *websocket.Conn, deviceID, remoteIP string) *DeviceConn {
	return &DeviceConn{
		Conn:        conn,
		DeviceID:    deviceID,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		LastSeenAt:  time.Now(),
		events:      newRingBuffer(h.maxEventsPerDev),
	}
}

// Connect registers a device connection
func (h *TransferHub) Connect(deviceID string, dc *DeviceConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, exists := h.connections[deviceID]; exists && old != nil && old.Conn != nil && old != dc {
		logger.Printf("[TRANSFER] TransferHub: closing previous connection for device %s (replaced by new)", deviceID)
		_ = old.Conn.Close(websocket.StatusPolicyViolation, "replaced by newer connection")
	}
	h.connections[deviceID] = dc
	logger.Printf("[TRANSFER] TransferHub: device %s registered, total connections=%d", deviceID, len(h.connections))
}

// Disconnect removes a device connection
func (h *TransferHub) Disconnect(deviceID string) {
	h.mu.Lock()
	dc := h.connections[deviceID]
	delete(h.connections, deviceID)
	h.mu.Unlock()

	if dc == nil {
		logger.Printf("[TRANSFER] TransferHub: Disconnect called for unknown device %s", deviceID)
		return
	}
	logger.Printf("[TRANSFER] TransferHub: device %s disconnected", deviceID)
}

// Get returns the DeviceConn for a device, or nil if not connected
func (h *TransferHub) Get(deviceID string) *DeviceConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connections[deviceID]
}

// ListDevices returns a snapshot of all connected device IDs and their metadata
func (h *TransferHub) ListDevices() []DeviceInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]DeviceInfo, 0, len(h.connections))
	for _, dc := range h.connections {
		result = append(result, DeviceInfo{
			DeviceID:    dc.DeviceID,
			RemoteIP:    dc.RemoteIP,
			ConnectedAt: dc.ConnectedAt,
			LastSeenAt:  dc.LastSeenAt,
			Status:      dc.GetStatus(),
		})
	}
	return result
}

// DeviceInfo is a read-only snapshot of a connected device
type DeviceInfo struct {
	DeviceID    string       `json:"device_id"`
	RemoteIP    string       `json:"remote_ip"`
	ConnectedAt time.Time    `json:"connected_at"`
	LastSeenAt  time.Time    `json:"last_seen_at"`
	Status      DeviceStatus `json:"status"`
}

// SendToDevice sends a JSON message to a connected device via WebSocket
func (h *TransferHub) SendToDevice(ctx context.Context, deviceID string, msg map[string]interface{}) error {
	dc := h.Get(deviceID)
	if dc == nil {
		return fmt.Errorf("device %s not connected", deviceID)
	}

	dc.WriteMu.Lock()
	defer dc.WriteMu.Unlock()

	if err := wsjson.Write(ctx, dc.Conn, msg); err != nil {
		return fmt.Errorf("failed to send message to device %s: %w", deviceID, err)
	}

	dc.RecordEvent("outbound", msg)
	return nil
}
