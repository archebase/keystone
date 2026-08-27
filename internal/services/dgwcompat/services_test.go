// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/services/deviceauth"
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

type countingSTSProvider struct {
	calls int
}

var concurrentSessionBarrierCounter atomic.Uint64

type calibrationSessionBarrier struct {
	arrived   chan struct{}
	release   chan struct{}
	committed chan struct{}
	commit    sync.Once
	calls     atomic.Int32
}

func (b *calibrationSessionBarrier) wait() error {
	call := b.calls.Add(1)
	if call > 2 {
		return nil
	}
	b.arrived <- struct{}{}
	if call == 2 {
		close(b.release)
	}
	select {
	case <-b.release:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out waiting for concurrent calibration session lookup")
	}
}

type calibrationSessionBarrierDriver struct {
	inner   driver.Driver
	barrier *calibrationSessionBarrier
}

func (d *calibrationSessionBarrierDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &calibrationSessionBarrierConn{Conn: conn, barrier: d.barrier}, nil
}

type calibrationSessionBarrierConn struct {
	driver.Conn
	barrier *calibrationSessionBarrier
}

func (c *calibrationSessionBarrierConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err != nil && strings.Contains(query, "INSERT INTO calibration_sessions") {
		return nil, &mysql.MySQLError{
			Number:  1213,
			Message: "simulated concurrent calibration session insert deadlock",
		}
	}
	return result, err
}

