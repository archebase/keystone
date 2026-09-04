// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud"
	"archebase.com/keystone-edge/internal/config"
)

// CalibrationBackfillResult describes a confirmed calibration binding.
type CalibrationBackfillResult struct {
	EpisodeID              int64  `json:"episode_id"`
	CameraSerial           string `json:"camera_serial"`
	ParamFileMotionStoreID string `json:"param_file_motion_store_id"`
	Status                 string `json:"status"`
}

// BackfillEpisodeCalibration uploads and binds a selected camera calibration without re-uploading MCAP.
func (w *SyncWorker) BackfillEpisodeCalibration(ctx context.Context, episodeID int64, cameraSerial string) (*CalibrationBackfillResult, error) {
	if w == nil || w.db == nil || w.hilbert == nil {
		return nil, fmt.Errorf("calibration backfill is not configured")
	}
	if episodeID <= 0 || strings.TrimSpace(cameraSerial) == "" {
		return nil, fmt.Errorf("episode ID and camera serial are required")
	}

	var episode struct {
		ID          int64         `db:"id"`
		CloudSynced bool          `db:"cloud_synced"`
		RawDataID   sql.NullInt64 `db:"hilbert_raw_data_id"`
		WorkspaceID sql.NullInt64 `db:"workspace_id"`
	}
	if err := w.db.GetContext(ctx, &episode, `
		SELECT e.id, e.cloud_synced, e.hilbert_raw_data_id, dp.workspace_id
		FROM episodes e
		LEFT JOIN dc_plan dp ON dp.id = e.dc_plan_id AND dp.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL`, episodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("episode %d not found", episodeID)
		}
		return nil, fmt.Errorf("query episode %d: %w", episodeID, err)
	}
	if !episode.CloudSynced || !episode.RawDataID.Valid || episode.RawDataID.Int64 <= 0 {
		return nil, fmt.Errorf("episode %d has not been synced to Hilbert", episodeID)
	}
	if !episode.WorkspaceID.Valid || episode.WorkspaceID.Int64 <= 0 {
		return nil, fmt.Errorf("episode %d has no valid workspace", episodeID)
	}

	rawData, err := w.hilbert.GetRawDataByID(ctx, episode.WorkspaceID.Int64, episode.RawDataID.Int64)
	if err != nil {
		return nil, fmt.Errorf("query Hilbert raw data %d: %w", episode.RawDataID.Int64, err)
	}
	if rawData == nil || rawData.ID != episode.RawDataID.Int64 {
		return nil, fmt.Errorf("hilbert raw data %d was not found", episode.RawDataID.Int64)
	}
	if rawData.ParamFileMotionStoreID != nil && strings.TrimSpace(*rawData.ParamFileMotionStoreID) != "" {
		return &CalibrationBackfillResult{EpisodeID: episodeID, CameraSerial: cameraSerial, ParamFileMotionStoreID: *rawData.ParamFileMotionStoreID, Status: "already_bound"}, nil
	}

	var calibration struct {
		Bucket    string `db:"bucket"`
		ObjectKey string `db:"object_key"`
		SizeBytes int64  `db:"size_bytes"`
		SHA256    string `db:"sha256"`
	}
	if err := w.db.GetContext(ctx, &calibration, `
		SELECT bucket, object_key, size_bytes, sha256
		FROM camera_calibrations
		WHERE BINARY camera_serial = BINARY ?`, strings.TrimSpace(cameraSerial)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("no calibration file found for camera serial %s", cameraSerial)
		}
		return nil, fmt.Errorf("query calibration for camera serial %s: %w", cameraSerial, err)
	}
	if calibration.SizeBytes <= 0 || len(strings.TrimSpace(calibration.SHA256)) != 64 {
		return nil, fmt.Errorf("calibration file for camera serial %s has invalid metadata", cameraSerial)
	}

	reader := w.sourceReader()
	if strings.EqualFold(strings.TrimSpace(calibration.Bucket), strings.TrimSpace(w.tosBucket)) {
		reader = w.tosSource
	}
	if reader == nil {
		return nil, fmt.Errorf("calibration source object reader not available")
	}
	bucket := strings.TrimSpace(calibration.Bucket)
	objectKey := strings.TrimLeft(strings.TrimSpace(calibration.ObjectKey), "/")
	size, etag, err := reader.StatObject(ctx, bucket, objectKey)
	if err != nil || size != calibration.SizeBytes {
		return nil, fmt.Errorf("calibration object identity changed")
	}

	registration, err := w.hilbert.RegisterParamFile(ctx, auth.HilbertParamFileRegisterRequest{
		WorkspaceID: episode.WorkspaceID.Int64, ContentSHA256: strings.ToLower(strings.TrimSpace(calibration.SHA256)), SizeBytes: size,
	})
	if err != nil {
		return nil, fmt.Errorf("register Hilbert calibration: %w", err)
	}
	paramFileID := strings.TrimSpace(registration.ParamFileMotionStoreID)
	if registration.State == auth.CalibrationSnapshotStateUploading {
		credentials, err := w.hilbert.GetParamFileUploadCredentials(ctx, episode.WorkspaceID.Int64, paramFileID)
		if err != nil {
			return nil, fmt.Errorf("get Hilbert calibration upload credentials: %w", err)
		}
		obj, err := w.openSourceObjectRangeStream(ctx, reader, bucket, objectKey, size, etag)
		if err != nil {
			return nil, fmt.Errorf("open calibration object: %w", err)
		}
		uploader := w.tosUploader
		if uploader == nil {
			uploader = cloud.NewTOSS3Uploader(w.syncOSSTimeout(), config.ModeEdge)
		}
		_, uploadErr := uploader.PutObject(ctx, hilbertUploadTarget(credentials), obj, size, strings.ToLower(strings.TrimSpace(calibration.SHA256)), nil)
		_ = obj.Close()
		if uploadErr != nil {
			return nil, fmt.Errorf("upload Hilbert calibration: %w", uploadErr)
		}
		if err := w.hilbert.FinishParamFileUpload(ctx, episode.WorkspaceID.Int64, paramFileID); err != nil {
			return nil, fmt.Errorf("finish Hilbert calibration upload: %w", err)
		}
	} else if registration.State != auth.CalibrationSnapshotStateReady {
		return nil, fmt.Errorf("unsupported Hilbert calibration snapshot state %q", registration.State)
	}

	if err := w.hilbert.UpdateRawDataParamFile(ctx, episode.WorkspaceID.Int64, episode.RawDataID.Int64, paramFileID); err != nil {
		return nil, fmt.Errorf("bind Hilbert calibration: %w", err)
	}
	confirmed, err := w.hilbert.GetRawDataByID(ctx, episode.WorkspaceID.Int64, episode.RawDataID.Int64)
	if err != nil {
		return nil, fmt.Errorf("confirm Hilbert calibration binding: %w", err)
	}
	if confirmed == nil || confirmed.ParamFileMotionStoreID == nil || strings.TrimSpace(*confirmed.ParamFileMotionStoreID) == "" {
		return nil, fmt.Errorf("confirm Hilbert calibration binding: binding is not present")
	}
	return &CalibrationBackfillResult{EpisodeID: episodeID, CameraSerial: cameraSerial, ParamFileMotionStoreID: *confirmed.ParamFileMotionStoreID, Status: "bound"}, nil
}
