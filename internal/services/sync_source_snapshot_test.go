// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"archebase.com/keystone-edge/internal/cloud"
)

func TestEnqueueEpisodeManual_SelectsSuccessfulApprovedStereoSplit(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 40, "approved", false)
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, qa_status,
			mcap_path, checksum, file_size_bytes
		) VALUES (40, 'stereo_split', 1, 'succeeded', 'approved',
		          'derived/40/output_bag.mcap', ?, 200)
	`, testSyncSHA256); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.SetStereoSplitSourceBucket("derivative-bucket")
	worker.running.Store(true)

	if err := worker.EnqueueEpisodeManual(context.Background(), 40); err != nil {
		t.Fatalf("EnqueueEpisodeManual() error=%v", err)
	}
	var rawSnapshot string
	if err := db.Get(&rawSnapshot, "SELECT source_snapshot FROM sync_logs WHERE episode_id = 40"); err != nil {
		t.Fatalf("load source snapshot: %v", err)
	}
	snapshot, err := decodeSyncSourceSnapshot(rawSnapshot)
	if err != nil {
		t.Fatalf("decode source snapshot: %v", err)
	}
	if snapshot.SourceType != SyncSourceStereoSplit || snapshot.Bucket != "derivative-bucket" ||
		snapshot.ObjectKey != "derived/40/output_bag.mcap" {
		t.Fatalf("source snapshot=%+v, want approved stereo split", snapshot)
	}
}

func TestEnqueueDepthNormalizationAutomaticUsesApprovedDerivative(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 43, "approved", false)
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_device_type='ZJ-WA1-D' WHERE id=43`); err != nil {
		t.Fatalf("seed device type: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, qa_status,
			mcap_path, checksum, file_size_bytes
		) VALUES (43, 'depth_normalization', 1, 'succeeded', 'approved',
		          'depth-normalized/episode-43/generation-1/source.mcap', ?, 200)
	`, testSyncSHA256); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	if err := worker.EnqueueDepthNormalizationAutomatic(context.Background(), 43); err != nil {
		t.Fatalf("EnqueueDepthNormalizationAutomatic() error=%v", err)
	}
	var rawSnapshot string
	if err := db.Get(&rawSnapshot, "SELECT source_snapshot FROM sync_logs WHERE episode_id=43"); err != nil {
		t.Fatalf("load source snapshot: %v", err)
	}
	snapshot, err := decodeSyncSourceSnapshot(rawSnapshot)
	if err != nil {
		t.Fatalf("decode source snapshot: %v", err)
	}
	if snapshot.SourceType != SyncSourceDepthNormalization || snapshot.Backend != SyncBackendMinIO ||
		snapshot.Bucket != "test-bucket" || snapshot.Generation != 1 {
		t.Fatalf("source snapshot=%+v, want approved depth normalization", snapshot)
	}
}

func TestEnqueueEpisodeManualBlocksZJWA1DWithoutReadyDerivative(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 44, "approved", false)
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_device_type='ZJ-WA1-D' WHERE id=44`); err != nil {
		t.Fatalf("seed device type: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	if err := worker.EnqueueEpisodeManual(context.Background(), 44); !errors.Is(err, ErrSyncSourceUnavailable) {
		t.Fatalf("EnqueueEpisodeManual() error=%v, want %v", err, ErrSyncSourceUnavailable)
	}
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM sync_logs WHERE episode_id=44"); err != nil || count != 0 {
		t.Fatalf("sync logs count=%d err=%v, want none", count, err)
	}
}