func (c *calibrationSessionBarrierConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	isSessionLookup := strings.Contains(query, "FROM calibration_sessions") &&
		strings.Contains(query, "WHERE session_id = ?")
	if isSessionLookup && c.barrier.calls.Load() >= 2 {
		select {
		case <-c.barrier.committed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("timed out waiting for calibration session commit")
		}
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || !isSessionLookup {
		return rows, err
	}
	return &calibrationSessionBarrierRows{Rows: rows, barrier: c.barrier}, nil
}

func (c *calibrationSessionBarrierConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &calibrationSessionBarrierTx{Tx: tx, barrier: c.barrier}, nil
}

type calibrationSessionBarrierTx struct {
	driver.Tx
	barrier *calibrationSessionBarrier
}

func (tx *calibrationSessionBarrierTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.barrier.commit.Do(func() { close(tx.barrier.committed) })
	return nil
}

type calibrationSessionBarrierRows struct {
	driver.Rows
	barrier      *calibrationSessionBarrier
	synchronized bool
}

func (r *calibrationSessionBarrierRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err != io.EOF || r.synchronized {
		return err
	}
	r.synchronized = true
	if waitErr := r.barrier.wait(); waitErr != nil {
		return waitErr
	}
	return err
}

func (p *countingSTSProvider) AssumeRole(context.Context, stsScope) (stsCredentials, error) {
	p.calls++
	return stsCredentials{
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		SecurityToken:   "token",
		Expiration:      time.Unix(2200, 0).UTC(),
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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

func TestGatewayCreateLogicalUploadUsesTarForEgoPortalE2(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	if _, err := db.Exec(`UPDATE robots SET device_type = ? WHERE id = 1`, egoPortalE2DeviceType); err != nil {
		t.Fatalf("update robot device type: %v", err)
	}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{ClientHints: map[string]string{
		"device_id": "101", "capture_id": "capture-e2", "task_id": "task-1",
		"dc_plan_id": "1001", "workspace_id": "10", "source": "ego-portal",
	}})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	objectKey := created.GetCredentials().GetObjectKey()
	if !strings.HasSuffix(objectKey, "/capture.tar") {
		t.Fatalf("object key = %q, want capture.tar", objectKey)
	}
	session, ok := service.sessions.getByUpload(created.GetUploadId())
	if !ok || session.DeviceType != egoPortalE2DeviceType || session.ObjectKey != objectKey {
		t.Fatalf("frozen session = %+v, want E2 device type and object key", session)
	}
}

func TestGatewayCompleteUploadPersistsEpisodeAndCompletesTask(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	qa := &fakeEpisodeQAEnqueuer{}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, qa)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
			"capture_id":            "capture-1",
			"checksum_md5":          "9777442976c95a2f302786b97e60ceb5",
			"checksum_sha256":       "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"task_id":               "task-1",
			"dc_plan_id":            "1001",
			"workspace_id":          "10",
			"device_id":             "101",
			"duration_sec":          "6.4",
			"recording_started_at":  "2026-08-25T10:00:00.123Z",
			"recording_finished_at": "2026-08-25T10:00:06.523Z",
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
		Metadata            string        `db:"metadata"`
		SidecarPath         string        `db:"sidecar_path"`
		Checksum            string        `db:"checksum"`
		DurationSec         float64       `db:"duration_sec"`
		RecordingStartedAt  time.Time     `db:"recording_started_at"`
		RecordingFinishedAt time.Time     `db:"recording_finished_at"`
		IngestionChannel    string        `db:"ingestion_channel"`
		StorageBackend      string        `db:"storage_backend"`
		HilbertRawDataID    sql.NullInt64 `db:"hilbert_raw_data_id"`
	}
	if err := db.Get(&episode, `
		SELECT metadata, sidecar_path, checksum, duration_sec,
			recording_started_at, recording_finished_at,
			ingestion_channel, storage_backend, hilbert_raw_data_id
		FROM episodes
		WHERE id = ?
	`, task.EpisodeID); err != nil {
		t.Fatalf("query episode metadata: %v", err)
	}
	if episode.RecordingStartedAt.UTC().Format(time.RFC3339) != "2026-08-25T10:00:00Z" ||
		episode.RecordingFinishedAt.UTC().Format(time.RFC3339) != "2026-08-25T10:00:06Z" {
		t.Fatalf("episode recording times = %s - %s", episode.RecordingStartedAt, episode.RecordingFinishedAt)
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

func TestGatewayCompleteUploadFreezesMatchingCalibrationResult(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	for _, statement := range []string{
		`INSERT INTO calibration_sessions (
			session_id, camera_serial, robot_id, device_id, workspace_id, status,
			successful_capture_id, created_at, updated_at
		) VALUES (
			'7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 'CAMERA-SN-001', 1, '101', 10,
			'succeeded', '92cd6f2f-d131-4bf0-9b4a-d96258d09011', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
		`INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			checksum_sha256, logical_upload_id, upload_id, result_object_key,
			result_size_bytes, result_checksum_sha256, algorithm_version,
			processing_finished_at, created_at, updated_at
		) VALUES (
			'92cd6f2f-d131-4bf0-9b4a-d96258d09011',
			'7f9af590-75c2-47ad-b6e0-76ebf05c44f7', 1, 'succeeded', 'bucket-1',
			'calibration/source.mcap',
			'9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08',
			'logical-calibration', 'upload-calibration', 'calibration-results/result.json',
			512, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			'archebase-calib-test', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed successful calibration: %v\n%s", err, statement)
		}
	}

	service := newGatewayService(
		testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
		newSessionStore(), db, nil,
	)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"camera_serial": "  CAMERA-SN-001  ",
			"capture_id":    "capture-1",
			"dc_plan_id":    "1001",
			"source":        "ego-portal",
			"task_id":       "task-1",
			"workspace_id":  "10",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId: created.GetUploadId(), FileSize: 1024, CompletedPartCount: 1, ObjectEtag: "etag-1",
		RawTags: map[string]string{
			"camera_serial": "CAMERA-SN-001",
			"capture_id":    "capture-1",
			"dc_plan_id":    "1001",
			"task_id":       "task-1",
			"workspace_id":  "10",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}

	var episode struct {
		CameraSerial            string `db:"camera_serial"`
		CalibrationCaptureID    string `db:"calibration_capture_id"`
		CalibrationResultSHA256 string `db:"calibration_result_sha256"`
	}
	if err := db.Get(&episode, `
		SELECT camera_serial, calibration_capture_id, calibration_result_sha256
		FROM episodes WHERE task_id = 1
	`); err != nil {
		t.Fatalf("query episode calibration selection: %v", err)
	}
	if episode.CameraSerial != "CAMERA-SN-001" ||
		episode.CalibrationCaptureID != "92cd6f2f-d131-4bf0-9b4a-d96258d09011" ||
		episode.CalibrationResultSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("episode calibration selection = %+v", episode)
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("idempotent CompleteUpload() error = %v", err)
	}
	if _, err := db.Exec(`UPDATE episodes SET camera_serial = 'OTHER-CAMERA' WHERE task_id = 1`); err != nil {
		t.Fatalf("change frozen camera serial: %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CompleteUpload() changed camera error = %v, want FailedPrecondition", err)
	}
	if _, err := db.Exec(`
		UPDATE episodes SET camera_serial = 'CAMERA-SN-001', calibration_capture_id = NULL
		WHERE task_id = 1
	`); err != nil {
		t.Fatalf("make frozen calibration partial: %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CompleteUpload() partial calibration error = %v, want FailedPrecondition", err)
	}
}

