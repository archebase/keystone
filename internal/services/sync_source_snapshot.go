// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

const (
	syncSourceAuto = "auto"
	// SyncSourceOriginal selects the Episode's original object for cloud upload.
	SyncSourceOriginal = "original"
	// SyncSourceStereoSplit selects the approved derivative for cloud upload.
	SyncSourceStereoSplit = "stereo_split"
	// SyncSourceDepthNormalization selects the approved local depth-normalization
	// derivative as the canonical cloud upload source.
	SyncSourceDepthNormalization = "depth_normalization"
	// SyncBackendMinIO reads the frozen source object through Keystone's MinIO client.
	SyncBackendMinIO = "minio"
	// SyncBackendTOS reads the frozen source object through Keystone's TOS client.
	SyncBackendTOS = "tos"
)

// ErrCloudPublishSourceLocked indicates that an Episode already claimed another canonical upload source.
var ErrCloudPublishSourceLocked = errors.New("episode cloud publish source is locked")

// ErrSyncSourceUnavailable indicates that an Episode's canonical upload source
// is not ready for cloud sync.
var ErrSyncSourceUnavailable = errors.New("episode sync source is unavailable")

// SyncSourceSnapshot is the immutable upload input stored on a sync_log. All
// retries and recovery read this value rather than re-selecting an object from
// mutable Episode or Derivative fields.
type SyncSourceSnapshot struct {
	SourceType   string `json:"source_type"`
	Backend      string `json:"backend"`
	Bucket       string `json:"bucket"`
	ObjectKey    string `json:"object_key"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256,omitempty"`
	BagName      string `json:"bag_name"`
	DerivativeID int64  `json:"derivative_id,omitempty"`
	Generation   int    `json:"generation,omitempty"`
}

func (s SyncSourceSnapshot) validate() error {
	if s.SourceType != SyncSourceOriginal && s.SourceType != SyncSourceStereoSplit &&
		s.SourceType != SyncSourceDepthNormalization {
		return fmt.Errorf("unsupported sync source type %q", s.SourceType)
	}
	if s.Backend != SyncBackendMinIO && s.Backend != SyncBackendTOS {
		return fmt.Errorf("unsupported sync source backend %q", s.Backend)
	}
	if strings.TrimSpace(s.Bucket) == "" || strings.TrimSpace(s.ObjectKey) == "" || strings.TrimSpace(s.BagName) == "" {
		return fmt.Errorf("sync source snapshot is missing object identity")
	}
	if s.SizeBytes <= 0 {
		return fmt.Errorf("sync source snapshot must contain a positive object size")
	}
	checksum := strings.ToLower(strings.TrimSpace(s.SHA256))
	if len(checksum) != 64 {
		return fmt.Errorf("sync source snapshot must contain a SHA-256 checksum")
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return fmt.Errorf("sync source snapshot has invalid SHA-256")
	}
	if (s.SourceType == SyncSourceStereoSplit || s.SourceType == SyncSourceDepthNormalization) &&
		(s.DerivativeID <= 0 || s.Generation <= 0) {
		return fmt.Errorf("%s sync snapshot is missing generation identity", s.SourceType)
	}
	return nil
}

func encodeSyncSourceSnapshot(snapshot SyncSourceSnapshot) (string, error) {
	snapshot.SourceType = strings.TrimSpace(snapshot.SourceType)
	snapshot.Backend = strings.TrimSpace(snapshot.Backend)
	snapshot.Bucket = strings.TrimSpace(snapshot.Bucket)
	snapshot.ObjectKey = strings.TrimLeft(strings.TrimSpace(snapshot.ObjectKey), "/")
	snapshot.SHA256 = strings.ToLower(strings.TrimSpace(snapshot.SHA256))
	snapshot.BagName = strings.TrimSpace(snapshot.BagName)
	if err := snapshot.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode sync source snapshot: %w", err)
	}
	return string(encoded), nil
}

func decodeSyncSourceSnapshot(raw string) (SyncSourceSnapshot, error) {
	var snapshot SyncSourceSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return SyncSourceSnapshot{}, fmt.Errorf("decode sync source snapshot: %w", err)
	}
	if err := snapshot.validate(); err != nil {
		return SyncSourceSnapshot{}, err
	}
	return snapshot, nil
}

