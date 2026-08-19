// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package depthnorm

import (
	"context"
	"errors"
	"testing"

	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestStartCreatesQueuedDerivative(t *testing.T) {
	db := newDepthNormTestDB(t)
	defer db.Close()
	seedDepthNormEpisode(t, db, 1, "episode-1", "bucket/object.mcap", "ZJ-WA1-D")

	manager := NewManager(db, &s3.Client{}, "bucket", Config{Enabled: true, Script: "script"})
	derivative, started, err := manager.Start(context.Background(), 1, "auto-sync")
	if err != nil || !started {
		t.Fatalf("Start() = %+v, %t, %v; want created, true, nil", derivative, started, err)
	}
	if derivative.EpisodeID != 1 || derivative.Generation != 1 || derivative.ProcessingStatus != statusQueued {
		t.Fatalf("derivative = %+v", derivative)
	}
	var sourceURI string
	if err := db.Get(&sourceURI, `SELECT source_uri FROM episode_derivatives WHERE id=?`, derivative.ID); err != nil {
		t.Fatalf("load source URI: %v", err)
	}
	if sourceURI != "minio://bucket/object.mcap" {
		t.Fatalf("source URI = %q", sourceURI)
	}
}

func TestStartRejectsActiveAndRetriesFailed(t *testing.T) {
	db := newDepthNormTestDB(t)
	defer db.Close()
	seedDepthNormEpisode(t, db, 2, "episode-2", "bucket/object.mcap", "ZJ-WA1-D")

	manager := NewManager(db, &s3.Client{}, "bucket", Config{Enabled: true, Script: "script"})
	first, started, err := manager.Start(context.Background(), 2, "auto-sync")
	if err != nil || !started {
		t.Fatalf("first Start() = %+v, %t, %v", first, started, err)
	}
	if _, _, err := manager.Start(context.Background(), 2, "auto-sync"); !errors.Is(err, ErrProcessingActive) {
		t.Fatalf("active Start() error = %v, want %v", err, ErrProcessingActive)
	}
	if _, err := db.Exec(`UPDATE episode_derivatives SET processing_status='failed' WHERE id=?`, first.ID); err != nil {
		t.Fatalf("seed failed derivative: %v", err)
	}
	retry, started, err := manager.Start(context.Background(), 2, "data-ops")
	if err != nil || !started {
		t.Fatalf("retry Start() = %+v, %t, %v", retry, started, err)
	}
	if retry.ID != first.ID || retry.Generation != 2 || retry.ProcessingStatus != statusQueued {
		t.Fatalf("retry derivative = %+v", retry)
	}
}

func TestStartRejectsOtherDeviceAndLockedSource(t *testing.T) {
	db := newDepthNormTestDB(t)
	defer db.Close()
	seedDepthNormEpisode(t, db, 3, "episode-3", "bucket/object.mcap", "Ego Portal Lite")
	manager := NewManager(db, &s3.Client{}, "bucket", Config{Enabled: true, Script: "script"})
	if _, _, err := manager.Start(context.Background(), 3, "auto-sync"); err == nil {
		t.Fatal("Start() error = nil, want device rejection")
	}

	seedDepthNormEpisode(t, db, 4, "episode-4", "bucket/object.mcap", "ZJ-WA1-D")
	if _, err := db.Exec(`UPDATE episodes SET cloud_publish_source='original' WHERE id=4`); err != nil {
		t.Fatalf("lock source: %v", err)
	}
	if _, _, err := manager.Start(context.Background(), 4, "auto-sync"); !errors.Is(err, ErrCloudSourceLocked) {
		t.Fatalf("locked Start() error = %v, want %v", err, ErrCloudSourceLocked)
	}
}

func newDepthNormTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			mcap_path TEXT,
			storage_backend TEXT,
			checksum TEXT,
			file_size_bytes INTEGER,
			cloud_publish_source TEXT,
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			metadata TEXT,
			auto_sync_device_type TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE episode_derivatives (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			generation INTEGER NOT NULL DEFAULT 1,
			source_uri TEXT,
			source_checksum TEXT,
			source_size_bytes INTEGER,
			processing_status TEXT NOT NULL DEFAULT 'queued',
			qa_status TEXT NOT NULL DEFAULT 'not_started',
			mcap_path TEXT,
			checksum TEXT,
			file_size_bytes INTEGER,
			processing_error TEXT,
			processing_result TEXT,
			processing_started_at TIMESTAMP,
			processing_finished_at TIMESTAMP,
			created_by TEXT,
			updated_by TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func seedDepthNormEpisode(t *testing.T, db *sqlx.DB, id int64, episodeID, mcapPath, deviceType string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, mcap_path, checksum, file_size_bytes,
			auto_sync_device_type
		) VALUES (?, ?, ?, ?, ?, ?)
	`, id, episodeID, mcapPath, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 100, deviceType); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
}
