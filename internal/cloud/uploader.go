// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/minio/minio-go/v7"
)

// UploaderConfig defines configuration for the high-level uploader.
type UploaderConfig struct {
	// RequestTimeout is the per-RPC/HTTP deadline for gateway calls.
	RequestTimeout time.Duration
	// OSSTimeout is the HTTP timeout for individual OSS part uploads.
	OSSTimeout time.Duration
	// PersistRootDir is the directory used to persist active upload state for recovery.
	// If empty, persistence is disabled.
	PersistRootDir string
	// MaxRestartCount limits the number of times an upload may be restarted before permanent failure.
	// Defaults to 3 if zero.
	MaxRestartCount uint32
}

// UploadRequest describes an episode upload from MinIO to cloud.
type UploadRequest struct {
	// EpisodeID is the unique episode identifier used as client hint.
	EpisodeID string
	// McapKey is the MinIO object key for the MCAP file (without bucket prefix).
	McapKey string
	// RawTags are arbitrary key-value tags passed to the data-gateway.
	RawTags map[string]string
	// ClientHints are passed to CreateLogicalUpload for server-side routing.
	ClientHints map[string]string
}

// UploadResult describes the outcome of a successful cloud upload.
type UploadResult struct {
	LogicalUploadID string
	UploadID        string
	Bucket          string
	ObjectKey       string
	FileSize        int64
	OSSObjectETag   string
}