func (w *SyncWorker) buildOriginalSourceSnapshot(ep syncEpisodeUploadRow) (SyncSourceSnapshot, error) {
	backend := SyncBackendMinIO
	bucket := strings.TrimSpace(w.minioBucket)
	objectKey := objectKeyFromStoredPath(ep.McapPath, bucket)
	if bucket == "" {
		storedPath := strings.TrimLeft(strings.TrimSpace(ep.McapPath), "/")
		parts := strings.SplitN(storedPath, "/", 2)
		if len(parts) == 2 {
			bucket = parts[0]
			objectKey = parts[1]
		}
	}
	var metadata episodeSourceMetadata
	if ep.Metadata.Valid && json.Unmarshal([]byte(ep.Metadata.String), &metadata) == nil &&
		(strings.EqualFold(strings.TrimSpace(ep.StorageBackend), "keystone_tos") || metadata.usesTOS(w.tosBucket)) {
		backend = SyncBackendTOS
		bucket = strings.TrimSpace(metadata.Bucket)
		if bucket == "" {
			bucket = strings.TrimSpace(w.tosBucket)
		}
		objectKey = strings.TrimLeft(strings.TrimSpace(metadata.ObjectKey), "/")
		if objectKey == "" {
			objectKey = objectKeyFromStoredPath(ep.McapPath, bucket)
		}
	}
	checksum := ""
	if ep.Checksum.Valid {
		checksum = strings.ToLower(strings.TrimSpace(ep.Checksum.String))
	}
	size := int64(0)
	if ep.FileSizeBytes.Valid && ep.FileSizeBytes.Int64 > 0 {
		size = ep.FileSizeBytes.Int64
	}
	snapshot := SyncSourceSnapshot{
		SourceType: SyncSourceOriginal,
		Backend:    backend,
		Bucket:     bucket,
		ObjectKey:  objectKey,
		SizeBytes:  size,
		SHA256:     checksum,
		BagName:    hilbertBagName(ep, objectKey),
	}
	if _, err := encodeSyncSourceSnapshot(snapshot); err != nil {
		return SyncSourceSnapshot{}, newNonRetryableSyncError("episode %d has invalid original sync source: %v", ep.ID, err)
	}
	return snapshot, nil
}

func (w *SyncWorker) buildStereoSplitSourceSnapshot(ctx context.Context, tx *sqlx.Tx, ep syncEpisodeUploadRow) (SyncSourceSnapshot, error) {
	var derivative struct {
		ID         int64          `db:"id"`
		Generation int            `db:"generation"`
		Status     string         `db:"processing_status"`
		QAStatus   string         `db:"qa_status"`
		McapPath   sql.NullString `db:"mcap_path"`
		Checksum   sql.NullString `db:"checksum"`
		SizeBytes  sql.NullInt64  `db:"file_size_bytes"`
	}
	if err := tx.GetContext(ctx, &derivative, `
		SELECT id, generation, processing_status, qa_status, mcap_path, checksum, file_size_bytes
		FROM episode_derivatives
		WHERE episode_id = ? AND kind = 'stereo_split'
	`+txLockClause(tx), ep.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncSourceSnapshot{}, fmt.Errorf("stereo split derivative not found for episode %d", ep.ID)
		}
		return SyncSourceSnapshot{}, fmt.Errorf("load stereo split sync source: %w", err)
	}
	if derivative.Status != "succeeded" || derivative.QAStatus != "approved" {
		return SyncSourceSnapshot{}, fmt.Errorf("stereo split derivative must be succeeded and QA approved")
	}
	objectKey := strings.TrimLeft(strings.TrimSpace(derivative.McapPath.String), "/")
	snapshot := SyncSourceSnapshot{
		SourceType:   SyncSourceStereoSplit,
		Backend:      SyncBackendTOS,
		Bucket:       strings.TrimSpace(w.derivativeBucket),
		ObjectKey:    objectKey,
		SizeBytes:    derivative.SizeBytes.Int64,
		SHA256:       strings.ToLower(strings.TrimSpace(derivative.Checksum.String)),
		BagName:      hilbertBagName(ep, objectKey),
		DerivativeID: derivative.ID,
		Generation:   derivative.Generation,
	}
	if _, err := encodeSyncSourceSnapshot(snapshot); err != nil {
		return SyncSourceSnapshot{}, newNonRetryableSyncError("episode %d has invalid stereo split sync source: %v", ep.ID, err)
	}
	return snapshot, nil
}

