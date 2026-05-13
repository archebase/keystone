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
	insertEpisodeForSyncWorkerTest(t, db, 4, "approved", false)

	insertSyncLogForSyncWorkerTest(t, db, 2, "failed", 3)
	insertSyncLogForSyncWorkerTest(t, db, 3, "failed", 2)
	insertSyncLogForSyncWorkerTest(t, db, 4, "pending", 1)

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

	latest := latestSyncLogForSyncWorkerTest(t, db, 10)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 0 {
		t.Fatalf("latest attempt_count = %d, want 0 for fresh manual chain", latest.AttemptCount)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 10); count != 2 {
		t.Fatalf("sync log count = %d, want failed history plus fresh pending", count)
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

func TestEnqueueEpisodeManual_PromotesDueFailureToPending(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 13, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 13, "failed", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 13); err != nil {
		t.Fatalf("manual enqueue failed: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 13)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want completed attempt count 1", latest.AttemptCount)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 13); count != 1 {
		t.Fatalf("sync log count = %d, want reused failed row", count)
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

func TestEnqueueEpisodeManual_PersistsPendingWhenMemoryQueueFull(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 14, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 14, "failed", 3)

	if err := w.EnqueueEpisodeManual(context.Background(), 14); err != nil {
		t.Fatalf("manual enqueue failed despite durable pending: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 14)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if !w.tryMarkEnqueued(14) {
		t.Fatal("episode marker remained set after enqueue channel was full")
	}
	w.unmarkEnqueued(14)
}

func TestEnqueueEpisodeManual_RejectsPendingEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 12, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 12, "pending", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 12); !errors.Is(err, ErrSyncAlreadyInProgress) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncAlreadyInProgress", err)
	}
}

func TestEnqueuePendingEpisodes_PersistsPendingWhenMemoryQueueFull(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 15, "approved", false)

	count, err := w.EnqueuePendingEpisodes(context.Background())
	if err != nil {
		t.Fatalf("enqueue pending episodes failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("enqueued count = %d, want 1", count)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 15)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
}

func TestDispatchPendingSyncLogs_DispatchesPersistedRows(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 16, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 16, "pending", 0)

	w.dispatchPendingSyncLogs(context.Background())

	select {
	case got := <-w.jobCh:
		if got.episodeID != 16 {
			t.Fatalf("unexpected episode id: got %d want 16", got.episodeID)
		}
		if got.manual {
			t.Fatal("unexpected manual mode for recovered pending row")
		}
	default:
		t.Fatal("expected persisted pending row to be dispatched")
	}
}

func TestRetryFailedEpisodes_PromotesDueFailureToPendingBeforeDispatch(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 17, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 17, "failed", 1)

	w.retryFailedEpisodes(context.Background())

	latest := latestSyncLogForSyncWorkerTest(t, db, 17)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want completed attempt count 1", latest.AttemptCount)
	}
	select {
	case got := <-w.jobCh:
		if got.episodeID != 17 {
			t.Fatalf("unexpected episode id: got %d want 17", got.episodeID)
		}
	default:
		t.Fatal("expected retryable failure to be dispatched")
	}
}

func TestAcquireSyncLogWithMode_ClaimsFreshPendingRow(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:  db,
		cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
	}

	insertEpisodeForSyncWorkerTest(t, db, 18, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 18, "pending", 0)

	syncLogID, attemptCount, err := w.acquireSyncLogWithMode(context.Background(), 18, "local/episode.mcap", false)
	if err != nil {
		t.Fatalf("claim pending sync log failed: %v", err)
	}
	if syncLogID <= 0 {
		t.Fatalf("syncLogID = %d, want positive id", syncLogID)
	}
	if attemptCount != 1 {
		t.Fatalf("attemptCount = %d, want 1", attemptCount)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 18)
	if latest.Status != "in_progress" {
		t.Fatalf("latest status = %q, want in_progress", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want 1", latest.AttemptCount)
	}
}

func TestAcquireSyncLogWithMode_ClaimsRetryPendingRow(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:  db,
		cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
	}

	insertEpisodeForSyncWorkerTest(t, db, 19, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 19, "pending", 1)

	_, attemptCount, err := w.acquireSyncLogWithMode(context.Background(), 19, "local/episode.mcap", false)
	if err != nil {
		t.Fatalf("claim retry pending sync log failed: %v", err)
	}
	if attemptCount != 2 {
		t.Fatalf("attemptCount = %d, want retry attempt 2", attemptCount)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 19)
	if latest.Status != "in_progress" {
		t.Fatalf("latest status = %q, want in_progress", latest.Status)
	}
	if latest.AttemptCount != 2 {
		t.Fatalf("latest attempt_count = %d, want 2", latest.AttemptCount)
	}
}

func TestProcessEnqueuedEpisode_HoldsMarkerUntilProcessingReturns(t *testing.T) {
	w := &SyncWorker{
		enqueuedEpisode: map[int64]struct{}{77: {}},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		w.processEnqueuedEpisodeWith(
			context.Background(),
			syncEnqueueRequest{episodeID: 77, manual: true},
			func(context.Context, int64, bool) {
				close(started)
				<-release
			},
		)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("processing did not start")
	}

	if w.tryMarkEnqueued(77) {
		t.Fatal("episode marker was released while processing was still active")
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processing did not finish")
	}

	if !w.tryMarkEnqueued(77) {
		t.Fatal("episode marker was not released after processing finished")
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
				source_path TEXT,
				status TEXT NOT NULL,
				duration_sec INTEGER,
				error_message TEXT,
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

type syncLogForSyncWorkerTest struct {
	Status       string `db:"status"`
	AttemptCount int    `db:"attempt_count"`
}

func latestSyncLogForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64) syncLogForSyncWorkerTest {
	t.Helper()

	var row syncLogForSyncWorkerTest
	if err := db.Get(&row, `
		SELECT status, attempt_count
		FROM sync_logs
		WHERE episode_id = ?
		ORDER BY id DESC
		LIMIT 1
	`, episodeID); err != nil {
		t.Fatalf("query latest sync log for episode %d: %v", episodeID, err)
	}
	return row
}

func countSyncLogsForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64) int {
	t.Helper()

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil {
		t.Fatalf("count sync logs for episode %d: %v", episodeID, err)
	}
	return count
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