func TestGatewayCompleteUploadAllowsMissingOrUnknownCameraSerial(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cameraSerial string
		wantStored   bool
	}{
		{name: "missing"},
		{name: "unknown", cameraSerial: "UNREGISTERED-CAMERA", wantStored: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newGatewayServiceTestDB(t)
			service := newGatewayService(
				testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
				newSessionStore(), db, nil,
			)
			ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
				RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
			})
			hints := map[string]string{
				"capture_id":   "capture-1",
				"dc_plan_id":   "1001",
				"source":       "ego-portal",
				"task_id":      "task-1",
				"workspace_id": "10",
			}
			rawTags := map[string]string{
				"capture_id":   "capture-1",
				"dc_plan_id":   "1001",
				"task_id":      "task-1",
				"workspace_id": "10",
			}
			if tt.cameraSerial != "" {
				hints["camera_serial"] = tt.cameraSerial
				rawTags["camera_serial"] = tt.cameraSerial
			}
			created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{ClientHints: hints})
			if err != nil {
				t.Fatalf("CreateLogicalUpload() error = %v", err)
			}
			if _, err := service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
				UploadId: created.GetUploadId(), FileSize: 1024, CompletedPartCount: 1,
				ObjectEtag: "etag-1", RawTags: rawTags,
			}); err != nil {
				t.Fatalf("CompleteUpload() error = %v", err)
			}
			var episode struct {
				CameraSerial         sql.NullString `db:"camera_serial"`
				CalibrationCaptureID sql.NullString `db:"calibration_capture_id"`
				CalibrationSHA256    sql.NullString `db:"calibration_result_sha256"`
			}
			if err := db.Get(&episode, `
				SELECT camera_serial, calibration_capture_id, calibration_result_sha256
				FROM episodes WHERE task_id = 1
			`); err != nil {
				t.Fatalf("query episode calibration selection: %v", err)
			}
			if episode.CameraSerial.Valid != tt.wantStored || episode.CameraSerial.String != tt.cameraSerial ||
				episode.CalibrationCaptureID.Valid || episode.CalibrationSHA256.Valid {
				t.Fatalf("episode calibration selection = %+v", episode)
			}
		})
	}
}

func TestGatewayCompleteUploadRequiresCameraSerialToMatchCreate(t *testing.T) {
	for _, tt := range []struct {
		name           string
		createSerial   string
		completeSerial string
	}{
		{name: "omitted on complete", createSerial: "CAMERA-SN-001"},
		{name: "changed on complete", createSerial: "CAMERA-SN-001", completeSerial: "CAMERA-SN-002"},
		{name: "added on complete", completeSerial: "CAMERA-SN-001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := newGatewayServiceTestDB(t)
			service := newGatewayService(
				testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
				newSessionStore(), db, nil,
			)
			ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
				RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
			})
			hints := map[string]string{
				"capture_id": "capture-1", "dc_plan_id": "1001", "source": "ego-portal",
				"task_id": "task-1", "workspace_id": "10",
			}
			if tt.createSerial != "" {
				hints["camera_serial"] = tt.createSerial
			}
			created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{ClientHints: hints})
			if err != nil {
				t.Fatalf("CreateLogicalUpload() error = %v", err)
			}
			rawTags := map[string]string{
				"capture_id": "capture-1", "dc_plan_id": "1001", "task_id": "task-1", "workspace_id": "10",
			}
			if tt.completeSerial != "" {
				rawTags["camera_serial"] = tt.completeSerial
			}
			_, err = service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
				UploadId: created.GetUploadId(), FileSize: 1024, CompletedPartCount: 1,
				ObjectEtag: "etag-1", RawTags: rawTags,
			})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("CompleteUpload() error = %v, want FailedPrecondition", err)
			}
		})
	}
}

