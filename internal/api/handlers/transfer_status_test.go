// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestTransferStatusCachesUploadRecords(t *testing.T) {
	hub := services.NewTransferHub(10)
	dc := hub.NewTransferConn(nil, "robot-001", "127.0.0.1")
	handler := NewTransferHandler(hub, &config.TransferConfig{}, nil, nil, "", "", nil, 0)

	handler.onStatus(dc, map[string]interface{}{
		"type": "status",
		"data": map[string]interface{}{
			"waiting_ack_task_ids": []interface{}{"task-uploaded"},
			"uploads": []interface{}{
				map[string]interface{}{
					"task_id":           "task-uploaded",
					"status":            "uploaded_wait_ack",
					"s3_key":            "factory/robot/task-uploaded.mcap",
					"object_key":        "factory/robot/task-uploaded.mcap",
					"file_size_bytes":   float64(1234),
					"checksum_sha256":   "abc123",
					"bytes_uploaded":    float64(1234),
					"upload_mode":       "mcap_json",
					"retry_count":       float64(2),
					"next_retry_at":     "2026-06-16T00:00:00Z",
					"last_error":        "previous failure",
					"created_at":        "2026-06-15T00:00:00Z",
					"updated_at":        "2026-06-16T00:00:00Z",
					"completed_at":      "2026-06-16T00:01:00Z",
					"delete_last_error": "cleanup pending",
				},
			},
		},
	})

	status := dc.GetStatus()
	if len(status.Uploads) != 1 {
		t.Fatalf("uploads len=%d want=1: %#v", len(status.Uploads), status.Uploads)
	}
	got := status.Uploads[0]
	if got.TaskID != "task-uploaded" || got.Status != "uploaded_wait_ack" {
		t.Fatalf("upload identity/status=%#v", got)
	}
	if got.S3Key != "factory/robot/task-uploaded.mcap" || got.ObjectKey != "factory/robot/task-uploaded.mcap" {
		t.Fatalf("upload object keys=%#v", got)
	}
	if got.FileSizeBytes != 1234 || got.ChecksumSHA256 != "abc123" || got.BytesUploaded != 1234 {
		t.Fatalf("upload file metadata=%#v", got)
	}
	if got.RetryCount != 2 || got.NextRetryAt == "" || got.LastError == "" || got.DeleteLastError == "" {
		t.Fatalf("upload retry metadata=%#v", got)
	}
}

func TestTransferStatusRequeuesUploadRequestAfterPreviousSendFailure(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-requeue", "uploading")
	if _, err := db.Exec(
		`UPDATE tasks SET error_message = ? WHERE task_id = ?`,
		"transfer disconnected; upload_request not sent",
		"task-requeue",
	); err != nil {
		t.Fatalf("seed upload_request error: %v", err)
	}

	hub := services.NewTransferHub(10)
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatalf("connect transfer failed")
	}
	handler := NewTransferHandler(hub, &config.TransferConfig{WriteTimeout: 1}, db, nil, "", "", nil, 0)

	handler.onStatus(dc, map[string]interface{}{
		"type": "status",
		"data": map[string]interface{}{
			"uploads": []interface{}{},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var msg map[string]interface{}
	if err := wsjson.Read(ctx, clientConn, &msg); err != nil {
		t.Fatalf("read requeued upload_request: %v", err)
	}
	if got := stringVal(msg, "type"); got != "upload_request" {
		t.Fatalf("message type=%q want upload_request: %#v", got, msg)
	}
	if got := stringVal(msg, "task_id"); got != "task-requeue" {
		t.Fatalf("task_id=%q want task-requeue: %#v", got, msg)
	}
}

func TestTransferStatusDoesNotRequeueUploadNotFound(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-not-found", "uploading")
	if _, err := db.Exec(
		`UPDATE tasks SET error_message = ? WHERE task_id = ?`,
		"No MCAP file matching task-not-found in /data",
		"task-not-found",
	); err != nil {
		t.Fatalf("seed upload_not_found error: %v", err)
	}

	hub := services.NewTransferHub(10)
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatalf("connect transfer failed")
	}
	handler := NewTransferHandler(hub, &config.TransferConfig{WriteTimeout: 1}, db, nil, "", "", nil, 0)

	handler.onStatus(dc, map[string]interface{}{
		"type": "status",
		"data": map[string]interface{}{
			"uploads": []interface{}{},
		},
	})

	assertNoTransferMessage(t, clientConn)
}

func TestTransferStatusDoesNotRequeueWhenUploadAlreadyActive(t *testing.T) {
	db := newTaskStateRecoveryDB(t)
	defer db.Close()
	seedTaskStateRecoveryTask(t, db, "task-active", "uploading")
	if _, err := db.Exec(
		`UPDATE tasks SET error_message = ? WHERE task_id = ?`,
		"upload_request failed: transfer write timeout",
		"task-active",
	); err != nil {
		t.Fatalf("seed upload_request error: %v", err)
	}

	hub := services.NewTransferHub(10)
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatalf("connect transfer failed")
	}
	handler := NewTransferHandler(hub, &config.TransferConfig{WriteTimeout: 1}, db, nil, "", "", nil, 0)

	handler.onStatus(dc, map[string]interface{}{
		"type": "status",
		"data": map[string]interface{}{
			"uploads": []interface{}{
				map[string]interface{}{
					"task_id": "task-active",
					"status":  "active",
				},
			},
		},
	})

	assertNoTransferMessage(t, clientConn)
}

func assertNoTransferMessage(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var msg map[string]interface{}
	if err := wsjson.Read(ctx, conn, &msg); err == nil {
		t.Fatalf("unexpected transfer message: %#v", msg)
	}
}