func TestEnqueueEpisodeManualUsesZJWA1DAlreadyCompressedOriginal(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 45, "approved", false)
	metadata := `{"depth_normalization":{"required":false,"reason":"already_compresseddepth"}}`
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_device_type='ZJ-WA1-D', metadata=? WHERE id=45`, metadata); err != nil {
		t.Fatalf("seed already-compressed episode: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	if err := worker.EnqueueEpisodeManual(context.Background(), 45); err != nil {
		t.Fatalf("EnqueueEpisodeManual() error=%v", err)
	}
	var rawSnapshot string
	if err := db.Get(&rawSnapshot, "SELECT source_snapshot FROM sync_logs WHERE episode_id=45"); err != nil {
		t.Fatalf("load source snapshot: %v", err)
	}
	snapshot, err := decodeSyncSourceSnapshot(rawSnapshot)
	if err != nil {
		t.Fatalf("decode source snapshot: %v", err)
	}
	if snapshot.SourceType != SyncSourceOriginal {
		t.Fatalf("source type=%q, want original", snapshot.SourceType)
	}
}

func TestEnqueueOriginalAutomaticPinsOriginalSource(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 39, "approved", false)
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	if err := worker.EnqueueOriginalAutomatic(context.Background(), 39); err != nil {
		t.Fatalf("EnqueueOriginalAutomatic() error=%v", err)
	}
	var rawSnapshot string
	if err := db.Get(&rawSnapshot, "SELECT source_snapshot FROM sync_logs WHERE episode_id = 39"); err != nil {
		t.Fatalf("load source snapshot: %v", err)
	}
	snapshot, err := decodeSyncSourceSnapshot(rawSnapshot)
	if err != nil {
		t.Fatalf("decode source snapshot: %v", err)
	}
	if snapshot.SourceType != SyncSourceOriginal {
		t.Fatalf("source type=%q, want original", snapshot.SourceType)
	}
}

func TestEnqueueEpisodeManual_WaitsForUnavailableStereoSplit(t *testing.T) {
	tests := []struct {
		name             string
		processingStatus string
		qaStatus         string
	}{
		{name: "processing", processingStatus: "running", qaStatus: "not_started"},
		{name: "processing_failed", processingStatus: "failed", qaStatus: "not_started"},
		{name: "qa_not_approved", processingStatus: "succeeded", qaStatus: "rejected"},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newTestSyncWorkerDB(t)
			episodeID := int64(43 + index)
			insertEpisodeForSyncWorkerTest(t, db, episodeID, "approved", false)
			if _, err := db.Exec(`
				INSERT INTO episode_derivatives (
					episode_id, kind, generation, processing_status, qa_status,
					mcap_path, checksum, file_size_bytes
				) VALUES (?, 'stereo_split', 1, ?, ?, NULL, NULL, NULL)
			`, episodeID, test.processingStatus, test.qaStatus); err != nil {
				t.Fatalf("insert derivative: %v", err)
			}
			worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
			worker.running.Store(true)

			err := worker.EnqueueEpisodeManual(context.Background(), episodeID)
			if !errors.Is(err, ErrSyncSourceUnavailable) {
				t.Fatalf("EnqueueEpisodeManual() error=%v want ErrSyncSourceUnavailable", err)
			}
			var logs int
			if err := db.Get(&logs, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil || logs != 0 {
				t.Fatalf("sync log count=%d error=%v want 0", logs, err)
			}
		})
	}
}

func TestEnqueueEpisodeManual_ReusesClaimedOriginalSource(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 44, "approved", false)
	if _, err := db.Exec("UPDATE episodes SET cloud_publish_source = 'original' WHERE id = 44"); err != nil {
		t.Fatalf("claim original source: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, qa_status,
			mcap_path, checksum, file_size_bytes
		) VALUES (44, 'stereo_split', 1, 'succeeded', 'approved',
		          'derived/44/output_bag.mcap', ?, 200)
	`, testSyncSHA256); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	if err := worker.EnqueueEpisodeManual(context.Background(), 44); err != nil {
		t.Fatalf("EnqueueEpisodeManual() error=%v", err)
	}
	var rawSnapshot string
	if err := db.Get(&rawSnapshot, "SELECT source_snapshot FROM sync_logs WHERE episode_id = 44"); err != nil {
		t.Fatalf("load source snapshot: %v", err)
	}
	snapshot, err := decodeSyncSourceSnapshot(rawSnapshot)
	if err != nil {
		t.Fatalf("decode source snapshot: %v", err)
	}
	if snapshot.SourceType != SyncSourceOriginal {
		t.Fatalf("source type=%q, want original", snapshot.SourceType)
	}
}