func TestGatewayCompleteUploadSelectsLatestSuccessfulCalibrationByExactCameraSerial(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	for _, statement := range []string{
		`INSERT INTO calibration_sessions (
			session_id, camera_serial, robot_id, device_id, workspace_id, status,
			successful_capture_id, created_at, updated_at
		) VALUES
			('session-old', 'CAMERA-SN-001', 1, '101', 10, 'succeeded', 'capture-old', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('session-latest', 'CAMERA-SN-001', 999, 'other-device', 999, 'succeeded', 'capture-latest', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('session-case', 'camera-sn-001', 1, '101', 10, 'succeeded', 'capture-case', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('session-running', 'CAMERA-SN-001', 1, '101', 10, 'running', 'capture-running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			checksum_sha256, logical_upload_id, upload_id, result_object_key,
			result_size_bytes, result_checksum_sha256, algorithm_version,
			processing_finished_at, created_at, updated_at
		) VALUES
			('capture-old', 'session-old', 1, 'succeeded', 'bucket-1', 'old.mcap', '` + strings.Repeat("1", 64) + `', 'logical-old', 'upload-old', 'old/result.json', 100, '` + strings.Repeat("a", 64) + `', 'test', '2026-08-01 10:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('capture-latest', 'session-latest', 1, 'succeeded', 'bucket-1', 'latest.mcap', '` + strings.Repeat("2", 64) + `', 'logical-latest', 'upload-latest', 'latest/result.json', 100, '` + strings.Repeat("b", 64) + `', 'test', '2026-08-02 10:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('capture-case', 'session-case', 1, 'succeeded', 'bucket-1', 'case.mcap', '` + strings.Repeat("3", 64) + `', 'logical-case', 'upload-case', 'case/result.json', 100, '` + strings.Repeat("c", 64) + `', 'test', '2026-08-03 10:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			('capture-running', 'session-running', 1, 'succeeded', 'bucket-1', 'running.mcap', '` + strings.Repeat("4", 64) + `', 'logical-running', 'upload-running', 'running/result.json', 100, '` + strings.Repeat("d", 64) + `', 'test', '2026-08-04 10:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed calibration selection candidates: %v\n%s", err, statement)
		}
	}
	service := newGatewayService(
		testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
		newSessionStore(), db, nil,
	)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{ClientHints: map[string]string{
		"camera_serial": "CAMERA-SN-001", "capture_id": "capture-1", "dc_plan_id": "1001",
		"source": "ego-portal", "task_id": "task-1", "workspace_id": "10",
	}})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId: created.GetUploadId(), FileSize: 1024, CompletedPartCount: 1, ObjectEtag: "etag-1",
		RawTags: map[string]string{
			"camera_serial": "CAMERA-SN-001", "capture_id": "capture-1", "dc_plan_id": "1001",
			"task_id": "task-1", "workspace_id": "10",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO calibration_sessions (
			session_id, camera_serial, robot_id, device_id, workspace_id, status,
			successful_capture_id, created_at, updated_at
		) VALUES ('session-newer', 'CAMERA-SN-001', 1, '101', 10, 'succeeded', 'capture-newer', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`); err != nil {
		t.Fatalf("seed newer calibration session: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO calibration_captures (
			capture_id, calibration_session_id, attempt_no, status, bucket, object_key,
			checksum_sha256, logical_upload_id, upload_id, result_object_key,
			result_size_bytes, result_checksum_sha256, algorithm_version,
			processing_finished_at, created_at, updated_at
		) VALUES ('capture-newer', 'session-newer', 1, 'succeeded', 'bucket-1', 'newer.mcap', ?,
			'logical-newer', 'upload-newer', 'newer/result.json', 100, ?, 'test',
			'2026-08-05 10:00:00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, strings.Repeat("5", 64), strings.Repeat("e", 64)); err != nil {
		t.Fatalf("seed newer calibration capture: %v", err)
	}
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("idempotent CompleteUpload() error = %v", err)
	}
	var frozen struct {
		CaptureID string `db:"calibration_capture_id"`
		SHA256    string `db:"calibration_result_sha256"`
	}
	if err := db.Get(&frozen, `
		SELECT calibration_capture_id, calibration_result_sha256 FROM episodes WHERE task_id = 1
	`); err != nil {
		t.Fatalf("load frozen calibration selection: %v", err)
	}
	if frozen.CaptureID != "capture-latest" || frozen.SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("frozen calibration selection = %+v", frozen)
	}
}

func TestGatewayCompleteCalibrationCaptureQueuesProcessingWithoutEpisode(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	qa := &fakeEpisodeQAEnqueuer{}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, qa)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"upload_kind":            "calibration_capture",
			"camera_serial":          testCameraSerial,
			"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			"capture_id":             "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
			"attempt_no":             "1",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"source":                 "ego-portal",
			"local_operator":         "local-user",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	if got := created.GetCredentials().GetObjectKey(); got != "device-uploads/calibration-captures/101/7f9af590-75c2-47ad-b6e0-76ebf05c44f7/92cd6f2f-d131-4bf0-9b4a-d96258d09011/capture.mcap" {
		t.Fatalf("calibration object key = %q", got)
	}

	_, err = service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"upload_kind":            "calibration_capture",
			"camera_serial":          testCameraSerial,
			"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			"capture_id":             "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
			"attempt_no":             "1",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"duration_sec":           "30.0",
			"source":                 "ego-portal",
			"local_operator":         "local-user",
		},
	})
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}

	var capture struct {
		Status         string  `db:"status"`
		ObjectKey      string  `db:"object_key"`
		FileSize       int64   `db:"file_size_bytes"`
		ChecksumSHA256 string  `db:"checksum_sha256"`
		DurationSec    float64 `db:"duration_sec"`
	}
	if err := db.Get(&capture, `
		SELECT status, object_key, file_size_bytes, checksum_sha256, duration_sec
		FROM calibration_captures
		WHERE capture_id = '92cd6f2f-d131-4bf0-9b4a-d96258d09011'
	`); err != nil {
		t.Fatalf("query calibration capture: %v", err)
	}
	if capture.Status != "queued" || capture.ObjectKey != created.GetCredentials().GetObjectKey() ||
		capture.FileSize != 1024 || capture.ChecksumSHA256 != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" ||
		capture.DurationSec != 30 {
		t.Fatalf("calibration capture = %+v", capture)
	}
	var cameraSerial string
	if err := db.Get(&cameraSerial, `
		SELECT camera_serial FROM calibration_sessions
		WHERE session_id = '7f9af590-75c2-47ad-b6e0-76ebf05c44f7'
	`); err != nil {
		t.Fatalf("query calibration session camera serial: %v", err)
	}
	if cameraSerial != testCameraSerial {
		t.Fatalf("camera_serial = %q, want %q", cameraSerial, testCameraSerial)
	}

	var episodeCount int
	if err := db.Get(&episodeCount, `SELECT COUNT(1) FROM episodes`); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 0 || len(qa.episodes) != 0 {
		t.Fatalf("calibration created episodes=%d qa=%v", episodeCount, qa.episodes)
	}
}

