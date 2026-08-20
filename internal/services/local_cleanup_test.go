// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type cleanupStoreFake struct {
	deletions []cleanupDelete
	err       error
}

type cleanupDelete struct {
	bucket string
	key    string
}

func (s *cleanupStoreFake) DeleteObject(_ context.Context, bucket, objectKey string) error {
	s.deletions = append(s.deletions, cleanupDelete{bucket: bucket, key: objectKey})
	return s.err
}

func TestLocalCleanupServiceDeletesCompletedMinIOOriginal(t *testing.T) {
	db := setupLocalCleanupTestDB(t)
	_, err := db.Exec(`INSERT INTO episodes (id, cloud_synced, local_storage_status) VALUES (1, TRUE, 'available')`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sync_logs (episode_id, status, source_snapshot) VALUES (?, 'completed', ?)`, 1, localCleanupSnapshotJSON())
	if err != nil {
		t.Fatalf("insert sync log: %v", err)
	}
	store := &cleanupStoreFake{}
	service := NewLocalCleanupService(db, store, "edge-data")

	result, err := service.CleanupEpisode(context.Background(), 1, "admin")
	if err != nil {
		t.Fatalf("CleanupEpisode() error: %v", err)
	}
	if result.Status != "deleted" || len(store.deletions) != 1 || store.deletions[0].bucket != "edge-data" || store.deletions[0].key != "raw/episode-1.mcap" {
		t.Fatalf("result=%+v deletions=%+v", result, store.deletions)
	}
	var status string
	if err := db.Get(&status, `SELECT local_storage_status FROM episodes WHERE id = 1`); err != nil {
		t.Fatalf("load episode status: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("local storage status = %q, want deleted", status)
	}
}

func TestLocalCleanupServiceDeletesOriginalAndDepthNormalization(t *testing.T) {
	db := setupLocalCleanupTestDB(t)
	if _, err := db.Exec(`INSERT INTO episodes (id, cloud_synced, local_storage_status) VALUES (1, TRUE, 'available')`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO episode_derivatives (id, episode_id, kind, generation, source_uri, mcap_path, processing_status, qa_status) VALUES (7, 1, 'depth_normalization', 2, 'minio://edge-data/raw/episode-1.mcap', 'derived/episode-1-depth.mcap', 'succeeded', 'approved')`); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	snapshot := `{"source_type":"depth_normalization","backend":"minio","bucket":"edge-data","object_key":"derived/episode-1-depth.mcap","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bag_name":"episode-1-depth.mcap","derivative_id":7,"generation":2}`
	if _, err := db.Exec(`INSERT INTO sync_logs (episode_id, status, source_snapshot) VALUES (?, 'completed', ?)`, 1, snapshot); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}
	store := &cleanupStoreFake{}
	_, err := NewLocalCleanupService(db, store, "edge-data").CleanupEpisode(context.Background(), 1, "admin")
	if err != nil {
		t.Fatalf("CleanupEpisode() error: %v", err)
	}
	if len(store.deletions) != 2 || store.deletions[0].key != "raw/episode-1.mcap" || store.deletions[1].key != "derived/episode-1-depth.mcap" {
		t.Fatalf("deletions=%+v", store.deletions)
	}
}

func TestLocalCleanupServiceRejectsUnsyncedEpisode(t *testing.T) {
	db := setupLocalCleanupTestDB(t)
	if _, err := db.Exec(`INSERT INTO episodes (id, cloud_synced, local_storage_status) VALUES (1, FALSE, 'available')`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	_, err := NewLocalCleanupService(db, &cleanupStoreFake{}, "edge-data").CleanupEpisode(context.Background(), 1, "admin")
	if !errors.Is(err, ErrLocalCleanupNotSynced) {
		t.Fatalf("CleanupEpisode() error = %v, want ErrLocalCleanupNotSynced", err)
	}
}

func setupLocalCleanupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE episodes (id INTEGER PRIMARY KEY, cloud_synced BOOLEAN NOT NULL, local_storage_status TEXT NOT NULL, local_storage_deleted_at TIMESTAMP NULL, local_storage_delete_error TEXT NULL, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE episode_derivatives (id INTEGER PRIMARY KEY, episode_id INTEGER NOT NULL, kind TEXT NOT NULL, generation INTEGER NOT NULL, source_uri TEXT, mcap_path TEXT, processing_status TEXT, qa_status TEXT)`,
		`CREATE TABLE local_cleanup_job_objects (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL, bucket TEXT NOT NULL, object_key TEXT NOT NULL, status TEXT NOT NULL, error_message TEXT NULL, UNIQUE(job_id, bucket, object_key))`,
		`CREATE TABLE local_cleanup_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL UNIQUE, bucket TEXT NOT NULL, object_key TEXT NOT NULL, status TEXT NOT NULL, requested_by TEXT NULL, requested_at TIMESTAMP NOT NULL, started_at TIMESTAMP NULL, completed_at TIMESTAMP NULL, retry_count INTEGER NOT NULL, error_message TEXT NULL)`,
		`CREATE TABLE sync_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL, status TEXT NOT NULL, source_snapshot TEXT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func localCleanupSnapshotJSON() string {
	return `{"source_type":"original","backend":"minio","bucket":"edge-data","object_key":"raw/episode-1.mcap","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bag_name":"episode-1.mcap"}`
}
