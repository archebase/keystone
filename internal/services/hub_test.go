// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRecorderHubConnectWithStaleThresholdRejectsFreshConnection(t *testing.T) {
	hub := NewRecorderHub()
	deviceID := "robot-001"

	oldConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	oldConn.LastSeenAt = time.Now()
	if !hub.Connect(deviceID, oldConn) {
		t.Fatalf("initial connect failed")
	}

	newConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	replaced, ok := hub.ConnectWithStaleThreshold(deviceID, newConn, time.Minute)
	if ok {
		t.Fatalf("fresh duplicate connection was accepted")
	}
	if replaced != nil {
		t.Fatalf("fresh duplicate returned replaced connection")
	}
	if got := hub.Get(deviceID); got != oldConn {
		t.Fatalf("hub connection changed on rejected duplicate")
	}
}

func TestRecorderHubConnectWithStaleThresholdReplacesStaleConnection(t *testing.T) {
	hub := NewRecorderHub()
	deviceID := "robot-001"

	oldConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	oldConn.LastSeenAt = time.Now().Add(-2 * time.Minute)
	if !hub.Connect(deviceID, oldConn) {
		t.Fatalf("initial connect failed")
	}

	newConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	replaced, ok := hub.ConnectWithStaleThreshold(deviceID, newConn, time.Minute)
	if !ok {
		t.Fatalf("stale duplicate connection was rejected")
	}
	if replaced != oldConn {
		t.Fatalf("replaced=%p want oldConn=%p", replaced, oldConn)
	}
	if got := hub.Get(deviceID); got != newConn {
		t.Fatalf("hub connection=%p want newConn=%p", got, newConn)
	}

	if hub.Disconnect(deviceID, oldConn) {
		t.Fatalf("old stale connection disconnected the current hub entry")
	}
	if got := hub.Get(deviceID); got != newConn {
		t.Fatalf("old disconnect changed current hub connection")
	}
	if !hub.Disconnect(deviceID, newConn) {
		t.Fatalf("new connection did not disconnect")
	}
	if got := hub.Get(deviceID); got != nil {
		t.Fatalf("hub connection=%p want nil", got)
	}
}

func TestRecorderHubConnectReplacingExistingTakesOverFreshConnection(t *testing.T) {
	hub := NewRecorderHub()
	deviceID := "robot-001"

	oldConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	oldConn.LastSeenAt = time.Now()
	if !hub.Connect(deviceID, oldConn) {
		t.Fatalf("initial connect failed")
	}

	newConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	replaced := hub.ConnectReplacingExisting(deviceID, newConn)
	if replaced != oldConn {
		t.Fatalf("replaced=%p want oldConn=%p", replaced, oldConn)
	}
	if got := hub.Get(deviceID); got != newConn {
		t.Fatalf("hub connection=%p want newConn=%p", got, newConn)
	}
	if hub.Disconnect(deviceID, oldConn) {
		t.Fatalf("old connection disconnected the current hub entry")
	}
	if got := hub.Get(deviceID); got != newConn {
		t.Fatalf("old disconnect changed current hub connection")
	}
	if !hub.Disconnect(deviceID, newConn) {
		t.Fatalf("new connection did not disconnect")
	}
}