func TestCreateLogicalUploadKeepsCalibrationSessionCameraSerialBeforeSTS(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	sts := &countingSTSProvider{}
	service := newGatewayService(testGatewayConfig(), sts, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	const sessionID = "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"

	if _, err := service.CreateLogicalUpload(ctx, calibrationCreateRequest(
		sessionID,
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"1",
		"CAMERA-A",
	)); err != nil {
		t.Fatalf("first CreateLogicalUpload() error = %v", err)
	}
	_, err := service.CreateLogicalUpload(ctx, calibrationCreateRequest(
		sessionID,
		"d4ad1825-35b4-4572-83aa-70cf3d8dd083",
		"2",
		"camera-a",
	))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mismatched camera_serial error = %v, want FailedPrecondition", err)
	}
	if sts.calls != 1 {
		t.Fatalf("STS calls = %d, want 1", sts.calls)
	}
}

func TestCreateLogicalUploadConcurrentSessionCreationKeepsCameraSerial(t *testing.T) {
	const sessionID = "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"
	barrier := &calibrationSessionBarrier{
		arrived:   make(chan struct{}, 2),
		release:   make(chan struct{}),
		committed: make(chan struct{}),
	}
	driverNumber := concurrentSessionBarrierCounter.Add(1)
	driverName := fmt.Sprintf("sqlite_calibration_session_barrier_%d", driverNumber)
	sql.Register(driverName, &calibrationSessionBarrierDriver{
		inner:   &sqlite.Driver{},
		barrier: barrier,
	})
	dsn := fmt.Sprintf("file:calibration-session-race-%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", driverNumber)
	db := newGatewayServiceTestDBWithDriver(t, driverName, dsn)
	db.SetMaxOpenConns(2)
	service := newGatewayService(
		testGatewayConfig(),
		fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()},
		newSessionStore(),
		db,
		nil,
	)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	requests := []*cloudpb.CreateLogicalUploadRequest{
		calibrationCreateRequest(sessionID, "92cd6f2f-d131-4bf0-9b4a-d96258d09011", "1", "CAMERA-A"),
		calibrationCreateRequest(sessionID, "d4ad1825-35b4-4572-83aa-70cf3d8dd083", "2", "CAMERA-B"),
	}
	start := make(chan struct{})
	results := make(chan error, len(requests))
	for _, request := range requests {
		go func() {
			<-start
			_, err := service.CreateLogicalUpload(ctx, request)
			results <- err
		}()
	}
	close(start)
	for range requests {
		select {
		case <-barrier.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("concurrent session creation reached database barrier %d times, want 2", barrier.calls.Load())
		}
	}

	var successCount, mismatchCount int
	for range requests {
		err := <-results
		switch status.Code(err) {
		case codes.OK:
			successCount++
		case codes.FailedPrecondition:
			mismatchCount++
		default:
			t.Fatalf("concurrent CreateLogicalUpload() error = %v, want success or FailedPrecondition", err)
		}
	}
	if successCount != 1 || mismatchCount != 1 {
		t.Fatalf("concurrent results: success=%d mismatch=%d, want 1 each", successCount, mismatchCount)
	}
}

func TestCalibrationSessionInsertConflictRecognizesMySQLConflicts(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "duplicate key", err: &mysql.MySQLError{Number: 1062}, want: true},
		{name: "deadlock", err: fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1213}), want: true},
		{name: "other MySQL error", err: &mysql.MySQLError{Number: 1048}},
		{name: "non-MySQL error", err: fmt.Errorf("database unavailable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCalibrationSessionInsertConflict(tt.err); got != tt.want {
				t.Fatalf("isCalibrationSessionInsertConflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCalibrationUploadDurationSecFitsDatabaseColumn(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantValid bool
		wantCode  codes.Code
	}{
		{name: "equivalent trailing zeros", value: "30.0000", wantValid: true},
		{name: "maximum value", value: "999999999.999", wantValid: true},
		{name: "sub-millisecond precision", value: "30.0004", wantCode: codes.InvalidArgument},
		{name: "integer overflow", value: "1000000000", wantCode: codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration, err := calibrationUploadDurationSec(map[string]string{"duration_sec": tt.value})
			if status.Code(err) != tt.wantCode {
				t.Fatalf("calibrationUploadDurationSec() error = %v, want code %v", err, tt.wantCode)
			}
			if duration.Valid != tt.wantValid {
				t.Fatalf("calibrationUploadDurationSec() valid = %v, want %v", duration.Valid, tt.wantValid)
			}
		})
	}
}