func (w *SyncWorker) buildDepthNormalizationSourceSnapshot(ctx context.Context, tx *sqlx.Tx, ep syncEpisodeUploadRow) (SyncSourceSnapshot, error) {
	var derivative struct {
		ID         int64          `db:"id"`
		Generation int            `db:"generation"`
		Status     string         `db:"processing_status"`
		QAStatus   string         `db:"qa_status"`
		McapPath   sql.NullString `db:"mcap_path"`
		Checksum   sql.NullString `db:"checksum"`
		SizeBytes  sql.NullInt64  `db:"file_size_bytes"`
	}
	if err := tx.GetContext(ctx, &derivative, `
		SELECT id, generation, processing_status, qa_status, mcap_path, checksum, file_size_bytes
		FROM episode_derivatives
		WHERE episode_id = ? AND kind = 'depth_normalization'
	`+txLockClause(tx), ep.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncSourceSnapshot{}, fmt.Errorf("depth normalization derivative not found for episode %d", ep.ID)
		}
		return SyncSourceSnapshot{}, fmt.Errorf("load depth normalization sync source: %w", err)
	}
	if derivative.Status != "succeeded" || derivative.QAStatus != "approved" {
		return SyncSourceSnapshot{}, fmt.Errorf("depth normalization derivative must be succeeded and QA approved")
	}
	objectKey := strings.TrimLeft(strings.TrimSpace(derivative.McapPath.String), "/")
	snapshot := SyncSourceSnapshot{
		SourceType:   SyncSourceDepthNormalization,
		Backend:      SyncBackendMinIO,
		Bucket:       strings.TrimSpace(w.minioBucket),
		ObjectKey:    objectKey,
		SizeBytes:    derivative.SizeBytes.Int64,
		SHA256:       strings.ToLower(strings.TrimSpace(derivative.Checksum.String)),
		BagName:      hilbertBagName(ep, objectKey),
		DerivativeID: derivative.ID,
		Generation:   derivative.Generation,
	}
	if _, err := encodeSyncSourceSnapshot(snapshot); err != nil {
		return SyncSourceSnapshot{}, newNonRetryableSyncError("episode %d has invalid depth normalization sync source: %v", ep.ID, err)
	}
	return snapshot, nil
}

func (w *SyncWorker) resolveManualSyncSourceTx(ctx context.Context, tx *sqlx.Tx, ep syncEpisodeUploadRow) (string, error) {
	if strings.EqualFold(strings.TrimSpace(ep.DeviceType), DeviceTypeZJWA1D) {
		return w.resolveZJWA1DSyncSourceTx(ctx, tx, ep)
	}

	claimedSource := strings.TrimSpace(ep.CloudPublishSource.String)
	switch claimedSource {
	case SyncSourceOriginal:
		return SyncSourceOriginal, nil
	case "", SyncSourceStereoSplit:
	default:
		return "", fmt.Errorf("%w: unsupported claimed source %q", ErrCloudPublishSourceLocked, claimedSource)
	}

	// Stereo split is only canonical for Ego Portal Stereo. Every other device
	// syncs the original object unless another source has already been claimed.
	if claimedSource == "" && !strings.EqualFold(strings.TrimSpace(ep.DeviceType), "Ego Portal Stereo") {
		return SyncSourceOriginal, nil
	}

	var derivative struct {
		ProcessingStatus string `db:"processing_status"`
		QAStatus         string `db:"qa_status"`
	}
	if err := tx.GetContext(ctx, &derivative, `
		SELECT processing_status, qa_status
		FROM episode_derivatives
		WHERE episode_id = ? AND kind = 'stereo_split'
	`+txLockClause(tx), ep.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if claimedSource == SyncSourceStereoSplit {
				return "", fmt.Errorf("%w: claimed stereo split derivative was not found", ErrSyncSourceUnavailable)
			}
			return SyncSourceOriginal, nil
		}
		return "", fmt.Errorf("load stereo split sync state: %w", err)
	}
	if derivative.ProcessingStatus == "succeeded" && derivative.QAStatus == "approved" {
		return SyncSourceStereoSplit, nil
	}
	return "", fmt.Errorf(
		"%w: stereo split processing_status=%q qa_status=%q",
		ErrSyncSourceUnavailable,
		derivative.ProcessingStatus,
		derivative.QAStatus,
	)
}

type depthNormalizationEpisodeMetadata struct {
	DepthNormalization struct {
		Required   bool   `json:"required"`
		Reason     string `json:"reason"`
		CheckedAt  string `json:"checked_at,omitempty"`
		OutputOnly bool   `json:"output_only,omitempty"`
	} `json:"depth_normalization"`
}

func zjwa1dDepthNormalizationNotRequired(ep syncEpisodeUploadRow) bool {
	if !ep.Metadata.Valid {
		return false
	}
	var metadata depthNormalizationEpisodeMetadata
	if err := json.Unmarshal([]byte(ep.Metadata.String), &metadata); err != nil {
		return false
	}
	if metadata.DepthNormalization.Required {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(metadata.DepthNormalization.Reason))
	return reason == "already_target" || reason == "already_compresseddepth"
}