// persistedUploadState is the JSON-serialisable snapshot written to disk for recovery.
type persistedUploadState struct {
	Version           uint32    `json:"version"`
	LogicalUploadID   string    `json:"logical_upload_id"`
	UploadID          string    `json:"upload_id"`
	RestartCount      uint32    `json:"restart_count"`
	MultipartUploadID string    `json:"multipart_upload_id,omitempty"`
	Bucket            string    `json:"bucket"`
	Endpoint          string    `json:"endpoint"`
	ObjectKey         string    `json:"object_key"`
	McapKey           string    `json:"mcap_key"`
	FileSize          int64     `json:"file_size"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// reconcileOutcome represents the decision after probing remote state.
type reconcileOutcome int

const (
	reconcileContinue  reconcileOutcome = iota // continue uploading parts
	reconcileRestart                           // abort and restart from scratch
	reconcileCompleted                         // object already present and verified
)

// resumeDecision is the result of decide_resume_action.
type resumeDecision int

const (
	resumeContinue         resumeDecision = iota // reuse the existing session
	resumeRestart                                // create a new session (restart)
	resumePermanentFailure                       // cannot recover; fail permanently
)

// Uploader orchestrates the complete upload flow:
//  1. Auth (gRPC) → JWT
//  2. Gateway (gRPC) → logical upload session + STS credentials + object key
//  3. OSS (REST) → multipart upload
//  4. Gateway (gRPC) → complete upload
//
// It mirrors the Rust data-platform-data-gateway-client SDK, including local
// persistence for cross-restart recovery and the full restart/abort state machine.
type Uploader struct {
	gateway     *GatewayClient
	oss         *OSSUploader
	minioClient *s3.Client
	minioBucket string
	cfg         UploaderConfig
}

// NewUploader creates a new uploader that streams data from MinIO to cloud OSS.
func NewUploader(gateway *GatewayClient, minioClient *s3.Client, minioBucket string, cfg UploaderConfig) *Uploader {
	if cfg.MaxRestartCount == 0 {
		cfg.MaxRestartCount = 3
	}
	return &Uploader{
		gateway:     gateway,
		oss:         NewOSSUploader(cfg.OSSTimeout),
		minioClient: minioClient,
		minioBucket: minioBucket,
		cfg:         cfg,
	}
}

// abortMultipartUpload attempts to abort a multipart upload with a timeout.
// It uses context.Background() as base to ensure the abort is independent of the
// caller's context, but with a 30s timeout to prevent indefinite hanging.
func (u *Uploader) abortMultipartUpload(session *UploadSession, multipartUploadID string) {
	abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u.oss.AbortMultipartUpload(abortCtx, session, multipartUploadID)
}

// Upload streams a file from MinIO to cloud OSS via the data-gateway control plane.
// It does NOT buffer the entire file in memory — only one part_size_bytes buffer is held at a time.
//
// On startup, it checks for a persisted active state for the given McapKey. If found,
// it calls GetUploadRecovery to determine the appropriate recovery action (continue,
// restart, complete-only, abort) before proceeding.
func (u *Uploader) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	// Merge client hints
	hints := map[string]string{
		"episode_id": req.EpisodeID,
		"source":     "keystone-edge",
	}
	for k, v := range req.ClientHints {
		hints[k] = v
	}

	// Step 1: Get file size from MinIO
	objInfo, err := u.minioClient.StatObject(ctx, u.minioBucket, req.McapKey, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("stat minio object %s/%s: %w", u.minioBucket, req.McapKey, err)
	}
	fileSize := objInfo.Size
	if fileSize == 0 {
		return nil, fmt.Errorf("zero-byte file uploads are not allowed: %s", req.McapKey)
	}

	logger.Printf("[CLOUD-UPLOAD] Starting upload: episode=%s mcap=%s size=%d", req.EpisodeID, req.McapKey, fileSize)

	// Step 2: Prepare upload session (with recovery if persisted state exists)
	session, restartCount, persistedActiveState, err := u.prepareUploadSession(ctx, hints, req.McapKey, fileSize)
	if err != nil {
		return nil, fmt.Errorf("prepare upload session: %w", err)
	}

	// Persist state (initial snapshot without multipart_upload_id)
	if !persistedActiveState {
		if err := u.persistActiveState(&persistedUploadState{
			Version:         1,
			LogicalUploadID: session.LogicalUploadID,
			UploadID:        session.UploadID,
			RestartCount:    restartCount,
			Bucket:          session.Bucket,
			Endpoint:        session.Endpoint,
			ObjectKey:       session.ObjectKey,
			McapKey:         req.McapKey,
			FileSize:        fileSize,
			UpdatedAt:       time.Now(),
		}); err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: failed to persist initial state: %v", err)
		}
	}

	logger.Printf("[CLOUD-UPLOAD] Session ready: logical_upload_id=%s upload_id=%s object_key=%s part_size=%d",
		session.LogicalUploadID, session.UploadID, session.ObjectKey, session.PartSizeBytes)

	// Step 3: Multipart upload
	multipartUploadID, parts, partMD5s, err := u.uploadParts(ctx, req, session, fileSize)
	if err != nil {
		return nil, err
	}

	// Update persisted state with multipart_upload_id
	if err := u.persistActiveState(&persistedUploadState{
		Version:           1,
		LogicalUploadID:   session.LogicalUploadID,
		UploadID:          session.UploadID,
		RestartCount:      restartCount,
		MultipartUploadID: multipartUploadID,
		Bucket:            session.Bucket,
		Endpoint:          session.Endpoint,
		ObjectKey:         session.ObjectKey,
		McapKey:           req.McapKey,
		FileSize:          fileSize,
		UpdatedAt:         time.Now(),
	}); err != nil {
		logger.Printf("[CLOUD-UPLOAD] Warning: failed to update state with multipart_upload_id: %v", err)
	}

	localETag := BuildMultipartETag(partMD5s)
	logger.Printf("[CLOUD-UPLOAD] OSS upload complete: parts=%d etag=%s", len(parts), localETag)

	// Step 4: Refresh STS credentials if about to expire before CompleteUpload RPC
	if time.Until(session.STSExpireAt) <= u.cfg.RequestTimeout {
		refreshed, err := u.gateway.ReissueUploadCredentials(ctx, session.UploadID)
		if err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: refresh credentials failed (proceeding anyway): %v", err)
		} else {
			session = refreshed
		}
	}

	// Step 5: Notify data-gateway that upload is complete
	if len(parts) > math.MaxInt32 {
		return nil, fmt.Errorf("too many upload parts: %d", len(parts))
	}
	//nolint:gosec // G115: len(parts) validated to fit into int32 above
	if err := u.gateway.CompleteUpload(ctx, session.UploadID, fileSize, req.RawTags, int32(len(parts)), localETag); err != nil {
		return nil, fmt.Errorf("complete upload on gateway: %w", err)
	}

	logger.Printf("[CLOUD-UPLOAD] Upload complete: episode=%s logical_upload_id=%s upload_id=%s object_key=%s",
		req.EpisodeID, session.LogicalUploadID, session.UploadID, session.ObjectKey)

	// Cleanup persisted state after success
	u.cleanupPersistedState(session.LogicalUploadID)

	return &UploadResult{
		LogicalUploadID: session.LogicalUploadID,
		UploadID:        session.UploadID,
		Bucket:          session.Bucket,
		ObjectKey:       session.ObjectKey,
		FileSize:        fileSize,
		OSSObjectETag:   localETag,
	}, nil
}

// prepareUploadSession checks for persisted state and either resumes or creates a new session.
// It mirrors the Rust SDK's prepare_upload_session logic.
// Returns the session, restart count, whether active state was already persisted, and any error.
func (u *Uploader) prepareUploadSession(ctx context.Context, clientHints map[string]string, mcapKey string, fileSize int64) (*UploadSession, uint32, bool, error) {
	state, err := u.findPersistedStateByKey(mcapKey)
	if err != nil {
		return nil, 0, false, fmt.Errorf("load persisted state: %w", err)
	}

	if state != nil {
		decision, session, err := u.decideResumeAction(ctx, state, fileSize)
		if err != nil {
			return nil, 0, false, err
		}
		switch decision {
		case resumeContinue:
			return session, state.RestartCount, true, nil
		case resumeRestart:
			if state.RestartCount >= u.cfg.MaxRestartCount {
				u.cleanupPersistedState(state.LogicalUploadID)
				return nil, 0, false, fmt.Errorf("upload restart count exceeded (logical_upload_id=%s)", state.LogicalUploadID)
			}
			u.cleanupPersistedState(state.LogicalUploadID)
			newSession, err := u.gateway.CreateLogicalUpload(ctx, clientHints, state.UploadID)
			if err != nil {
				return nil, 0, false, fmt.Errorf("create logical upload (restart): %w", err)
			}
			newRestartCount := state.RestartCount + 1
			if err := u.persistActiveState(&persistedUploadState{
				Version:         1,
				LogicalUploadID: newSession.LogicalUploadID,
				UploadID:        newSession.UploadID,
				RestartCount:    newRestartCount,
				Bucket:          newSession.Bucket,
				Endpoint:        newSession.Endpoint,
				ObjectKey:       newSession.ObjectKey,
				McapKey:         mcapKey,
				FileSize:        fileSize,
				UpdatedAt:       time.Now(),
			}); err != nil {
				logger.Printf("[CLOUD-UPLOAD] Warning: failed to persist restart state: %v", err)
			}
			return newSession, newRestartCount, true, nil
		case resumePermanentFailure:
			u.cleanupPersistedState(state.LogicalUploadID)
			return nil, 0, false, fmt.Errorf("persisted upload cannot be resumed (logical_upload_id=%s)", state.LogicalUploadID)
		}
	}

	// No persisted state — create a fresh logical upload
	session, err := u.gateway.CreateLogicalUpload(ctx, clientHints, "")
	if err != nil {
		return nil, 0, false, fmt.Errorf("create logical upload: %w", err)
	}
	return session, 0, false, nil
}

// decideResumeAction consults the gateway recovery RPC and probes OSS state to determine
// whether to continue, restart, or permanently fail the upload.
// It mirrors the Rust SDK's decide_resume_action logic.
func (u *Uploader) decideResumeAction(ctx context.Context, state *persistedUploadState, fileSize int64) (resumeDecision, *UploadSession, error) {
	// Verify local file size hasn't changed
	if state.FileSize != fileSize {
		return resumePermanentFailure, nil, nil
	}

	recovery, err := u.gateway.GetUploadRecovery(ctx, state.LogicalUploadID)
	if err != nil {
		return resumePermanentFailure, nil, fmt.Errorf("GetUploadRecovery: %w", err)
	}

	switch recovery.NextAction {
	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
		pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY:

		session, err := u.gateway.ReissueUploadCredentials(ctx, recovery.CurrentUploadID)
		if err != nil {
			return resumePermanentFailure, nil, fmt.Errorf("ReissueUploadCredentials: %w", err)
		}

		if state.MultipartUploadID != "" {
			outcome, err := u.reconcileRemoteParts(ctx, session, state.MultipartUploadID)
			if err != nil {
				return resumePermanentFailure, nil, fmt.Errorf("reconcile remote parts: %w", err)
			}
			switch outcome {
			case reconcileRestart:
				return resumeRestart, nil, nil
			case reconcileCompleted:
				return resumeContinue, session, nil
			}
		}

		if recovery.NextAction == pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY {
			outcome, err := u.reconcileCompletedObject(ctx, session, recovery.OSSObjectETag)
			if err != nil {
				return resumePermanentFailure, nil, fmt.Errorf("reconcile completed object: %w", err)
			}
			switch outcome {
			case reconcileCompleted:
				return resumeContinue, session, nil
			case reconcileRestart:
				return resumeRestart, nil, nil
			}
		}

		return resumeContinue, session, nil

	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_RESTART:
		return resumeRestart, nil, nil

	default: // ABORT or UNSPECIFIED
		return resumePermanentFailure, nil, nil
	}
}

// reconcileRemoteParts verifies that a multipart upload still exists on OSS.
// Returns reconcileRestart if the multipart upload ID is stale, reconcileContinue otherwise.
func (u *Uploader) reconcileRemoteParts(ctx context.Context, session *UploadSession, multipartUploadID string) (reconcileOutcome, error) {
	err := u.oss.ListParts(ctx, session, multipartUploadID)
	if err != nil {
		if errors.Is(err, ErrOSSNotFound) {
			return reconcileRestart, nil
		}
		return reconcileRestart, nil // treat unknown error as restart
	}
	return reconcileContinue, nil
}

// reconcileCompletedObject checks whether the final object already exists on OSS with the
// expected ETag. Returns reconcileCompleted on a match, reconcileRestart otherwise.
func (u *Uploader) reconcileCompletedObject(ctx context.Context, session *UploadSession, expectedETag string) (reconcileOutcome, error) {
	etag, err := u.oss.HeadObjectETag(ctx, session)
	if err != nil {
		if errors.Is(err, ErrOSSNotFound) {
			return reconcileRestart, nil
		}
		return reconcileRestart, nil
	}
	if etag == expectedETag {
		return reconcileCompleted, nil
	}
	return reconcileRestart, nil
}

// uploadParts streams the MCAP from MinIO and uploads it to OSS in parts.
// Returns the OSS multipart upload ID, the list of uploaded parts, per-part MD5 digests, and any error.
func (u *Uploader) uploadParts(ctx context.Context, req UploadRequest, session *UploadSession, fileSize int64) (string, []UploadedPart, [][16]byte, error) {
	// Initiate multipart upload on OSS
	multipartUploadID, err := u.oss.InitiateMultipartUpload(ctx, session)
	if err != nil {
		return "", nil, nil, fmt.Errorf("initiate multipart upload: %w", err)
	}
	logger.Printf("[CLOUD-UPLOAD] Multipart initiated: multipart_upload_id=%s", multipartUploadID)

	// Stream from MinIO → OSS in parts
	mcapStream, err := u.minioClient.GetObject(ctx, u.minioBucket, req.McapKey, minio.GetObjectOptions{})
	if err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return "", nil, nil, fmt.Errorf("get minio object %s: %w", req.McapKey, err)
	}
	defer func() {
		_ = mcapStream.Close()
	}()

	partSizeBytes := session.PartSizeBytes
	if partSizeBytes <= 0 {
		partSizeBytes = 8 * 1024 * 1024 // 8MB default
	}

	buf := make([]byte, partSizeBytes)
	var parts []UploadedPart
	var partMD5s [][16]byte
	var offset int64
	partNumber := 1

	for offset < fileSize {
		if err := ctx.Err(); err != nil {
			u.abortMultipartUpload(session, multipartUploadID)
			return "", nil, nil, err
		}

		remaining := fileSize - offset
		readSize := partSizeBytes
		if remaining < readSize {
			readSize = remaining
		}

		n, readErr := io.ReadFull(mcapStream, buf[:readSize])
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			u.abortMultipartUpload(session, multipartUploadID)
			return "", nil, nil, fmt.Errorf("read part %d from minio: %w", partNumber, readErr)
		}

		partSlice := buf[:n]
		partMD5s = append(partMD5s, MD5DigestBytes(partSlice))

		etag, err := u.oss.UploadPart(ctx, session, multipartUploadID, partNumber, partSlice)
		if err != nil {
			u.abortMultipartUpload(session, multipartUploadID)
			return "", nil, nil, fmt.Errorf("upload part %d: %w", partNumber, err)
		}

		parts = append(parts, UploadedPart{
			PartNumber: partNumber,
			ETag:       etag,
		})

		offset += int64(n)
		partNumber++

		if partNumber%10 == 0 {
			logger.Printf("[CLOUD-UPLOAD] Progress: episode=%s parts=%d offset=%d/%d",
				req.EpisodeID, len(parts), offset, fileSize)
		}
	}

	// Complete multipart upload on OSS
	if _, err := u.oss.CompleteMultipartUpload(ctx, session, multipartUploadID, parts); err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return "", nil, nil, fmt.Errorf("complete multipart upload on OSS: %w", err)
	}

	return multipartUploadID, parts, partMD5s, nil
}

// activeStateDir returns the directory for active upload state files.
func (u *Uploader) activeStateDir() string {
	return filepath.Join(u.cfg.PersistRootDir, "data-gateway-client", "uploads", "active")
}

// persistActiveState atomically writes the upload state JSON to the active directory.
func (u *Uploader) persistActiveState(state *persistedUploadState) error {
	if u.cfg.PersistRootDir == "" {
		return nil
	}
	dir := u.activeStateDir()
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G301: state dir contains no secrets; 0750 satisfies gosec
		return fmt.Errorf("mkdir active state dir: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	finalPath := filepath.Join(dir, state.LogicalUploadID+".json")
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil { //nolint:gosec // G306: upload state files do not contain secrets
		return fmt.Errorf("write tmp state: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}
	return nil
}

// cleanupPersistedState removes the active state file for the given logical upload ID.
func (u *Uploader) cleanupPersistedState(logicalUploadID string) {
	if u.cfg.PersistRootDir == "" {
		return
	}
	path := filepath.Join(u.activeStateDir(), logicalUploadID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Printf("[CLOUD-UPLOAD] Warning: failed to cleanup state file %s: %v", path, err)
	}
}

// findPersistedStateByKey scans the active state directory for a state matching the given mcap key.
func (u *Uploader) findPersistedStateByKey(mcapKey string) (*persistedUploadState, error) {
	if u.cfg.PersistRootDir == "" {
		return nil, nil
	}
	dir := u.activeStateDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read active state dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // G304: path is built from os.ReadDir results within a controlled state directory
		if err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: failed to read state file %s: %v", entry.Name(), err)
			continue
		}
		var state persistedUploadState
		if err := json.Unmarshal(data, &state); err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: failed to parse state file %s: %v", entry.Name(), err)
			continue
		}
		if state.McapKey == mcapKey {
			return &state, nil
		}
	}
	return nil, nil
}
