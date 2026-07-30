// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

type fixedSTSProvider struct {
	expiration time.Time
}

func (p fixedSTSProvider) AssumeRole(context.Context, stsScope) (stsCredentials, error) {
	return stsCredentials{
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		SecurityToken:   "token",
		Expiration:      p.expiration,
	}, nil
}

func TestGatewayCreateReissueCompleteFlow(t *testing.T) {
	expiration := time.Unix(2200, 0).UTC()
	cfg := Config{
		TOSBucket:      "bucket-1",
		TOSEndpoint:    "https://tos-cn-beijing.volces.com",
		TOSRegion:      "cn-beijing",
		TOSKeyPrefix:   "device-uploads",
		UploadPartSize: 8 * 1024 * 1024,
	}
	service := newGatewayService(cfg, fixedSTSProvider{expiration: expiration}, newSessionStore(), nil, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		DeviceID: "robot-1", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":  "robot-1",
			"capture_id": "capture-1",
			"task_id":    "task-1",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	if created.GetLogicalUploadId() == "" || created.GetUploadId() == "" {
		t.Fatalf("CreateLogicalUpload() returned empty ids: %+v", created)
	}
	credentials := created.GetCredentials()
	if credentials.GetBucket() != "bucket-1" {
		t.Fatalf("bucket = %q, want bucket-1", credentials.GetBucket())
	}
	if credentials.GetStsSecurityToken() != "token" {
		t.Fatalf("security token = %q, want token", credentials.GetStsSecurityToken())
	}

	reissued, err := service.ReissueUploadCredentials(ctx, &cloudpb.ReissueUploadCredentialsRequest{
		UploadId: created.GetUploadId(),
	})
	if err != nil {
		t.Fatalf("ReissueUploadCredentials() error = %v", err)
	}
	if reissued.GetCredentials().GetObjectKey() != credentials.GetObjectKey() {
		t.Fatalf("reissued object key = %q, want %q", reissued.GetCredentials().GetObjectKey(), credentials.GetObjectKey())
	}

	recovery, err := service.GetUploadRecovery(ctx, &cloudpb.GetUploadRecoveryRequest{
		LogicalUploadId: created.GetLogicalUploadId(),
	})
	if err != nil {
		t.Fatalf("GetUploadRecovery() error = %v", err)
	}
	if recovery.GetCredentialRefreshCount() != 1 {
		t.Fatalf("refresh count = %d, want 1", recovery.GetCredentialRefreshCount())
	}

	_, err = service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"capture_id": "capture-1",
			"task_id":    "task-1",
			"device_id":  "robot-1",
		},
	})
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
}

