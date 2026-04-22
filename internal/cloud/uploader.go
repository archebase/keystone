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
	resumeContinue           resumeDecision = iota // reuse the existing session
	resumeRestart                                  // create a new session (restart)
	resumePermanentFailure                         // cannot recover; fail permanently
	resumeOSSAlreadyComplete                       // OSS object already present and verified; skip upload, go straight to CompleteUpload
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
	prepared, err := u.prepareUploadSession(ctx, hints, req.McapKey, fileSize)
	if err != nil {
		return nil, fmt.Errorf("prepare upload session: %w", err)
	}
	session := prepared.session
	restartCount := prepared.restartCount

	// Fast path: OSS object is already present (COMPLETE_ONLY recovery, ETag verified).
	// Skip the multipart upload and go straight to CompleteUpload on the gateway.
	if prepared.ossCompleteETag != "" {
		logger.Printf("[CLOUD-UPLOAD] OSS object already verified (COMPLETE_ONLY): logical_upload_id=%s etag=%s",
			session.LogicalUploadID, prepared.ossCompleteETag)
		if err := u.gateway.CompleteUpload(ctx, session.UploadID, fileSize, req.RawTags, 0, prepared.ossCompleteETag); err != nil {
			return nil, fmt.Errorf("complete upload on gateway (oss-already-complete): %w", err)
		}
		u.cleanupPersistedState(session.LogicalUploadID)
		return &UploadResult{
			LogicalUploadID: session.LogicalUploadID,
			UploadID:        session.UploadID,
			Bucket:          session.Bucket,
			ObjectKey:       session.ObjectKey,
			FileSize:        fileSize,
			OSSObjectETag:   prepared.ossCompleteETag,
		}, nil
	}

	// Persist state (initial snapshot without multipart_upload_id).
	// Mirrors Rust SDK: persistence failure is not silently ignored — without a persisted
	// state the upload cannot be recovered after a process restart.
	if !prepared.persistedState {
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
			return nil, fmt.Errorf("persist initial upload state: %w", err)
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

// preparedSession is the result of prepareUploadSession.
type preparedSession struct {
	session         *UploadSession
	restartCount    uint32
	persistedState  bool   // whether an active state file already exists on disk
	ossCompleteETag string // non-empty when the OSS object is already present and verified;
	// caller should skip uploadParts and proceed directly to CompleteUpload with this ETag.
}

// prepareUploadSession checks for persisted state and either resumes or creates a new session.
// It mirrors the Rust SDK's prepare_upload_session logic.
func (u *Uploader) prepareUploadSession(ctx context.Context, clientHints map[string]string, mcapKey string, fileSize int64) (preparedSession, error) {
	state, err := u.findPersistedStateByKey(mcapKey)
	if err != nil {
		return preparedSession{}, fmt.Errorf("load persisted state: %w", err)
	}

	if state != nil {
		decision, session, ossETag, err := u.decideResumeAction(ctx, state, fileSize)
		if err != nil {
			return preparedSession{}, err
		}
		switch decision {
		case resumeContinue:
			return preparedSession{session: session, restartCount: state.RestartCount, persistedState: true}, nil
		case resumeOSSAlreadyComplete:
			// The OSS object is already present and its ETag matches the server's record.
			// Skip uploadParts and go straight to CompleteUpload with the server-provided ETag.
			return preparedSession{
				session:         session,
				restartCount:    state.RestartCount,
				persistedState:  true,
				ossCompleteETag: ossETag,
			}, nil
		case resumeRestart:
			if state.RestartCount >= u.cfg.MaxRestartCount {
				u.abortAndCleanupSession(ctx, state.LogicalUploadID, "restart count exceeded")
				return preparedSession{}, fmt.Errorf("upload restart count exceeded (logical_upload_id=%s)", state.LogicalUploadID)
			}
			u.cleanupPersistedState(state.LogicalUploadID)
			newSession, err := u.gateway.CreateLogicalUpload(ctx, clientHints, state.UploadID)
			if err != nil {
				return preparedSession{}, fmt.Errorf("create logical upload (restart): %w", err)
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
			return preparedSession{session: newSession, restartCount: newRestartCount, persistedState: true}, nil
		case resumePermanentFailure:
			u.abortAndCleanupSession(ctx, state.LogicalUploadID, "upload cannot be resumed")
			return preparedSession{}, fmt.Errorf("persisted upload cannot be resumed (logical_upload_id=%s)", state.LogicalUploadID)
		}
	}

	// No persisted state — create a fresh logical upload
	session, err := u.gateway.CreateLogicalUpload(ctx, clientHints, "")
	if err != nil {
		return preparedSession{}, fmt.Errorf("create logical upload: %w", err)
	}
	return preparedSession{session: session, restartCount: 0, persistedState: false}, nil
}

// decideResumeAction consults the gateway recovery RPC and probes OSS state to determine
// whether to continue, restart, or permanently fail the upload.
// It mirrors the Rust SDK's decide_resume_action logic.
//
// Returns (decision, session, ossETag, error):
//   - ossETag is non-empty only when decision == resumeOSSAlreadyComplete; it is the
//     server-verified ETag to pass directly to CompleteUpload.
func (u *Uploader) decideResumeAction(ctx context.Context, state *persistedUploadState, fileSize int64) (resumeDecision, *UploadSession, string, error) {
	// Verify local file size hasn't changed
	if state.FileSize != fileSize {
		return resumePermanentFailure, nil, "", nil
	}

	recovery, err := u.gateway.GetUploadRecovery(ctx, state.LogicalUploadID)
	if err != nil {
		// Treat RPC failures as transient: return an error without a decision so the
		// caller's err != nil path preserves the local state file for the next retry.
		return resumeContinue, nil, "", fmt.Errorf("GetUploadRecovery: %w", err)
	}

	switch recovery.NextAction {
	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
		pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY:

		session, err := u.gateway.ReissueUploadCredentials(ctx, recovery.CurrentUploadID)
		if err != nil {
			// Treat RPC failures as transient: preserve local state for next retry.
			return resumeContinue, nil, "", fmt.Errorf("ReissueUploadCredentials: %w", err)
		}

		if state.MultipartUploadID != "" {
			outcome, err := u.reconcileRemoteParts(ctx, session, state.MultipartUploadID)
			if err != nil {
				// Non-404 OSS error (network/auth): treat as transient, preserve state for next retry.
				return resumeContinue, nil, "", fmt.Errorf("reconcile remote parts: %w", err)
			}
			if outcome == reconcileRestart {
				return resumeRestart, nil, "", nil
			}
		}

		if recovery.NextAction == pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY {
			outcome, err := u.reconcileCompletedObject(ctx, session, recovery.OSSObjectETag)
			if err != nil {
				// Non-404 OSS error: treat as transient, preserve state for next retry.
				return resumeContinue, nil, "", fmt.Errorf("reconcile completed object: %w", err)
			}
			switch outcome {
			case reconcileCompleted:
				// OSS object already exists and ETag matches: skip re-upload, go straight to CompleteUpload.
				return resumeOSSAlreadyComplete, session, recovery.OSSObjectETag, nil
			case reconcileRestart:
				return resumeRestart, nil, "", nil
			}
		}

		return resumeContinue, session, "", nil

	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_RESTART:
		return resumeRestart, nil, "", nil

	default: // ABORT or UNSPECIFIED
		return resumePermanentFailure, nil, "", nil
	}
}

// reconcileRemoteParts verifies that a multipart upload still exists on OSS.
// Returns reconcileRestart if the multipart upload ID is stale (HTTP 404).
// Returns reconcileContinue if the multipart upload is still active.
// Non-404 OSS errors (network failures, auth errors, etc.) are returned as errors so that
// callers can distinguish transient failures from a genuinely stale multipart upload ID;
// the returned reconcileOutcome is undefined when err != nil.
func (u *Uploader) reconcileRemoteParts(ctx context.Context, session *UploadSession, multipartUploadID string) (reconcileOutcome, error) {
	err := u.oss.ListParts(ctx, session, multipartUploadID)
	if err != nil {
		if errors.Is(err, ErrOSSNotFound) {
			return reconcileRestart, nil
		}
		return reconcileContinue, fmt.Errorf("list parts: %w", err)
	}
	return reconcileContinue, nil
}

// reconcileCompletedObject checks whether the final object already exists on OSS with the
// expected ETag. Returns reconcileCompleted on a match, reconcileRestart on a 404 or ETag
// mismatch. Non-404 OSS errors are returned as errors so the caller can distinguish transient
// failures from a genuinely missing object.
func (u *Uploader) reconcileCompletedObject(ctx context.Context, session *UploadSession, expectedETag string) (reconcileOutcome, error) {
	etag, err := u.oss.HeadObjectETag(ctx, session)
	if err != nil {
		if errors.Is(err, ErrOSSNotFound) {
			return reconcileRestart, nil
		}
		return reconcileRestart, fmt.Errorf("head object: %w", err)
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

// abortAndCleanupSession notifies the data-gateway to abort the logical upload session
// (best-effort: failures are only logged, not propagated) and then removes all local
// state files for that session. This prevents server-side session leaks when a local
// upload is permanently abandoned.
// context.Background() is used as the base so that the abort is always attempted,
// even when the caller's context is already cancelled (e.g. sync worker shutdown).
func (u *Uploader) abortAndCleanupSession(_ context.Context, logicalUploadID string, reason string) {
	if u.gateway != nil {
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := u.gateway.AbortUpload(abortCtx, logicalUploadID, reason); err != nil {
			logger.Printf("[CLOUD-UPLOAD] Warning: AbortUpload RPC failed for logical_upload_id=%s: %v", logicalUploadID, err)
		}
	}
	u.cleanupPersistedState(logicalUploadID)
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

// cleanupPersistedState removes state files for the given logical upload ID from the
// active/, terminal/, and completed/ subdirectories. This mirrors the Rust SDK's
// cleanup_persisted_state which removes from all three directories.
func (u *Uploader) cleanupPersistedState(logicalUploadID string) {
	if u.cfg.PersistRootDir == "" {
		return
	}
	base := filepath.Join(u.cfg.PersistRootDir, "data-gateway-client", "uploads")
	for _, subdir := range []string{"active", "terminal", "completed"} {
		path := filepath.Join(base, subdir, logicalUploadID+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Printf("[CLOUD-UPLOAD] Warning: failed to cleanup state file %s: %v", path, err)
		}
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
