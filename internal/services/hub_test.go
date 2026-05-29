// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRecorderHubConnectReplacingExistingFailsPendingRPCWaitersWithError(t *testing.T) {
	hub := NewRecorderHub()
	deviceID := "robot-001"
	serverConn, clientConn := newRecorderHubTestWebSocketPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, _, _ = clientConn.Read(ctx)
	}()

	oldConn := hub.NewRecorderConn(serverConn, deviceID, "127.0.0.1")
	if !hub.Connect(deviceID, oldConn) {
		t.Fatalf("initial connect failed")
	}

	errC := make(chan error, 1)
	go func() {
		resp, err := hub.SendRPC(ctx, deviceID, "begin", nil, time.Second)
		if !errors.Is(err, ErrRecorderNotConnected) {
			errC <- fmt.Errorf("SendRPC err=%v resp=%+v, want ErrRecorderNotConnected", err, resp)
			return
		}
		errC <- nil
	}()

	waitForPendingRecorderRPC(t, oldConn)
	newConn := hub.NewRecorderConn(&websocket.Conn{}, deviceID, "127.0.0.1")
	replaced := hub.ConnectReplacingExisting(deviceID, newConn)
	if replaced != oldConn {
		t.Fatalf("replaced=%p want oldConn=%p", replaced, oldConn)
	}

	select {
	case err := <-errC:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatalf("SendRPC did not return after connection replacement")
	}
}

func waitForPendingRecorderRPC(t *testing.T, rc *RecorderConn) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rc.PendingMu.Lock()
		n := len(rc.Pending)
		rc.PendingMu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pending RPC")
}

func newRecorderHubTestWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
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