func TestGatewayCompleteUploadPersistsEpisodeAndCompletesTask(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	qa := &fakeEpisodeQAEnqueuer{}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, qa)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":       "101",
			"capture_id":      "capture-1",
			"checksum_md5":    "9777442976C95A2F302786B97E60CEB5",
			"checksum_sha256": "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
			"product":         "ego_portal_lite",
			"task_id":         "task-1",
			"dc_plan_id":      "1001",
			"workspace_id":    "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"capture_id":      "capture-1",
			"checksum_md5":    "9777442976c95a2f302786b97e60ceb5",
			"checksum_sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"task_id":         "task-1",
			"dc_plan_id":      "1001",
			"workspace_id":    "10",
			"device_id":       "101",
			"duration_sec":    "6.4",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("second CompleteUpload() error = %v", err)
	}

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(1) FROM episodes WHERE task_id = 1 AND dc_plan_id = 1001`); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 1 {
		t.Fatalf("episode count = %d, want 1", episodeCount)
	}
	var task struct {
		Status    string `db:"status"`
		EpisodeID int64  `db:"episode_id"`
	}
	if err := db.Get(&task, `SELECT status, episode_id FROM tasks WHERE id = 1`); err != nil {
		t.Fatalf("query task: %v", err)
	}
	if task.Status != "completed" || task.EpisodeID <= 0 {
		t.Fatalf("task status=%q episode_id=%d, want completed with episode", task.Status, task.EpisodeID)
	}
	var episode struct {
		Metadata         string        `db:"metadata"`
		SidecarPath      string        `db:"sidecar_path"`
		Checksum         string        `db:"checksum"`
		DurationSec      float64       `db:"duration_sec"`
		IngestionChannel string        `db:"ingestion_channel"`
		StorageBackend   string        `db:"storage_backend"`
		HilbertRawDataID sql.NullInt64 `db:"hilbert_raw_data_id"`
	}
	if err := db.Get(&episode, `
		SELECT metadata, sidecar_path, checksum, duration_sec,
			ingestion_channel, storage_backend, hilbert_raw_data_id
		FROM episodes
		WHERE id = ?
	`, task.EpisodeID); err != nil {
		t.Fatalf("query episode metadata: %v", err)
	}
	if episode.Checksum != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("episode checksum = %q", episode.Checksum)
	}
	if !strings.Contains(episode.Metadata, created.GetUploadId()) {
		t.Fatalf("episode metadata does not contain upload id: %s", episode.Metadata)
	}
	var metadata struct {
		ChecksumMD5    string `json:"checksum_md5"`
		ChecksumSHA256 string `json:"checksum_sha256"`
	}
	if err := json.Unmarshal([]byte(episode.Metadata), &metadata); err != nil {
		t.Fatalf("decode episode metadata: %v", err)
	}
	if metadata.ChecksumMD5 != "9777442976c95a2f302786b97e60ceb5" {
		t.Fatalf("episode checksum_md5 = %q", metadata.ChecksumMD5)
	}
	if metadata.ChecksumSHA256 != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("episode checksum_sha256 = %q", metadata.ChecksumSHA256)
	}
	if episode.SidecarPath != "" {
		t.Fatalf("sidecar_path = %q, want empty for TOS-only upload", episode.SidecarPath)
	}
	if episode.DurationSec != 6.4 {
		t.Fatalf("duration_sec = %v, want 6.4", episode.DurationSec)
	}
	if episode.IngestionChannel != "data_gateway" || episode.StorageBackend != "keystone_tos" {
		t.Fatalf("episode provenance=%q/%q want data_gateway/keystone_tos", episode.IngestionChannel, episode.StorageBackend)
	}
	if episode.HilbertRawDataID.Valid {
		t.Fatalf("hilbert_raw_data_id=%#v want NULL", episode.HilbertRawDataID)
	}
	if len(qa.episodes) != 1 || qa.episodes[0] != task.EpisodeID {
		t.Fatalf("qa enqueued episodes = %v, want [%d]", qa.episodes, task.EpisodeID)
	}
}

func TestGatewayCompleteUploadRejectsExistingEpisodeFromAnotherProvenance(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
			"device_id":    "101",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if _, err := db.Exec(`
		UPDATE episodes
		SET ingestion_channel = 'axon_transfer', storage_backend = 'minio'
		WHERE task_id = 1
	`); err != nil {
		t.Fatalf("change episode provenance: %v", err)
	}

	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second CompleteUpload() error = %v, want FailedPrecondition", err)
	}
}

func TestGatewayCompleteUploadPersistsEgoPortalRecordingSHA256(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"source":       "ego-portal",
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"source":                    "ego-portal",
			"capture_id":                "capture-1",
			"task_id":                   "task-1",
			"dc_plan_id":                "1001",
			"workspace_id":              "10",
			"device_id":                 "101",
			"recording.checksum_sha256": "11285128952F5BE76F0780C62974788C40BF3E2514DE941F0F1706A1F1D6105E",
		},
	}
	_, err = service.CompleteUpload(ctx, complete)
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("second CompleteUpload() error = %v", err)
	}

	var checksum string
	if err := db.Get(&checksum, `SELECT checksum FROM episodes WHERE task_id = 1`); err != nil {
		t.Fatalf("query episode checksum: %v", err)
	}
	const wantChecksum = "11285128952f5be76f0780c62974788c40bf3e2514de941f0f1706a1f1d6105e"
	if checksum != wantChecksum {
		t.Fatalf("episode checksum = %q, want %q", checksum, wantChecksum)
	}
	complete.RawTags["recording.checksum_sha256"] = strings.Repeat("a", 64)
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CompleteUpload() with changed checksum error = %v, want FailedPrecondition", err)
	}
}

func TestGatewayCompleteUploadRejectsInvalidEgoPortalRecordingSHA256(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"source":       "ego-portal",
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	_, err = service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"source":                    "ego-portal",
			"capture_id":                "capture-1",
			"task_id":                   "task-1",
			"dc_plan_id":                "1001",
			"workspace_id":              "10",
			"device_id":                 "101",
			"recording.checksum_sha256": "not-a-sha256",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CompleteUpload() error = %v, want InvalidArgument", err)
	}
}

func TestGatewayCompleteUploadRejectsInvalidEgoPortalRecordingSHA256OnRetry(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"source":       "ego-portal",
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"source":       "ego-portal",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
			"device_id":    "101",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}

	complete.RawTags["recording.checksum_sha256"] = "not-a-sha256"
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("second CompleteUpload() error = %v, want InvalidArgument", err)
	}
}

func TestGatewayCompleteUploadAllowsLegacyEgoPortalRetryAfterChecksumBackfill(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"source":       "ego-portal",
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"source":       "ego-portal",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
			"device_id":    "101",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	var missingChecksumCount int
	if err := db.Get(&missingChecksumCount, `SELECT COUNT(1) FROM episodes WHERE task_id = 1 AND checksum IS NULL`); err != nil {
		t.Fatalf("query episodes missing checksum: %v", err)
	}
	if missingChecksumCount != 1 {
		t.Fatalf("episodes missing checksum = %d, want 1", missingChecksumCount)
	}

	const backfilledChecksum = "11285128952f5be76f0780c62974788c40bf3e2514de941f0f1706a1f1d6105e"
	if _, err := db.Exec(`UPDATE episodes SET checksum = ? WHERE task_id = 1`, backfilledChecksum); err != nil {
		t.Fatalf("backfill episode checksum: %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() after checksum backfill error = %v", err)
	}
}

func TestGatewayCreateLogicalUploadRejectsMismatchedPlan(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":    "101",
			"capture_id":   "capture-1",
			"checksum_md5": "9777442976c95a2f302786b97e60ceb5",
			"task_id":      "task-1",
			"dc_plan_id":   "9999",
			"workspace_id": "10",
		},
	})
	if err == nil {
		t.Fatal("CreateLogicalUpload() error = nil, want mismatch rejection")
	}
}

func TestGatewayCreateLogicalUploadRejectsTaskFromAnotherWorkstation(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	for _, statement := range []string{
		`INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch) VALUES (2, '202', 10, 'active', 1)`,
		`INSERT INTO workstations (id, robot_id, workspace_id) VALUES (41, 2, 10)`,
		`UPDATE tasks SET workstation_id = 41 WHERE id = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed foreign workstation: %v", err)
		}
	}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":    "101",
			"capture_id":   "capture-1",
			"task_id":      "task-1",
			"dc_plan_id":   "1001",
			"workspace_id": "10",
		},
	})

	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CreateLogicalUpload() error = %v, want PermissionDenied", err)
	}
}