func TestEnqueueEpisodeManual_WaitsForMissingClaimedStereoSplit(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 47, "approved", false)
	if _, err := db.Exec("UPDATE episodes SET cloud_publish_source = 'stereo_split' WHERE id = 47"); err != nil {
		t.Fatalf("claim stereo split source: %v", err)
	}
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.running.Store(true)

	err := worker.EnqueueEpisodeManual(context.Background(), 47)
	if !errors.Is(err, ErrSyncSourceUnavailable) {
		t.Fatalf("EnqueueEpisodeManual() error=%v want ErrSyncSourceUnavailable", err)
	}
}

func TestStereoSplitSyncRequiresSucceededApprovedDerivative(t *testing.T) {
	tests := []struct {
		name             string
		processingStatus string
		qaStatus         string
	}{
		{name: "processing_not_succeeded", processingStatus: "running", qaStatus: "pending"},
		{name: "qa_not_approved", processingStatus: "succeeded", qaStatus: "rejected"},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newTestSyncWorkerDB(t)
			episodeID := int64(50 + index)
			insertEpisodeForSyncWorkerTest(t, db, episodeID, "failed", false)
			if _, err := db.Exec(`
				INSERT INTO episode_derivatives (
					episode_id, kind, generation, processing_status, qa_status,
					mcap_path, checksum, file_size_bytes
				) VALUES (?, 'stereo_split', 1, ?, ?,
				          'derived/output_bag.mcap', ?, 200)
			`, episodeID, test.processingStatus, test.qaStatus, testSyncSHA256); err != nil {
				t.Fatalf("insert derivative: %v", err)
			}

			worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
			worker.SetStereoSplitSourceBucket("derivative-bucket")
			worker.running.Store(true)

			err := worker.EnqueueStereoSplitManual(context.Background(), episodeID)
			if err == nil || err.Error() != "stereo split derivative must be succeeded and QA approved" {
				t.Fatalf("EnqueueStereoSplitManual() error=%v want eligibility error", err)
			}
			var logs int
			if err := db.Get(&logs, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil || logs != 0 {
				t.Fatalf("sync log count=%d error=%v want 0", logs, err)
			}
			var claimed int
			if err := db.Get(&claimed, `
				SELECT COUNT(*) FROM episodes
				WHERE id = ? AND cloud_publish_source IS NOT NULL
			`, episodeID); err != nil || claimed != 0 {
				t.Fatalf("claimed source count=%d error=%v want 0", claimed, err)
			}
		})
	}
}

