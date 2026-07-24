// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"
)

func TestUploadCompleteCopiesTaskPlanFieldsToEpisode(t *testing.T) {
	db := openTransferDCPlanTestDB(t)
	seedTransferDCPlanTask(t, db)
	s3Client := newTransferDCPlanTestS3(t, nil)

	hub := services.NewTransferHub(1)
	serverConn, _ := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	handler := NewTransferHandler(hub, &config.TransferConfig{WriteTimeout: 1}, db, s3Client, "bucket", "", nil, 0)
	broker := services.NewDeviceStateBroker()
	handler.SetDeviceStateBroker(broker)
	events, unsubscribe := broker.Subscribe(1)
	defer unsubscribe()

	handler.onUploadComplete(context.Background(), dc, map[string]interface{}{
		"data": map[string]interface{}{
			"task_id": "task-plan-1",
			"s3_key":  "robot-001/task-plan-1.mcap",
		},
	})

	var got struct {
		DCPlanID      sql.NullInt64 `db:"dc_plan_id"`
		LocalDCPlanID sql.NullInt64 `db:"local_dc_plan_id"`
		BatchID       sql.NullInt64 `db:"batch_id"`
		OrderID       sql.NullInt64 `db:"order_id"`
	}
	if err := db.Get(&got, `SELECT dc_plan_id, local_dc_plan_id, batch_id, order_id FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("query created episode: %v", err)
	}
	if !got.DCPlanID.Valid || got.DCPlanID.Int64 != 1001 {
		t.Fatalf("dc_plan_id=%#v want 1001", got.DCPlanID)
	}
	if !got.LocalDCPlanID.Valid || got.LocalDCPlanID.Int64 != 2001 {
		t.Fatalf("local_dc_plan_id=%#v want 2001", got.LocalDCPlanID)
	}
	if got.BatchID.Valid || got.OrderID.Valid {
		t.Fatalf("legacy order/batch fields should be null: batch=%#v order=%#v", got.BatchID, got.OrderID)
	}

	select {
	case event := <-events:
		if event["type"] != "plan_progress_changed" || event["device_id"] != "robot-001" ||
			event["task_id"] != "task-plan-1" || event["dc_plan_id"] != int64(1001) ||
			event["qa_status"] != qaStatusPendingQA || event["reason"] != "episode_created" {
			t.Fatalf("unexpected episode progress event: %#v", event)
		}
	default:
		t.Fatal("upload completion did not publish an episode progress event")
	}
}

func TestUploadCompleteDoesNotCreateEpisodeWhenTaskIsCancelledDuringVerification(t *testing.T) {
	db := openTransferDCPlanTestDB(t)
	seedTransferDCPlanTask(t, db)
	if _, err := db.Exec(`UPDATE tasks SET status = 'uploading' WHERE id = 10`); err != nil {
		t.Fatalf("set task uploading: %v", err)
	}
	s3Client := newTransferDCPlanTestS3(t, func() {
		if _, err := db.Exec(`UPDATE tasks SET status = 'cancelled' WHERE id = 10`); err != nil {
			t.Errorf("cancel task during S3 verification: %v", err)
		}
	})

	hub := services.NewTransferHub(1)
	serverConn, _ := newRecorderHandlerTestWebSocketPair(t)
	dc := hub.NewTransferConn(serverConn, "robot-001", "127.0.0.1")
	handler := NewTransferHandler(hub, &config.TransferConfig{WriteTimeout: 1}, db, s3Client, "bucket", "", nil, 0)

	handler.onUploadComplete(context.Background(), dc, map[string]interface{}{
		"data": map[string]interface{}{
			"task_id": "task-plan-1",
			"s3_key":  "robot-001/task-plan-1.mcap",
		},
	})

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(*) FROM episodes WHERE task_id = 10`); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 0 {
		t.Fatalf("episode count=%d want 0", episodeCount)
	}
	var status string
	if err := db.Get(&status, `SELECT status FROM tasks WHERE id = 10`); err != nil {
		t.Fatalf("query task status: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("task status=%q want cancelled", status)
	}
}

func openTransferDCPlanTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			asset_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			status TEXT NOT NULL,
			batch_id INTEGER,
			order_id INTEGER,
			workstation_id INTEGER,
			organization_id INTEGER,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			completed_at TIMESTAMP NULL,
			error_message TEXT,
			updated_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			batch_id INTEGER,
			order_id INTEGER,
			workstation_id INTEGER,
			organization_id INTEGER,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			mcap_path TEXT NOT NULL,
			sidecar_path TEXT NOT NULL,
			duration_sec REAL,
			file_size_bytes INTEGER,
			checksum TEXT,
			qa_status TEXT,
			metadata TEXT,
			updated_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create transfer dc plan schema: %v", err)
		}
	}
	return db
}

func seedTransferDCPlanTask(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO robots (id, device_id, asset_id, deleted_at) VALUES (1, 'robot-001', 'asset-1', NULL)`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id, deleted_at) VALUES (1, 1, NULL)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (
			id, task_id, status, batch_id, order_id,
			workstation_id, organization_id,
			dc_plan_id, local_dc_plan_id, deleted_at
		) VALUES (10, 'task-plan-1', 'completed', 20, 30, 1, 60, 1001, 2001, NULL)
	`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func newTransferDCPlanTestS3(t *testing.T, onMCAPHead func()) *s3.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onMCAPHead != nil && r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, ".mcap") {
			onMCAPHead()
		}
		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Last-Modified", "Mon, 13 Jul 2026 05:00:00 GMT")
		if _, ok := r.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"recording":{"duration_sec":1,"file_size_bytes":2,"checksum_sha256":"abc"}}`))
			return
		}
		if strings.Count(strings.Trim(r.URL.Path, "/"), "/") > 0 {
			w.Header().Set("Content-Length", "2")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client, err := s3.Connect(&s3.Config{
		Endpoint:     strings.TrimPrefix(server.URL, "http://"),
		AccessKey:    "access",
		SecretKey:    "secret",
		Bucket:       "bucket",
		UseSSL:       false,
		EnsureBucket: true,
	})
	if err != nil {
		t.Fatalf("connect test s3: %v", err)
	}
	return client
}
