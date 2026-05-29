// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"sync"
	"time"
)

// DeviceStateEvent is a versioned event emitted when recorder state or device
// connection state changes.
type DeviceStateEvent map[string]interface{}

// DeviceStateBroker fans out device state events to API stream subscribers.
type DeviceStateBroker struct {
	mu          sync.RWMutex
	nextSubID   uint64
	versions    map[string]uint64
	subscribers map[uint64]chan DeviceStateEvent
}

// NewDeviceStateBroker creates a state event broker.
func NewDeviceStateBroker() *DeviceStateBroker {
	return &DeviceStateBroker{
		versions:    make(map[string]uint64),
		subscribers: make(map[uint64]chan DeviceStateEvent),
	}
}

// CurrentVersion returns the latest emitted version for a device.
func (b *DeviceStateBroker) CurrentVersion(deviceID string) uint64 {
	if b == nil || deviceID == "" {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.versions[deviceID]
}

// Publish assigns a per-device monotonically increasing state_version and fans
// the event out to subscribers. Slow subscribers may drop events; clients must
// recover by fetching a full snapshot after reconnect.
func (b *DeviceStateBroker) Publish(deviceID string, event DeviceStateEvent) DeviceStateEvent {
	if b == nil || deviceID == "" || event == nil {
		return event
	}

	b.mu.Lock()
	version := b.versions[deviceID] + 1
	b.versions[deviceID] = version
	event["device_id"] = deviceID
	event["state_version"] = version
	if _, ok := event["updated_at"]; !ok {
		event["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}

	subscribers := make([]chan DeviceStateEvent, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subscribers = append(subscribers, ch)
	}
	b.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- cloneDeviceStateEvent(event):
		default:
		}
	}
	return event
}

// Subscribe registers a subscriber and returns a channel plus cleanup function.
func (b *DeviceStateBroker) Subscribe(buffer int) (<-chan DeviceStateEvent, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	ch := make(chan DeviceStateEvent, buffer)
	if b == nil {
		close(ch)
		return ch, func() {}
	}

	b.mu.Lock()
	b.nextSubID++
	id := b.nextSubID
	b.subscribers[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(existing)
		}
		b.mu.Unlock()
	}
}

func cloneDeviceStateEvent(src DeviceStateEvent) DeviceStateEvent {
	out := make(DeviceStateEvent, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