func TestStereoSplitSyncClaimsAndReusesFrozenSnapshot(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 41, "failed", false)
	result, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, qa_status,
			mcap_path, checksum, file_size_bytes
		) VALUES (41, 'stereo_split', 2, 'succeeded', 'approved',
		          'derived/41/g2/output_bag.mcap', ?, 200)
	`, testSyncSHA256)
	if err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	derivativeID, _ := result.LastInsertId()
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.SetStereoSplitSourceBucket("derivative-bucket")
	worker.running.Store(true)

	if err := worker.EnqueueStereoSplitManual(context.Background(), 41); err != nil {
		t.Fatalf("EnqueueStereoSplitManual() error=%v", err)
	}
	var firstRaw string
	if err := db.Get(&firstRaw, "SELECT source_snapshot FROM sync_logs WHERE episode_id = 41"); err != nil {
		t.Fatalf("load first snapshot: %v", err)
	}
	first, err := decodeSyncSourceSnapshot(firstRaw)
	if err != nil {
		t.Fatalf("decode first snapshot: %v", err)
	}
	if first.SourceType != SyncSourceStereoSplit || first.Bucket != "derivative-bucket" ||
		first.ObjectKey != "derived/41/g2/output_bag.mcap" || first.BagName != "episode-41.mcap" ||
		first.DerivativeID != derivativeID || first.Generation != 2 {
		t.Fatalf("first snapshot=%+v", first)
	}
	var claimed string
	if err := db.Get(&claimed, "SELECT cloud_publish_source FROM episodes WHERE id = 41"); err != nil || claimed != SyncSourceStereoSplit {
		t.Fatalf("claimed source=%q error=%v", claimed, err)
	}

	if _, err := db.Exec("UPDATE sync_logs SET status = 'failed', attempt_count = 3, next_retry_at = NULL WHERE episode_id = 41"); err != nil {
		t.Fatalf("mark first sync failed: %v", err)
	}
	if _, err := db.Exec("UPDATE episode_derivatives SET mcap_path = 'mutated/output.mcap' WHERE id = ?", derivativeID); err != nil {
		t.Fatalf("mutate derivative: %v", err)
	}
	if err := worker.EnqueueEpisodeManual(context.Background(), 41); err == nil || !strings.Contains(err.Error(), "qa_status") {
		t.Fatalf("manual retry with unapproved Episode error=%v, want qa_status rejection", err)
	}
	if _, err := db.Exec("UPDATE episodes SET qa_status = 'approved' WHERE id = 41"); err != nil {
		t.Fatalf("approve Episode: %v", err)
	}
	if err := worker.EnqueueEpisodeManual(context.Background(), 41); err != nil {
		t.Fatalf("manual retry error=%v", err)
	}
	var latestRaw string
	if err := db.Get(&latestRaw, "SELECT source_snapshot FROM sync_logs WHERE episode_id = 41 ORDER BY id DESC LIMIT 1"); err != nil {
		t.Fatalf("load retry snapshot: %v", err)
	}
	if latestRaw != firstRaw {
		t.Fatalf("retry snapshot changed\nfirst=%s\nretry=%s", firstRaw, latestRaw)
	}
	if err := worker.EnqueueEpisodeManual(context.Background(), 41); !errors.Is(err, ErrSyncAlreadyInProgress) {
		t.Fatalf("second manual retry error=%v want ErrSyncAlreadyInProgress", err)
	}
}

func TestStereoSplitSyncCompletionRejectsChangedDerivativeEligibility(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 42, "failed", false)
	result, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processing_status, qa_status,
			mcap_path, checksum, file_size_bytes
		) VALUES (42, 'stereo_split', 3, 'succeeded', 'approved',
		          'derived/42/g3/output_bag.mcap', ?, 200)
	`, testSyncSHA256)
	if err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	derivativeID, _ := result.LastInsertId()
	worker := NewSyncWorker(db, nil, nil, "test-bucket", SyncWorkerConfig{MaxRetries: 3}, nil)
	worker.SetStereoSplitSourceBucket("derivative-bucket")
	worker.running.Store(true)

	if err := worker.EnqueueStereoSplitManual(context.Background(), 42); err != nil {
		t.Fatalf("EnqueueStereoSplitManual() error=%v", err)
	}
	var syncLogID int64
	if err := db.Get(&syncLogID, "SELECT id FROM sync_logs WHERE episode_id = 42"); err != nil {
		t.Fatalf("load sync log: %v", err)
	}
	if _, err := db.Exec("UPDATE sync_logs SET status = 'in_progress' WHERE id = ?", syncLogID); err != nil {
		t.Fatalf("mark sync in progress: %v", err)
	}
	if _, err := db.Exec("UPDATE episode_derivatives SET qa_status = 'rejected' WHERE id = ?", derivativeID); err != nil {
		t.Fatalf("change derivative eligibility: %v", err)
	}

	worker.markSyncCompleted(context.Background(), syncLogID, 42, &cloud.UploadResult{
		LogicalUploadID: "logical-42",
		UploadID:        "upload-42",
		ObjectKey:       "cloud/derived-42.mcap",
		FileSize:        200,
	}, 7)

	var cloudSynced bool
	if err := db.Get(&cloudSynced, "SELECT cloud_synced FROM episodes WHERE id = 42"); err != nil {
		t.Fatalf("load episode cloud status: %v", err)
	}
	if cloudSynced {
		t.Fatal("cloud_synced=true want false after derivative eligibility changed")
	}
	var logRow struct {
		Status       string `db:"status"`
		ErrorMessage string `db:"error_message"`
	}
	if err := db.Get(&logRow, "SELECT status, error_message FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		t.Fatalf("load sync log result: %v", err)
	}
	if logRow.Status != "failed" || logRow.ErrorMessage != "sync source eligibility changed before completion" {
		t.Fatalf("sync log result=%+v want terminal eligibility failure", logRow)
	}
}

const testSyncSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
