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
)

var (
	// ErrTransferWriteTimeout indicates that a transfer WebSocket write exceeded its deadline.
	ErrTransferWriteTimeout = errors.New("transfer write timeout")
)

const (
	// DefaultTransferWriteTimeout caps device writes when no handler-specific timeout is configured.
	DefaultTransferWriteTimeout = 10 * time.Second
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
	ActiveCount       int       `json:"active_count"`
	UploadingCount    int       `json:"uploading_count"`
	RetryWaitCount    int       `json:"retry_wait_count"`
	WaitingACKCount   int       `json:"waiting_ack_count"`
	WaitingACKTaskIDs []string  `json:"waiting_ack_task_ids"`
	CompletedCount    int       `json:"completed_count"`
	FailedCount       int       `json:"failed_count"`
	PendingBytes      int64     `json:"pending_bytes"`
	BytesPerSec       int64     `json:"bytes_per_sec"`
	Uploads           []Upload  `json:"uploads"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Upload is one axon_transfer upload_state record reported in a status snapshot.
type Upload struct {
	TaskID          string `json:"task_id"`
	Status          string `json:"status"`
	S3Key           string `json:"s3_key"`
	ObjectKey       string `json:"object_key"`
	FileSizeBytes   int64  `json:"file_size_bytes"`
	ChecksumSHA256  string `json:"checksum_sha256"`
	BytesUploaded   int64  `json:"bytes_uploaded"`
	UploadMode      string `json:"upload_mode"`
	RetryCount      int    `json:"retry_count"`
	NextRetryAt     string `json:"next_retry_at"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	CompletedAt     string `json:"completed_at"`
	DeleteLastError string `json:"delete_last_error"`
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

// TransferConn holds the WebSocket connection and metadata for a connected device
type TransferConn struct {
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

// GetDeviceID implements Connection.
func (d *TransferConn) GetDeviceID() string { return d.DeviceID }

// GetWSConn implements Connection.
func (d *TransferConn) GetWSConn() *websocket.Conn { return d.Conn }

// GetConnectedAt implements Connection.
func (d *TransferConn) GetConnectedAt() time.Time { return d.ConnectedAt }

// GetLastSeenAt implements Connection.
func (d *TransferConn) GetLastSeenAt() time.Time { return d.LastSeenAt }

// RecordEvent appends an event to the device's ring buffer
func (d *TransferConn) RecordEvent(direction string, payload map[string]interface{}) {
	d.events.Push(DeviceEvent{
		Direction: direction,
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// Events returns up to limit recent events
func (d *TransferConn) Events(limit int) []DeviceEvent {
	return d.events.Slice(limit)
}

// UpdateStatus updates the device status snapshot (thread-safe)
func (d *TransferConn) UpdateStatus(s DeviceStatus) {
	d.StatusMu.Lock()
	defer d.StatusMu.Unlock()
	s.UpdatedAt = time.Now()
	d.Status = s
}

// GetStatus returns a copy of the current device status (thread-safe)
func (d *TransferConn) GetStatus() DeviceStatus {
	d.StatusMu.RLock()
	defer d.StatusMu.RUnlock()
	return d.Status
}

// DeviceInfo is a read-only snapshot of a connected device
type DeviceInfo struct {
	DeviceID    string       `json:"device_id"`
	RemoteIP    string       `json:"remote_ip"`
	ConnectedAt time.Time    `json:"connected_at"`
	LastSeenAt  time.Time    `json:"last_seen_at"`
	Status      DeviceStatus `json:"status"`
}

// TransferHub manages all active WebSocket device connections.
// It embeds the generic Hub[*TransferConn] to handle all concurrency and
// bookkeeping, and only adds Transfer-specific behaviour on top.
type TransferHub struct {
	*Hub[*TransferConn]
	maxEventsPerDev int
}

// NewTransferHub creates a new TransferHub
func NewTransferHub(maxEventsPerDevice int) *TransferHub {
	return &TransferHub{
		Hub:             newHub[*TransferConn]("TRANSFER"),
		maxEventsPerDev: maxEventsPerDevice,
	}
}

// NewTransferConn creates a TransferConn with a ring buffer sized by hub config
func (h *TransferHub) NewTransferConn(conn *websocket.Conn, deviceID, remoteIP string) *TransferConn {
	return &TransferConn{
		Conn:        conn,
		DeviceID:    deviceID,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		LastSeenAt:  time.Now(),
		events:      newRingBuffer(h.maxEventsPerDev),
	}
}

// Connect registers a device connection. If the device already has an active
// connection, returns false and the caller must close the new WebSocket.
func (h *TransferHub) Connect(deviceID string, dc *TransferConn) bool {
	return h.connect(deviceID, dc)
}

// ConnectWithStaleThreshold registers a transfer connection, replacing and
// returning the old connection when it has not been seen within staleThreshold.
func (h *TransferHub) ConnectWithStaleThreshold(deviceID string, dc *TransferConn, staleThreshold time.Duration) (*TransferConn, bool) {
	return h.connectWithStaleThreshold(deviceID, dc, staleThreshold)
}

// ConnectReplacingExisting registers a transfer connection, replacing any
// existing connection for the same device and returning it to the caller.
func (h *TransferHub) ConnectReplacingExisting(deviceID string, dc *TransferConn) *TransferConn {
	return h.connectReplacingExisting(deviceID, dc)
}

// Disconnect removes a device connection
func (h *TransferHub) Disconnect(deviceID string, dc *TransferConn) bool {
	return h.disconnect(deviceID, dc)
}

// Get returns the TransferConn for a device, or nil if not connected
func (h *TransferHub) Get(deviceID string) *TransferConn {
	return h.get(deviceID)
}

// ListDevices returns a snapshot of all connected device IDs and their metadata
func (h *TransferHub) ListDevices() []DeviceInfo {
	conns := h.list()
	result := make([]DeviceInfo, 0, len(conns))
	for _, dc := range conns {
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

// SendToDevice sends a JSON message to a connected device via WebSocket.
func (h *TransferHub) SendToDevice(ctx context.Context, deviceID string, msg map[string]interface{}) error {
	return h.SendToDeviceWithTimeout(ctx, deviceID, msg, DefaultTransferWriteTimeout)
}

// SendToDeviceWithTimeout sends a JSON message to a connected device with a bounded write deadline.
func (h *TransferHub) SendToDeviceWithTimeout(ctx context.Context, deviceID string, msg map[string]interface{}, timeout time.Duration) error {
	dc := h.Get(deviceID)
	if dc == nil {
		return fmt.Errorf("device %s not connected", deviceID)
	}
	return h.SendToConnWithTimeout(ctx, dc, msg, timeout)
}

// SendToConnWithTimeout sends a JSON message to an existing transfer connection.
func (h *TransferHub) SendToConnWithTimeout(ctx context.Context, dc *TransferConn, msg map[string]interface{}, timeout time.Duration) error {
	if dc == nil || dc.Conn == nil {
		return fmt.Errorf("device not connected")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultTransferWriteTimeout
	}

	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dc.WriteMu.Lock()
	defer dc.WriteMu.Unlock()

	if err := wsjson.Write(writeCtx, dc.Conn, msg); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(writeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w after %s: failed to send message to device %s", ErrTransferWriteTimeout, timeout, dc.DeviceID)
		}
		return fmt.Errorf("failed to send message to device %s: %w", dc.DeviceID, err)
	}

	dc.RecordEvent("outbound", msg)
	return nil
}
