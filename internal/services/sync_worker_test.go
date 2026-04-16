// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestStripBucketPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"edge-factory-default/factory-default/device/2024-01-01/task.mcap", "factory-default/device/2024-01-01/task.mcap"},
		{"/edge-factory-default/factory-default/device/2024-01-01/task.mcap", "factory-default/device/2024-01-01/task.mcap"},
		{"bucket/key", "key"},
		{"just-a-file.mcap", "just-a-file.mcap"},
		{"  ", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := stripBucketPrefix(tt.input)
		if got != tt.want {
			t.Errorf("stripBucketPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEnqueueEpisode_DeduplicatesPendingEpisode(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 42); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if err := w.EnqueueEpisode(ctx, 42); !errors.Is(err, ErrEpisodeAlreadyEnqueued) {
		t.Fatalf("second enqueue error = %v, want ErrEpisodeAlreadyEnqueued", err)
	}

	select {
	case got := <-w.enqueueCh:
		if got.episodeID != 42 {
			t.Fatalf("unexpected episode id: got %d want 42", got.episodeID)
		}
		if got.manual {
			t.Fatal("unexpected manual mode for EnqueueEpisode")
		}
	default:
		t.Fatal("expected episode to be enqueued")
	}

	select {
	case got := <-w.enqueueCh:
		t.Fatalf("duplicate enqueue detected: got %d", got.episodeID)
	default:
	}
}

func TestEnqueueEpisode_AllowsReenqueueAfterProcessing(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("initial enqueue failed: %v", err)
	}
	w.unmarkEnqueued(7)
	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("reenqueue failed: %v", err)
	}

	count := 0
	for {
		select {
		case <-w.enqueueCh:
			count++
		default:
			if count != 2 {
				t.Fatalf("expected 2 enqueue records after reenqueue, got %d", count)
			}
			return
		}
	}
}

func TestFindPendingEpisodes_ExcludesExhaustedFailuresFromPollingOnly(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db, cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3}}

	insertEpisodeForSyncWorkerTest(t, db, 1, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 2, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 3, "approved", false)

	insertSyncLogForSyncWorkerTest(t, db, 2, "failed", 3)
	insertSyncLogForSyncWorkerTest(t, db, 3, "failed", 2)

	apiIDs, err := w.findPendingEpisodes(context.Background(), true)
	if err != nil {
		t.Fatalf("api pending query failed: %v", err)
	}
	assertEpisodeIDs(t, apiIDs, []int64{1, 2, 3})

	pollIDs, err := w.findPendingEpisodes(context.Background(), false)
	if err != nil {
		t.Fatalf("poll pending query failed: %v", err)
	}
	assertEpisodeIDs(t, pollIDs, []int64{1, 3})
}

func TestEnqueueEpisodeManual_AllowsExhaustedRetryEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 10, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 10, "failed", 3)

	if err := w.EnqueueEpisodeManual(context.Background(), 10); err != nil {
		t.Fatalf("manual enqueue failed: %v", err)
	}

	select {
	case got := <-w.enqueueCh:
		if got.episodeID != 10 {
			t.Fatalf("unexpected episode id: got %d want 10", got.episodeID)
		}
		if !got.manual {
			t.Fatal("expected manual mode for EnqueueEpisodeManual")
		}
	default:
		t.Fatal("expected episode to be enqueued")
	}
}

func TestEnqueueEpisode_RejectsInProgressEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 11, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 11, "in_progress", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 11); !errors.Is(err, ErrSyncAlreadyInProgress) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncAlreadyInProgress", err)
	}
}

func TestEnqueueEpisode_ReturnsQueueFull(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	if err := w.EnqueueEpisode(context.Background(), 99); !errors.Is(err, ErrSyncQueueFull) {
		t.Fatalf("enqueue error = %v, want ErrSyncQueueFull", err)
	}
}

func TestEnqueueEpisodeManual_ReturnsNotRunningWhenWorkerNotStarted(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	if err := w.EnqueueEpisodeManual(context.Background(), 123); !errors.Is(err, ErrSyncWorkerNotRunning) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncWorkerNotRunning", err)
	}
}

func TestNextRetryDelay_UsesMinuteScaleBackoff(t *testing.T) {
	w := &SyncWorker{
		cfg: SyncWorkerConfig{
			RetryBaseSec:   30,
			RetryMaxSec:    1800,
			RetryJitterSec: 0,
		},
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 30 * time.Second},
		{attempt: 2, want: 60 * time.Second},
		{attempt: 3, want: 120 * time.Second},
		{attempt: 4, want: 240 * time.Second},
		{attempt: 10, want: 1800 * time.Second},
	}

	for _, tt := range tests {
		got := w.nextRetryDelay(tt.attempt)
		if got != tt.want {
			t.Fatalf("nextRetryDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestNextRetryDelay_IncludesBoundedJitter(t *testing.T) {
	w := &SyncWorker{
		cfg: SyncWorkerConfig{
			RetryBaseSec:   30,
			RetryMaxSec:    1800,
			RetryJitterSec: 30,
		},
	}

	got := w.nextRetryDelay(3)
	min := 120 * time.Second
	max := 150 * time.Second
	if got < min || got > max {
		t.Fatalf("nextRetryDelay with jitter = %v, want [%v, %v]", got, min, max)
	}
}

func newTestSyncWorkerDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			qa_status TEXT NOT NULL,
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			deleted_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at TIMESTAMP NULL,
			started_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL
		)`,
	}

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("create schema: %v", err)
		}
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func insertEpisodeForSyncWorkerTest(t *testing.T, db *sqlx.DB, id int64, qaStatus string, cloudSynced bool) {
	t.Helper()

	createdAt := time.Date(2026, 1, int(id), 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, cloud_synced, deleted_at, created_at)
		VALUES (?, ?, ?, NULL, ?)
	`, id, qaStatus, cloudSynced, createdAt); err != nil {
		t.Fatalf("insert episode %d: %v", id, err)
	}
}

func insertSyncLogForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64, status string, attemptCount int) {
	t.Helper()

	startedAt := time.Date(2026, 2, int(episodeID), 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status, attempt_count, started_at)
		VALUES (?, ?, ?, ?)
	`, episodeID, status, attemptCount, startedAt); err != nil {
		t.Fatalf("insert sync log for episode %d: %v", episodeID, err)
	}
}

func assertEpisodeIDs(t *testing.T, got, want []int64) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("unexpected id count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected ids: got %v want %v", got, want)
		}
	}
}