func TestCreateLogicalUploadAllowsSameCameraSerialInDifferentCalibrationSessions(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	sts := &countingSTSProvider{}
	service := newGatewayService(testGatewayConfig(), sts, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	requests := []*cloudpb.CreateLogicalUploadRequest{
		calibrationCreateRequest(
			"7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
			"1",
			testCameraSerial,
		),
		calibrationCreateRequest(
			"593f25fb-e9df-4c2c-85ca-c7bd85515316",
			"d4ad1825-35b4-4572-83aa-70cf3d8dd083",
			"1",
			testCameraSerial,
		),
	}
	for index, request := range requests {
		if _, err := service.CreateLogicalUpload(ctx, request); err != nil {
			t.Fatalf("CreateLogicalUpload(%d) error = %v", index, err)
		}
	}
	if sts.calls != 2 {
		t.Fatalf("STS calls = %d, want 2", sts.calls)
	}
}

func TestCreateLogicalUploadRejectsHistoricalCalibrationSessionWithoutCameraSerial(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	const sessionID = "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"
	if _, err := db.Exec(`
		INSERT INTO calibration_sessions (
			session_id, robot_id, device_id, workspace_id, camera_serial, status, created_at, updated_at
		) VALUES (?, 1, '101', 10, NULL, 'running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, sessionID); err != nil {
		t.Fatalf("seed historical calibration session: %v", err)
	}
	sts := &countingSTSProvider{}
	service := newGatewayService(testGatewayConfig(), sts, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	_, err := service.CreateLogicalUpload(ctx, calibrationCreateRequest(
		sessionID,
		"92cd6f2f-d131-4bf0-9b4a-d96258d09011",
		"1",
		testCameraSerial,
	))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateLogicalUpload() error = %v, want FailedPrecondition", err)
	}
	if sts.calls != 0 {
		t.Fatalf("STS calls = %d, want 0", sts.calls)
	}
}

func TestCompleteUploadRequiresStableCalibrationMetadataAndIsIdempotent(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	const sessionID = "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"
	const captureID = "92cd6f2f-d131-4bf0-9b4a-d96258d09011"
	created, err := service.CreateLogicalUpload(ctx, calibrationCreateRequest(sessionID, captureID, "1", testCameraSerial))
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	complete := &cloudpb.CompleteUploadRequest{
		UploadId:           created.GetUploadId(),
		FileSize:           1024,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-1",
		RawTags: map[string]string{
			"upload_kind":            "calibration_capture",
			"calibration_session_id": sessionID,
			"capture_id":             captureID,
			"attempt_no":             "1",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	}
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing camera_serial error = %v, want FailedPrecondition", err)
	}
	complete.RawTags["camera_serial"] = strings.ToLower(testCameraSerial)
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mutated camera_serial error = %v, want FailedPrecondition", err)
	}
	complete.RawTags["camera_serial"] = " " + testCameraSerial + " "
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("non-normalized camera_serial error = %v, want FailedPrecondition", err)
	}
	complete.RawTags["camera_serial"] = testCameraSerial
	complete.RawTags["duration_sec"] = "30.0004"
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over-precise duration_sec error = %v, want InvalidArgument", err)
	}
	complete.RawTags["duration_sec"] = "30.0"
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("CompleteUpload() error = %v", err)
	}
	complete.RawTags["duration_sec"] = "60.0"
	if _, err := service.CompleteUpload(ctx, complete); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mutated duration_sec error = %v, want FailedPrecondition", err)
	}
	complete.RawTags["duration_sec"] = "30.00"
	if _, err := service.CompleteUpload(ctx, complete); err != nil {
		t.Fatalf("semantically idempotent CompleteUpload() error = %v", err)
	}
}

func TestGatewayCreateCalibrationCaptureRejectsOversizedMetadata(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "source", key: "source", value: strings.Repeat("s", 65)},
		{name: "local operator", key: "local_operator", value: strings.Repeat("采", 256)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newGatewayServiceTestDB(t)
			service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
			ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
				RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
			})
			hints := map[string]string{
				"upload_kind":            "calibration_capture",
				"camera_serial":          testCameraSerial,
				"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
				"capture_id":             "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
				"attempt_no":             "1",
				"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
				tt.key:                   tt.value,
			}

			_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{ClientHints: hints})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CreateLogicalUpload() error = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestGatewayCalibrationSessionRejectsNewCaptureButSupersedesStartedCaptureAfterSuccess(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	sessionID := "7f9af590-75c2-47ad-b6e0-76ebf05c44f7"
	createCapture := func(captureID, attempt string) *cloudpb.CreateLogicalUploadResponse {
		t.Helper()
		created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
			ClientHints: map[string]string{
				"upload_kind":            "calibration_capture",
				"camera_serial":          testCameraSerial,
				"calibration_session_id": sessionID,
				"capture_id":             captureID,
				"attempt_no":             attempt,
				"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			},
		})
		if err != nil {
			t.Fatalf("CreateLogicalUpload(%s) error = %v", captureID, err)
		}
		return created
	}
	firstCaptureID := "92cd6f2f-d131-4bf0-9b4a-d96258d09011"
	secondCaptureID := "d4ad1825-35b4-4572-83aa-70cf3d8dd083"
	createCapture(firstCaptureID, "1")
	second := createCapture(secondCaptureID, "2")
	if _, err := db.Exec(`
		UPDATE calibration_sessions
		SET status = 'succeeded', successful_capture_id = ?
		WHERE session_id = ?
	`, firstCaptureID, sessionID); err != nil {
		t.Fatalf("succeed calibration session: %v", err)
	}

	if _, err := service.CompleteUpload(ctx, &cloudpb.CompleteUploadRequest{
		UploadId:           second.GetUploadId(),
		FileSize:           2048,
		CompletedPartCount: 1,
		ObjectEtag:         "etag-2",
		RawTags: map[string]string{
			"upload_kind":            "calibration_capture",
			"camera_serial":          testCameraSerial,
			"calibration_session_id": sessionID,
			"capture_id":             secondCaptureID,
			"attempt_no":             "2",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	}); err != nil {
		t.Fatalf("CompleteUpload(started capture) error = %v", err)
	}
	var statusValue string
	if err := db.Get(&statusValue, `SELECT status FROM calibration_captures WHERE capture_id = ?`, secondCaptureID); err != nil {
		t.Fatalf("query superseded capture: %v", err)
	}
	if statusValue != "superseded" {
		t.Fatalf("started capture status = %q, want superseded", statusValue)
	}

	_, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		ClientHints: map[string]string{
			"upload_kind":            "calibration_capture",
			"camera_serial":          testCameraSerial,
			"calibration_session_id": sessionID,
			"capture_id":             "593f25fb-e9df-4c2c-85ca-c7bd85515316",
			"attempt_no":             "3",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("new capture after success error = %v, want FailedPrecondition", err)
	}
}

func TestGatewayCalibrationRejectsDifferentCaptureForSameAttempt(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	request := func(captureID string) *cloudpb.CreateLogicalUploadRequest {
		return &cloudpb.CreateLogicalUploadRequest{ClientHints: map[string]string{
			"upload_kind":            "calibration_capture",
			"camera_serial":          testCameraSerial,
			"calibration_session_id": "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			"capture_id":             captureID,
			"attempt_no":             "1",
			"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		}}
	}
	if _, err := service.CreateLogicalUpload(ctx, request("92cd6f2f-d131-4bf0-9b4a-d96258d09011")); err != nil {
		t.Fatalf("first CreateLogicalUpload() error = %v", err)
	}
	_, err := service.CreateLogicalUpload(ctx, request("d4ad1825-35b4-4572-83aa-70cf3d8dd083"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate attempt error = %v, want FailedPrecondition", err)
	}
}

func TestGatewayCompleteUploadRejectsExistingEpisodeFromAnotherProvenance(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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

func TestGatewayCreateLogicalUploadAutoAssignsTaskForDevice(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	sessions := newSessionStore()
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, sessions, db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})

	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		AutoAssignTask: true,
		DcPlanId:       1001,
		ClientHints: map[string]string{
			"device_id":       "101",
			"capture_id":      "capture-auto",
			"product":         "ego_portal_lite",
			"checksum_md5":    "9777442976c95a2f302786b97e60ceb5",
			"checksum_sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	if created.GetResolvedTaskId() != "task-1" || created.GetResolvedDcPlanId() != 1001 || created.GetResolvedWorkspaceId() != 10 {
		t.Fatalf("resolved target = task %q plan %d workspace %d", created.GetResolvedTaskId(), created.GetResolvedDcPlanId(), created.GetResolvedWorkspaceId())
	}
	var statusValue string
	if err := db.Get(&statusValue, `SELECT status FROM tasks WHERE id = 1`); err != nil {
		t.Fatalf("query reserved task: %v", err)
	}
	if statusValue != "uploading" {
		t.Fatalf("reserved task status = %q, want uploading", statusValue)
	}
	session, ok := sessions.getByLogical(created.GetLogicalUploadId())
	if !ok || session.TaskID != "task-1" || !session.AutoAssignedTask {
		t.Fatalf("stored session = %#v, found=%t", session, ok)
	}
}

func TestGatewayAbortAutoAssignedUploadFailsReservedTask(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
		RobotID: 1, DeviceID: "101", WorkspaceID: 10, AuthEpoch: 1,
	})
	created, err := service.CreateLogicalUpload(ctx, &cloudpb.CreateLogicalUploadRequest{
		AutoAssignTask: true,
		DcPlanId:       1001,
		ClientHints: map[string]string{
			"capture_id":      "capture-abort",
			"product":         "ego_portal_lite",
			"checksum_md5":    "9777442976c95a2f302786b97e60ceb5",
			"checksum_sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		},
	})
	if err != nil {
		t.Fatalf("CreateLogicalUpload() error = %v", err)
	}
	if _, err := service.AbortUpload(ctx, &cloudpb.AbortUploadRequest{LogicalUploadId: created.GetLogicalUploadId()}); err != nil {
		t.Fatalf("AbortUpload() error = %v", err)
	}
	var statusValue, errorMessage string
	if err := db.QueryRowx(`SELECT status, error_message FROM tasks WHERE id = 1`).Scan(&statusValue, &errorMessage); err != nil {
		t.Fatalf("query aborted task: %v", err)
	}
	if statusValue != "failed" || errorMessage == "" {
		t.Fatalf("aborted task = status %q error %q, want failed with reason", statusValue, errorMessage)
	}
}

func TestGatewayCreateLogicalUploadRejectsTaskFromAnotherWorkstation(t *testing.T) {
	db := newGatewayServiceTestDB(t)
	for _, statement := range []string{
		`INSERT INTO robots (id, device_id, device_type, workspace_id, status, auth_epoch) VALUES (2, '202', 'Ego Portal Stereo', 10, 'active', 1)`,
		`INSERT INTO workstations (id, robot_id, workspace_id) VALUES (41, 2, 10)`,
		`UPDATE tasks SET workstation_id = 41 WHERE id = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed foreign workstation: %v", err)
		}
	}
	service := newGatewayService(testGatewayConfig(), fixedSTSProvider{expiration: time.Unix(2200, 0).UTC()}, newSessionStore(), db, nil)
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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
	ctx := deviceauth.WithPrincipal(context.Background(), devicePrincipal{
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

func calibrationCreateRequest(sessionID, captureID, attemptNo, cameraSerial string) *cloudpb.CreateLogicalUploadRequest {
	return &cloudpb.CreateLogicalUploadRequest{ClientHints: map[string]string{
		"upload_kind":            "calibration_capture",
		"camera_serial":          cameraSerial,
		"calibration_session_id": sessionID,
		"capture_id":             captureID,
		"attempt_no":             attemptNo,
		"checksum_sha256":        "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}}
}

func newGatewayServiceTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	return newGatewayServiceTestDBWithDSN(t, ":memory:")
}

func newGatewayServiceTestDBWithDSN(t *testing.T, dsn string) *sqlx.DB {
	t.Helper()
	return newGatewayServiceTestDBWithDriver(t, "sqlite", dsn)
}

func newGatewayServiceTestDBWithDriver(t *testing.T, driverName, dsn string) *sqlx.DB {
	t.Helper()
	standardDB, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db := sqlx.NewDb(standardDB, "sqlite")
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	})
	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			device_type TEXT,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			auth_epoch INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER NOT NULL,
			data_collector_id INTEGER,
			workspace_id INTEGER NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE data_collectors (
			id INTEGER PRIMARY KEY,
			operator_id TEXT NOT NULL,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			workspace_id INTEGER NOT NULL,
			name TEXT,
			operator TEXT,
			dc_project_description TEXT,
			dc_task_description TEXT,
			dc_device_id INTEGER,
			status TEXT,
			dc_type TEXT,
			target_count INTEGER,
			cur_count INTEGER DEFAULT 0,
			target_duration INTEGER,
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
			assigned_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL,
			episode_id INTEGER,
			error_message TEXT,
			metadata TEXT,
			created_at TIMESTAMP NULL,
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
			recording_started_at TIMESTAMP NULL,
			recording_finished_at TIMESTAMP NULL,
			checksum TEXT,
			qa_status TEXT,
			cloud_synced BOOLEAN DEFAULT FALSE,
			metadata TEXT,
			camera_serial TEXT,
			calibration_capture_id TEXT,
			calibration_result_sha256 TEXT,
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
		`CREATE TABLE calibration_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL UNIQUE,
			camera_serial TEXT,
			robot_id INTEGER NOT NULL,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			successful_capture_id TEXT,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE calibration_captures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			capture_id TEXT NOT NULL UNIQUE,
			calibration_session_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			status TEXT NOT NULL,
			bucket TEXT NOT NULL,
			object_key TEXT NOT NULL,
			file_size_bytes INTEGER,
			duration_sec REAL,
			checksum_sha256 TEXT NOT NULL,
			object_etag TEXT,
			logical_upload_id TEXT NOT NULL,
			upload_id TEXT NOT NULL,
			source TEXT,
			local_operator TEXT,
			processing_finished_at TIMESTAMP,
			result_object_key TEXT,
			result_size_bytes INTEGER,
			result_checksum_sha256 TEXT,
			algorithm_version TEXT,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			uploaded_at TIMESTAMP
		)`,
		`INSERT INTO robots (id, device_id, device_type, workspace_id, status, auth_epoch) VALUES (1, '101', 'Ego Portal Stereo', 10, 'active', 1)`,
		`INSERT INTO data_collectors (id, operator_id) VALUES (7, 'collector-1')`,
		`INSERT INTO workstations (id, robot_id, data_collector_id, workspace_id) VALUES (40, 1, 7, 10)`,
		`INSERT INTO dc_plan (
			id, workspace_id, name, operator, dc_device_id, dc_type,
			target_count, cur_count, target_duration
		) VALUES (1001, 10, 'Plan 1001', 'collector-1', 101, 'ego', 10, 0, 3600)`,
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