func TestGatewayCreateLogicalUploadRejectsInvalidMD5(t *testing.T) {
	service := newGatewayService(
		testGatewayConfig(),
		fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
		newSessionStore(),
		nil,
		nil,
	)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":    "101",
			"capture_id":   "capture-1",
			"checksum_md5": "not-an-md5",
			"product":      "ego_portal_lite",
			"task_id":      "task-1",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateLogicalUpload() error = %v, want InvalidArgument", err)
	}
}

func TestGatewayCreateLogicalUploadRejectsInvalidSHA256(t *testing.T) {
	service := newGatewayService(
		testGatewayConfig(),
		fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
		newSessionStore(),
		nil,
		nil,
	)
	ctx := context.WithValue(context.Background(), devicePrincipalContextKey{}, devicePrincipal{
		DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"device_id":       "101",
			"capture_id":      "capture-1",
			"checksum_md5":    "9777442976c95a2f302786b97e60ceb5",
			"checksum_sha256": "not-a-sha256",
			"product":         "ego_portal_lite",
			"task_id":         "task-1",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateLogicalUpload() error = %v, want InvalidArgument", err)
	}
}

func testGatewayConfig() Config {
	return Config{
		TOSBucket:      "bucket-1",
		TOSEndpoint:    "https://tos-cn-beijing.volces.com",
		TOSRegion:      "cn-beijing",
		TOSKeyPrefix:   "device-uploads",
		UploadPartSize: 8 * 1024 * 1024,
	}
}

func newGatewayServiceTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	})
	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			auth_epoch INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER NOT NULL,
			workspace_id INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT NOT NULL,
			workstation_id INTEGER,
			organization_id INTEGER,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			status TEXT NOT NULL,
			completed_at TIMESTAMP NULL,
			episode_id INTEGER,
			error_message TEXT,
			deleted_at TIMESTAMP NULL,
			updated_at TIMESTAMP NULL
		)`,
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			workstation_id INTEGER,
			organization_id INTEGER,
			dc_plan_id INTEGER,
			local_dc_plan_id INTEGER,
			ingestion_channel TEXT NOT NULL,
			storage_backend TEXT NOT NULL,
			hilbert_raw_data_id INTEGER,
			mcap_path TEXT NOT NULL,
			sidecar_path TEXT NOT NULL,
			file_size_bytes INTEGER,
			duration_sec REAL,
			checksum TEXT,
			qa_status TEXT,
			metadata TEXT,
			created_at TIMESTAMP NULL,
			updated_at TIMESTAMP NULL,
			deleted_at TIMESTAMP NULL,
			CHECK (
				(ingestion_channel = 'axon_transfer' AND storage_backend = 'minio')
				OR
				(ingestion_channel = 'data_gateway' AND storage_backend = 'keystone_tos')
			),
			CHECK (hilbert_raw_data_id IS NULL OR hilbert_raw_data_id > 0)
		)`,
		`INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch) VALUES (1, '101', 10, 'active', 1)`,
		`INSERT INTO workstations (id, robot_id, workspace_id) VALUES (40, 1, 10)`,
		`INSERT INTO dc_plan (id, workspace_id) VALUES (1001, 10)`,
		`INSERT INTO tasks (id, task_id, workstation_id, organization_id, dc_plan_id, status) VALUES (1, 'task-1', 40, 10, 1001, 'pending')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec schema: %v\n%s", err, stmt)
		}
	}
	return db
}

type fakeEpisodeQAEnqueuer struct {
	episodes []int64
}

func (e *fakeEpisodeQAEnqueuer) EnqueueEpisode(episodeID int64) {
	e.episodes = append(e.episodes, episodeID)
}
