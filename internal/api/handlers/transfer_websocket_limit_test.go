// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
)

func TestTransferHandlerHandleWebSocket(t *testing.T) {
	t.Run("accepts message at 2 MiB read limit", func(t *testing.T) {
		hub, conn := openTransferWebSocketForLimitTest(t)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, transferStatusMessageOfSize(t, 2<<20)); err != nil {
			t.Fatalf("write 2 MiB transfer message: %v", err)
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			dc := hub.Get("robot-001")
			if dc == nil {
				t.Fatal("transfer disconnected after a 2 MiB message")
			}
			events := dc.Events(1)
			if len(events) == 1 {
				if events[0].Payload["type"] != "status" {
					t.Fatalf("last inbound message type=%v want status", events[0].Payload["type"])
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("2 MiB transfer message was not processed")
	})

	t.Run("rejects message above 2 MiB read limit", func(t *testing.T) {
		_, conn := openTransferWebSocketForLimitTest(t)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, transferStatusMessageOfSize(t, 2<<20+1)); err != nil {
			t.Fatalf("write message above 2 MiB: %v", err)
		}

		_, _, err := conn.Read(ctx)
		if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
			t.Fatalf("close status=%v want %v: %v", status, websocket.StatusMessageTooBig, err)
		}
	})
}

func openTransferWebSocketForLimitTest(t *testing.T) (*services.TransferHub, *websocket.Conn) {
	t.Helper()
	db := newTransferTakeoverDB(t)
	hub := services.NewTransferHub(10)
	handler := NewTransferHandler(hub, &config.TransferConfig{}, db, nil, "", "", nil, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.HandleWebSocket(w, r, "robot-001")
	}))
	t.Cleanup(func() {
		server.Close()
		if err := db.Close(); err != nil {
			t.Errorf("close transfer test database: %v", err)
		}
	})

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	conn, _, err := websocket.Dial(
		dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil,
	)
	cancelDial()
	if err != nil {
		t.Fatalf("dial transfer websocket: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.CloseNow(); err != nil {
			t.Errorf("close transfer websocket: %v", err)
		}
	})
	return hub, conn
}

func transferStatusMessageOfSize(t *testing.T, size int) []byte {
	t.Helper()
	const prefix = `{"type":"status","data":{"padding":"`
	const suffix = `"}}`
	paddingSize := size - len(prefix) - len(suffix)
	if paddingSize < 0 {
		t.Fatalf("message size %d is too small", size)
	}
	return []byte(prefix + strings.Repeat("x", paddingSize) + suffix)
}
