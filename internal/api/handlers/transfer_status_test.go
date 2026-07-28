// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"sync"
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

	handler.onStatus(context.Background(), dc, map[string]interface{}{
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

func TestTransferHandlerReconcileWaitingACKsFromStatusRecoversAfterReconnect(t *testing.T) {
	db := openTransferDCPlanTestDB(t)
	seedTransferDCPlanTask(t, db)
	if _, err := db.Exec(`UPDATE tasks SET status = 'uploading' WHERE id = 10`); err != nil {
		t.Fatalf("set task uploading: %v", err)
	}

	hub := services.NewTransferHub(10)
	disconnectedServerConn, _ := newRecorderHandlerTestWebSocketPair(t)
	disconnectResult := make(chan error, 1)
	var disconnectOnce sync.Once
	handler := NewTransferHandler(
		hub,
		&config.TransferConfig{WriteTimeout: 1},
		db,
		newTransferDCPlanTestS3(t, func() {
			disconnectOnce.Do(func() {
				disconnectResult <- disconnectedServerConn.CloseNow()
			})
		}),
		"bucket",
		"",
		nil,
		0,
	)
	connectedPayload := waitingACKStatusPayload("task-plan-1", "robot-001/task-plan-1.mcap")
	connectedPayload["type"] = "connected"

	// The initial connection disappears before Keystone can deliver the ACK.
	disconnected := hub.NewTransferConn(disconnectedServerConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", disconnected) {
		t.Fatal("connect initial transfer")
	}
	handler.handleMessage(context.Background(), disconnected, connectedPayload)
	if err := <-disconnectResult; err != nil {
		t.Fatalf("close initial transfer connection: %v", err)
	}

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(*) FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("count episodes after disconnected ACK: %v", err)
	}
	if episodeCount != 1 {
		t.Fatalf("episode count after disconnected ACK=%d want 1", episodeCount)
	}
	var taskStatus string
	if err := db.Get(&taskStatus, `SELECT status FROM tasks WHERE id = 10`); err != nil {
		t.Fatalf("query task after disconnected ACK: %v", err)
	}
	if taskStatus != "uploading" {
		t.Fatalf("task status after disconnected ACK=%q want uploading", taskStatus)
	}

	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	reconnected := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	hub.ConnectReplacingExisting("robot-001", reconnected)
	statusPayload := waitingACKStatusPayload("task-plan-1", "robot-001/task-plan-1.mcap")
	statusPayload["type"] = "status"
	handler.handleMessage(context.Background(), reconnected, statusPayload)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var ack map[string]interface{}
	if err := wsjson.Read(ctx, clientConn, &ack); err != nil {
		t.Fatalf("read replayed upload_ack: %v", err)
	}
	if got := stringVal(ack, "type"); got != "upload_ack" {
		t.Fatalf("message type=%q want upload_ack: %#v", got, ack)
	}
	if got := stringVal(ack, "task_id"); got != "task-plan-1" {
		t.Fatalf("task_id=%q want task-plan-1: %#v", got, ack)
	}
	if err := db.Get(&taskStatus, `SELECT status FROM tasks WHERE id = 10`); err != nil {
		t.Fatalf("query task after replayed ACK: %v", err)
	}
	if taskStatus != "completed" {
		t.Fatalf("task status after replayed ACK=%q want completed", taskStatus)
	}
	if err := db.Get(&episodeCount, `SELECT COUNT(*) FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("count episodes after replayed ACK: %v", err)
	}
	if episodeCount != 1 {
		t.Fatalf("episode count after replayed ACK=%d want 1", episodeCount)
	}
}

func TestTransferHandlerReconcileWaitingACKsFromStatusIsIdempotent(t *testing.T) {
	db := openTransferDCPlanTestDB(t)
	seedTransferDCPlanTask(t, db)
	if _, err := db.Exec(`UPDATE tasks SET status = 'uploading' WHERE id = 10`); err != nil {
		t.Fatalf("set task uploading: %v", err)
	}

	hub := services.NewTransferHub(10)
	serverConn, clientConn := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatal("connect transfer")
	}
	handler := NewTransferHandler(
		hub,
		&config.TransferConfig{WriteTimeout: 1},
		db,
		newTransferDCPlanTestS3(t, nil),
		"bucket",
		"",
		nil,
		0,
	)
	payload := waitingACKStatusPayload("task-plan-1", "robot-001/task-plan-1.mcap")
	payload["type"] = "status"

	for replay := 1; replay <= 2; replay++ {
		handler.handleMessage(context.Background(), dc, payload)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var ack map[string]interface{}
		err := wsjson.Read(ctx, clientConn, &ack)
		cancel()
		if err != nil {
			t.Fatalf("read upload_ack replay %d: %v", replay, err)
		}
		if got := stringVal(ack, "type"); got != "upload_ack" {
			t.Fatalf("replay %d message type=%q want upload_ack: %#v", replay, got, ack)
		}
		if got := stringVal(ack, "task_id"); got != "task-plan-1" {
			t.Fatalf("replay %d task_id=%q want task-plan-1: %#v", replay, got, ack)
		}
	}

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(*) FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("count episodes after repeated status: %v", err)
	}
	if episodeCount != 1 {
		t.Fatalf("episode count after repeated status=%d want 1", episodeCount)
	}
}

func TestTransferHandlerReconcileWaitingACKsFromStatusHonorsCancellation(t *testing.T) {
	db := openTransferDCPlanTestDB(t)
	seedTransferDCPlanTask(t, db)
	if _, err := db.Exec(`UPDATE tasks SET status = 'uploading' WHERE id = 10`); err != nil {
		t.Fatalf("set task uploading: %v", err)
	}

	hub := services.NewTransferHub(10)
	serverConn, _ := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	if !hub.Connect("robot-001", dc) {
		t.Fatal("connect transfer")
	}
	handler := NewTransferHandler(
		hub,
		&config.TransferConfig{WriteTimeout: 1},
		db,
		newTransferDCPlanTestS3(t, nil),
		"bucket",
		"",
		nil,
		0,
	)
	payload := waitingACKStatusPayload("task-plan-1", "robot-001/task-plan-1.mcap")
	payload["type"] = "status"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	handler.handleMessage(ctx, dc, payload)

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(*) FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("count episodes after cancellation: %v", err)
	}
	if episodeCount != 0 {
		t.Fatalf("episode count after cancellation=%d want 0", episodeCount)
	}
	if events := dc.Events(1); len(events) != 0 {
		t.Fatalf("connection events after cancellation=%#v want none", events)
	}
}

func waitingACKStatusPayload(taskID, s3Key string) map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"waiting_ack_count":    float64(1),
			"waiting_ack_task_ids": []interface{}{taskID},
			"uploads": []interface{}{
				map[string]interface{}{
					"task_id": taskID,
					"status":  "uploaded_wait_ack",
					"s3_key":  s3Key,
				},
			},
		},
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

	handler.onStatus(context.Background(), dc, map[string]interface{}{
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

	handler.onStatus(context.Background(), dc, map[string]interface{}{
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

	handler.onStatus(context.Background(), dc, map[string]interface{}{
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