func (w *SyncWorker) resolveZJWA1DSyncSourceTx(ctx context.Context, tx *sqlx.Tx, ep syncEpisodeUploadRow) (string, error) {
	claimedSource := strings.TrimSpace(ep.CloudPublishSource.String)
	if claimedSource != SyncSourceDepthNormalization && zjwa1dDepthNormalizationNotRequired(ep) {
		return SyncSourceOriginal, nil
	}
	switch claimedSource {
	case SyncSourceDepthNormalization:
	case SyncSourceOriginal:
		if !zjwa1dDepthNormalizationNotRequired(ep) {
			return "", fmt.Errorf("%w: ZJ-WA1-D original source must already use a supported depth format", ErrSyncSourceUnavailable)
		}
		return SyncSourceOriginal, nil
	case "":
	default:
		return "", fmt.Errorf("%w: unsupported claimed source %q", ErrCloudPublishSourceLocked, claimedSource)
	}

	var derivative struct {
		ProcessingStatus string `db:"processing_status"`
		QAStatus         string `db:"qa_status"`
	}
	if zjwa1dDepthNormalizationNotRequired(ep) {
		return SyncSourceOriginal, nil
	}

	err := tx.GetContext(ctx, &derivative, `
		SELECT processing_status, qa_status
		FROM episode_derivatives
		WHERE episode_id = ? AND kind = 'depth_normalization'
	`+txLockClause(tx), ep.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if zjwa1dDepthNormalizationNotRequired(ep) {
			return SyncSourceOriginal, nil
		}
		return "", fmt.Errorf("%w: depth normalization is required", ErrSyncSourceUnavailable)
	}
	if err != nil {
		return "", fmt.Errorf("load depth normalization sync state: %w", err)
	}
	if derivative.ProcessingStatus == "succeeded" && derivative.QAStatus == "approved" {
		return SyncSourceDepthNormalization, nil
	}
	return "", fmt.Errorf(
		"%w: depth normalization processing_status=%q qa_status=%q",
		ErrSyncSourceUnavailable,
		derivative.ProcessingStatus,
		derivative.QAStatus,
	)
}

func (w *SyncWorker) sourceForSnapshot(snapshot SyncSourceSnapshot) (SourceObjectReader, string, string, error) {
	if err := snapshot.validate(); err != nil {
		return nil, "", "", newNonRetryableSyncError("invalid persisted sync source snapshot: %v", err)
	}
	switch snapshot.Backend {
	case SyncBackendTOS:
		if w.tosSource == nil {
			return nil, "", "", fmt.Errorf("TOS source object reader not available")
		}
		return w.tosSource, snapshot.Bucket, snapshot.ObjectKey, nil
	case SyncBackendMinIO:
		reader := w.sourceReader()
		if reader == nil {
			return nil, "", "", fmt.Errorf("source object reader not available")
		}
		return reader, snapshot.Bucket, snapshot.ObjectKey, nil
	default:
		return nil, "", "", newNonRetryableSyncError("unsupported persisted source backend %q", snapshot.Backend)
	}
}

func loadSyncSourceSnapshot(ctx context.Context, db sqlx.QueryerContext, syncLogID int64) (SyncSourceSnapshot, bool, error) {
	var raw sql.NullString
	if err := sqlx.GetContext(ctx, db, &raw, "SELECT source_snapshot FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		return SyncSourceSnapshot{}, false, fmt.Errorf("load sync source snapshot: %w", err)
	}
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return SyncSourceSnapshot{}, false, nil
	}
	snapshot, err := decodeSyncSourceSnapshot(raw.String)
	if err != nil {
		return SyncSourceSnapshot{}, false, newNonRetryableSyncError("sync_log %d has invalid source snapshot: %v", syncLogID, err)
	}
	return snapshot, true, nil
}

func (w *SyncWorker) validateSyncSourceGateTx(ctx context.Context, tx *sqlx.Tx, episodeID int64, episodeQA, claimedSource string, snapshot SyncSourceSnapshot) error {
	if claimedSource != snapshot.SourceType {
		return fmt.Errorf("%w: episode source is %q but sync snapshot is %q", ErrCloudPublishSourceLocked, claimedSource, snapshot.SourceType)
	}
	if snapshot.SourceType == SyncSourceOriginal {
		if episodeQA != "approved" {
			return fmt.Errorf("episode %d qa_status is %q, must be approved", episodeID, episodeQA)
		}
		return nil
	}
	var count int
	if err := tx.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM episode_derivatives
		WHERE id = ? AND episode_id = ? AND kind = ? AND generation = ?
		  AND processing_status = 'succeeded' AND qa_status = 'approved'
	`, snapshot.DerivativeID, episodeID, snapshot.SourceType, snapshot.Generation); err != nil {
		return fmt.Errorf("validate %s sync gate: %w", snapshot.SourceType, err)
	}
	if count != 1 {
		return fmt.Errorf("%s generation is no longer eligible for sync", snapshot.SourceType)
	}
	return nil
}
