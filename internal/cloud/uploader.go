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
	"strings"
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
	// AssetID is the Data Platform device id used for this upload.
	AssetID string
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
	AssetID           string    `json:"asset_id"`
	FileSize          int64     `json:"file_size"`
	PartSizeBytes     int64     `json:"part_size_bytes,omitempty"`
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

// gatewayClient is the subset of GatewayClient methods used by Uploader.
// It is defined as an interface to allow test injection of fake implementations.
type gatewayClient interface {
	CreateLogicalUpload(ctx context.Context, clientHints map[string]string, restartFromUploadID string) (*UploadSession, error)
	GetUploadRecovery(ctx context.Context, logicalUploadID string) (*UploadRecoveryInfo, error)
	ReissueUploadCredentials(ctx context.Context, uploadID string) (*UploadSession, error)
	AbortUpload(ctx context.Context, logicalUploadID string, reason string) error
	CompleteUpload(ctx context.Context, uploadID string, fileSize int64, rawTags map[string]string, completedPartCount int32, ossObjectEtag string, partSizeBytes int64) error
}

// ossClient is the subset of OSSUploader methods used by Uploader.
// It is defined as an interface to allow test injection of fake implementations.
type ossClient interface {
	InitiateMultipartUpload(ctx context.Context, session *UploadSession) (string, error)
	UploadPart(ctx context.Context, session *UploadSession, multipartUploadID string, partNumber int, body []byte) (string, error)
	CompleteMultipartUpload(ctx context.Context, session *UploadSession, multipartUploadID string, parts []UploadedPart) (string, error)
	AbortMultipartUpload(ctx context.Context, session *UploadSession, multipartUploadID string)
	HeadObjectETag(ctx context.Context, session *UploadSession) (string, error)
	ListParts(ctx context.Context, session *UploadSession, multipartUploadID string) error
}

// Uploader orchestrates the complete upload flow:
//  1. Auth (gRPC) → JWT
//  2. Gateway (gRPC) → logical upload session + STS credentials + object key
//  3. OSS (REST) → multipart upload
//  4. Gateway (gRPC) → complete upload
//
// It mirrors the Rust data-platform-data-gateway-client SDK, including local
// persistence for cross-restart recovery and the full restart/abort state machine.
type Uploader struct {
	gateway     gatewayClient
	oss         ossClient
	minioClient *s3.Client
	minioBucket string
	cfg         UploaderConfig
}

// NewUploader creates a new uploader that streams data from MinIO to cloud OSS.
// If PersistRootDir is set, the active state directory is created and tested for
// writability at construction time. An error is returned if the directory cannot
// be created or is not writable; callers should treat this as a fatal startup error.
func NewUploader(gateway *GatewayClient, minioClient *s3.Client, minioBucket string, cfg UploaderConfig) (*Uploader, error) {
	if cfg.MaxRestartCount == 0 {
		cfg.MaxRestartCount = 3
	}
	u := &Uploader{
		gateway:     gateway,
		oss:         NewOSSUploader(cfg.OSSTimeout),
		minioClient: minioClient,
		minioBucket: minioBucket,
		cfg:         cfg,
	}
	if cfg.PersistRootDir != "" {
		if err := u.validatePersistDir(); err != nil {
			return nil, fmt.Errorf("persist_root_dir %q is not usable: %w", cfg.PersistRootDir, err)
		}
	}
	return u, nil
}

// validatePersistDir creates the active state directory and verifies it is writable
// by writing and immediately removing a probe file.
func (u *Uploader) validatePersistDir() error {
	dir := u.activeStateDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cannot create state directory: %w", err)
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return fmt.Errorf("state directory is not writable: %w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// abortMultipartUpload attempts to abort a multipart upload with a timeout.
// It uses context.Background() as base to ensure the abort is independent of the
// caller's context, but with a 30s timeout to prevent indefinite hanging.
func (u *Uploader) abortMultipartUpload(session *UploadSession, multipartUploadID string) {
	if session == nil {
		logger.Printf("[CLOUD-UPLOAD] Warning: skip OSS abort for multipart_upload_id=%s: missing upload session", multipartUploadID)
		return
	}
	abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refreshed, err := u.ensureFreshUploadCredentials(abortCtx, session)
	if err != nil {
		logger.Printf("[CLOUD-UPLOAD] Warning: refresh credentials before abort failed (proceeding anyway): %v", err)
	} else {
		session = refreshed
	}
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
	prepared, err := u.prepareUploadSession(ctx, hints, req.McapKey, req.AssetID, fileSize)
	if err != nil {
		return nil, fmt.Errorf("prepare upload session: %w", err)
	}
	session := prepared.session
	restartCount := prepared.restartCount

	// Fast path: OSS object is already present (COMPLETE_ONLY recovery, ETag verified).
	// Skip the multipart upload and go straight to CompleteUpload on the gateway.
	// Use ossCompletePartCount from GetUploadRecovery — the server requires completed_part_count > 0
	// and uses it for idempotency validation, so we must pass the value it previously recorded.
	if prepared.ossCompleteETag != "" {
		logger.Printf("[CLOUD-UPLOAD] OSS object already verified (COMPLETE_ONLY): logical_upload_id=%s etag=%s parts=%d",
			session.LogicalUploadID, prepared.ossCompleteETag, prepared.ossCompletePartCount)
		if err := u.gateway.CompleteUpload(ctx, session.UploadID, fileSize, req.RawTags, prepared.ossCompletePartCount, prepared.ossCompleteETag, session.PartSizeBytes); err != nil {
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
			AssetID:         req.AssetID,
			FileSize:        fileSize,
			PartSizeBytes:   session.PartSizeBytes,
			UpdatedAt:       time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("persist initial upload state: %w", err)
		}
	}

	logger.Printf("[CLOUD-UPLOAD] Session ready: logical_upload_id=%s upload_id=%s object_key=%s part_size=%d",
		session.LogicalUploadID, session.UploadID, session.ObjectKey, session.PartSizeBytes)

	// Step 3: Multipart upload
	//
	// KNOWN LIMITATION: the multipart_upload_id is only persisted AFTER uploadParts returns
	// (see the persistActiveState call below). If the process crashes during the transfer
	// window — which can span several minutes for large MCAP files — the state file on disk
	// will have an empty multipart_upload_id. On the next restart, reconcileRemoteParts is
	// skipped (nothing to probe) and the client initiates a brand-new multipart upload,
	// silently retransmitting the entire file and orphaning the previous OSS session.
	//
	// The correct fix is to persist the multipart_upload_id immediately after
	// InitiateMultipartUpload succeeds, before streaming any parts. This requires splitting
	// uploadParts into an initiate step (called here, result persisted) and a stream step.
	// The Rust SDK has the same gap; defer fixing until the upstream SDK is updated.
	session, multipartUploadID, parts, partMD5s, err := u.uploadParts(ctx, req, session, fileSize)
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
		AssetID:           req.AssetID,
		FileSize:          fileSize,
		PartSizeBytes:     session.PartSizeBytes,
		UpdatedAt:         time.Now(),
	}); err != nil {
		logger.Printf("[CLOUD-UPLOAD] Warning: failed to update state with multipart_upload_id: %v", err)
	}

	localETag := BuildMultipartETag(partMD5s)
	logger.Printf("[CLOUD-UPLOAD] OSS upload complete: parts=%d etag=%s", len(parts), localETag)

	// Step 4: Refresh STS credentials if about to expire before CompleteUpload RPC
	if time.Until(session.STSExpireAt) <= u.cfg.RequestTimeout {
		refreshed, err := u.refreshUploadCredentials(ctx, session)
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
	if err := u.gateway.CompleteUpload(ctx, session.UploadID, fileSize, req.RawTags, int32(len(parts)), localETag, session.PartSizeBytes); err != nil {
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
	ossCompletePartCount int32 // the completed_part_count recorded by the server; valid only when ossCompleteETag is set.
}

// prepareUploadSession checks for persisted state and either resumes or creates a new session.
// It mirrors the Rust SDK's prepare_upload_session logic.
func (u *Uploader) prepareUploadSession(ctx context.Context, clientHints map[string]string, mcapKey string, assetID string, fileSize int64) (preparedSession, error) {
	state, err := u.findPersistedStateByKey(mcapKey, assetID)
	if err != nil {
		return preparedSession{}, fmt.Errorf("load persisted state: %w", err)
	}

	if state != nil {
		decision, session, ossETag, ossPartCount, err := u.decideResumeAction(ctx, state, fileSize)
		if err != nil {
			return preparedSession{}, err
		}
		switch decision {
		case resumeContinue:
			return preparedSession{session: session, restartCount: state.RestartCount, persistedState: true}, nil
		case resumeOSSAlreadyComplete:
			// The OSS object is already present and its ETag matches the server's record.
			// Skip uploadParts and go straight to CompleteUpload with the server-provided ETag and
			// completed_part_count (required by the server's > 0 validation and idempotency check).
			return preparedSession{
				session:              session,
				restartCount:         state.RestartCount,
				persistedState:       true,
				ossCompleteETag:      ossETag,
				ossCompletePartCount: ossPartCount,
			}, nil
		case resumeRestart:
			if state.RestartCount >= u.cfg.MaxRestartCount {
				u.abortAndCleanupSession(ctx, state.LogicalUploadID, "restart count exceeded")
				return preparedSession{}, fmt.Errorf("upload restart count exceeded (logical_upload_id=%s)", state.LogicalUploadID)
			}
			newSession, err := u.gateway.CreateLogicalUpload(ctx, clientHints, state.UploadID)
			if err != nil {
				// RPC failed — the old state file is still on disk so the next retry can
				// re-enter this path and try again with the same logical upload ID.
				return preparedSession{}, fmt.Errorf("create logical upload (restart): %w", err)
			}
			newRestartCount := state.RestartCount + 1
			// Persist the new session state BEFORE removing the old one.
			// This ensures that a crash between the two operations always leaves at least
			// one valid state file on disk:
			//   - crash before persist(new)  → old file still present; next retry retries the restart RPC
			//   - crash after  persist(new)  → new file present; next retry continues normally
			//   - crash after  cleanup(old)  → only new file present; correct
			// Note: since new and old logical_upload_ids are different, the two files have
			// different names and can coexist without conflict.
			// This intentionally diverges from the Rust SDK (which cleans up first) to close
			// the crash-window identified in the code review.
			if err := u.persistActiveState(&persistedUploadState{
				Version:         1,
				LogicalUploadID: newSession.LogicalUploadID,
				UploadID:        newSession.UploadID,
				RestartCount:    newRestartCount,
				Bucket:          newSession.Bucket,
				Endpoint:        newSession.Endpoint,
				ObjectKey:       newSession.ObjectKey,
				McapKey:         mcapKey,
				AssetID:         assetID,
				FileSize:        fileSize,
				UpdatedAt:       time.Now(),
			}); err != nil {
				// Persistence failed — abort the new session (best-effort) so it does not
				// leak on the server, and propagate the error so the caller retries.
				u.abortAndCleanupSession(ctx, newSession.LogicalUploadID, "failed to persist restart state")
				return preparedSession{}, fmt.Errorf("persist restart state: %w", err)
			}
			u.cleanupPersistedState(state.LogicalUploadID)
			return preparedSession{session: newSession, restartCount: newRestartCount, persistedState: true}, nil
		case resumePermanentFailure:
			u.abortAndCleanupSession(ctx, state.LogicalUploadID, "upload cannot be resumed")
			return preparedSession{}, fmt.Errorf("persisted upload cannot be resumed (logical_upload_id=%s)", state.LogicalUploadID)
		default:
			// Should never happen: all resumeDecision values must be handled above.
			// Guard against future enum additions silently falling through to CreateLogicalUpload.
			return preparedSession{}, fmt.Errorf("unexpected resume decision %d (logical_upload_id=%s)", decision, state.LogicalUploadID)
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
// Returns (decision, session, ossETag, completedPartCount, error):
//   - ossETag and completedPartCount are non-zero only when decision == resumeOSSAlreadyComplete;
//     ossETag is the server-verified ETag and completedPartCount is the value recorded by the
//     server, both to be passed directly to CompleteUpload.
func (u *Uploader) decideResumeAction(ctx context.Context, state *persistedUploadState, fileSize int64) (resumeDecision, *UploadSession, string, int32, error) {
	// Verify local file size hasn't changed
	if state.FileSize != fileSize {
		return resumePermanentFailure, nil, "", 0, nil
	}

	recovery, err := u.gateway.GetUploadRecovery(ctx, state.LogicalUploadID)
	if err != nil {
		// ErrLogicalUploadNotFound means the session no longer exists on the server
		// (expired, deleted, or never created). There is nothing to recover — treat
		// as permanent failure so the caller can abort and clean up local state.
		if errors.Is(err, ErrLogicalUploadNotFound) {
			return resumePermanentFailure, nil, "", 0, nil
		}
		// All other RPC errors are treated as transient: return the error so the
		// caller preserves the local state file for the next retry.
		return resumeContinue, nil, "", 0, fmt.Errorf("GetUploadRecovery: %w", err)
	}

	switch recovery.NextAction {
	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
		pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY:

		session, err := u.gateway.ReissueUploadCredentials(ctx, recovery.CurrentUploadID)
		if err != nil {
			// Treat RPC failures as transient: preserve local state for next retry.
			return resumeContinue, nil, "", 0, fmt.Errorf("ReissueUploadCredentials: %w", err)
		}
		if state.PartSizeBytes > 0 {
			session.PartSizeBytes = state.PartSizeBytes
		}

		if state.MultipartUploadID != "" {
			outcome, err := u.reconcileRemoteParts(ctx, session, state.MultipartUploadID)
			if err != nil {
				// Non-404 OSS error (network/auth): treat as transient, preserve state for next retry.
				return resumeContinue, nil, "", 0, fmt.Errorf("reconcile remote parts: %w", err)
			}
			if outcome == reconcileRestart {
				return resumeRestart, nil, "", 0, nil
			}
		}

		if recovery.NextAction == pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY {
			outcome, err := u.reconcileCompletedObject(ctx, session, recovery.OSSObjectETag)
			if err != nil {
				// Non-404 OSS error: treat as transient, preserve state for next retry.
				return resumeContinue, nil, "", 0, fmt.Errorf("reconcile completed object: %w", err)
			}
			switch outcome {
			case reconcileCompleted:
				// OSS object already exists and ETag matches: skip re-upload, go straight to CompleteUpload.
				// Use the server-recorded completed_part_count from GetUploadRecovery to satisfy the
				// server's > 0 validation and its idempotency check on CompleteUpload.
				return resumeOSSAlreadyComplete, session, recovery.OSSObjectETag, recovery.CompletedPartCount, nil
			case reconcileRestart:
				return resumeRestart, nil, "", 0, nil
			}
		}

		return resumeContinue, session, "", 0, nil

	case pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_RESTART:
		return resumeRestart, nil, "", 0, nil

	default: // ABORT or UNSPECIFIED
		return resumePermanentFailure, nil, "", 0, nil
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
// mismatch. Other errors (network, auth, missing ETag header) are returned as errors so
// the caller can treat them as transient rather than triggering an unnecessary restart.
func (u *Uploader) reconcileCompletedObject(ctx context.Context, session *UploadSession, expectedETag string) (reconcileOutcome, error) {
	etag, err := u.oss.HeadObjectETag(ctx, session)
	if err != nil {
		if errors.Is(err, ErrOSSNotFound) {
			return reconcileRestart, nil
		}
		// ErrOSSETagMissing and other non-404 errors are transient; return an error
		// so the caller preserves local state and retries on the next sync cycle.
		return reconcileContinue, fmt.Errorf("head object: %w", err)
	}
	if etag == expectedETag {
		return reconcileCompleted, nil
	}
	return reconcileRestart, nil
}

// partStreamFactory opens a stream for a specific byte range of the MCAP file.
// Each call returns an independent io.ReadCloser so that connections are not
// kept idle across part uploads.
type partStreamFactory func(ctx context.Context, offset, length int64) (io.ReadCloser, error)

// minioRangeReader returns a partStreamFactory that reads byte ranges from
// MinIO using independent ranged GetObject requests.
func (u *Uploader) minioRangeReader(key string) partStreamFactory {
	return func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		opts := minio.GetObjectOptions{}
		if err := opts.SetRange(offset, offset+length-1); err != nil {
			return nil, fmt.Errorf("set range %d-%d: %w", offset, offset+length-1, err)
		}
		obj, err := u.minioClient.GetObject(ctx, u.minioBucket, key, opts)
		if err != nil {
			return nil, fmt.Errorf("get minio object range %d-%d: %w", offset, offset+length-1, err)
		}
		return obj, nil
	}
}

// uploadParts streams the MCAP from MinIO and uploads it to OSS in parts.
// Returns the OSS multipart upload ID, the list of uploaded parts, per-part MD5 digests, and any error.
func (u *Uploader) uploadParts(ctx context.Context, req UploadRequest, session *UploadSession, fileSize int64) (*UploadSession, string, []UploadedPart, [][16]byte, error) {
	fixedPartSizeBytes := normalizedPartSizeBytes(session.PartSizeBytes)
	session, err := u.ensureFreshUploadCredentials(ctx, session)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("refresh credentials before initiate multipart upload: %w", err)
	}
	session.PartSizeBytes = fixedPartSizeBytes

	// Initiate multipart upload on OSS
	multipartUploadID, err := u.oss.InitiateMultipartUpload(ctx, session)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("initiate multipart upload: %w", err)
	}
	logger.Printf("[CLOUD-UPLOAD] Multipart initiated: multipart_upload_id=%s", multipartUploadID)

	// Stream from MinIO to OSS in parts.
	// Each part uses an independent ranged GetObject so that the MinIO HTTP
	// connection is not left idle during OSS part uploads. A single streaming
	// response would risk idle connection timeout (~20-25s on MinIO or network
	// intermediaries) when upload speed is slow.
	session, parts, partMD5s, err := u.streamMultipartParts(ctx, req.EpisodeID, session, multipartUploadID, fileSize, fixedPartSizeBytes, u.minioRangeReader(req.McapKey))
	if err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return nil, "", nil, nil, err
	}

	session, err = u.ensureFreshUploadCredentials(ctx, session)
	if err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return nil, "", nil, nil, fmt.Errorf("refresh credentials before complete multipart upload: %w", err)
	}
	session.PartSizeBytes = fixedPartSizeBytes

	// Complete multipart upload on OSS
	if _, err := u.oss.CompleteMultipartUpload(ctx, session, multipartUploadID, parts); err != nil {
		u.abortMultipartUpload(session, multipartUploadID)
		return nil, "", nil, nil, fmt.Errorf("complete multipart upload on OSS: %w", err)
	}

	return session, multipartUploadID, parts, partMD5s, nil
}

func (u *Uploader) streamMultipartParts(ctx context.Context, episodeID string, session *UploadSession, multipartUploadID string, fileSize int64, partSizeBytes int64, newPartStream partStreamFactory) (*UploadSession, []UploadedPart, [][16]byte, error) {
	partSizeBytes = normalizedPartSizeBytes(partSizeBytes)
	session.PartSizeBytes = partSizeBytes
	partSize := int(partSizeBytes)
	if int64(partSize) != partSizeBytes {
		return session, nil, nil, fmt.Errorf("invalid part_size_bytes %d", partSizeBytes)
	}

	buf := make([]byte, partSize)
	var parts []UploadedPart
	var partMD5s [][16]byte
	var offset int64
	partNumber := 1

	for offset < fileSize {
		if err := ctx.Err(); err != nil {
			return session, nil, nil, err
		}

		remaining := fileSize - offset
		readSize := partSizeBytes
		if remaining < readSize {
			readSize = remaining
		}

		// Open a new connection for each part so that the MinIO stream is not
		// left idle during OSS uploads. MinIO or intervening network equipment
		// may drop idle streaming connections after ~20-25s, and the OSS upload
		// between part reads can easily exceed this threshold on slow networks.
		partStream, err := newPartStream(ctx, offset, readSize)
		if err != nil {
			return session, nil, nil, fmt.Errorf("open part %d stream at offset %d: %w", partNumber, offset, err)
		}

		n, readErr := io.ReadFull(partStream, buf[:int(readSize)])
		_ = partStream.Close() // close ASAP, best-effort
		if readErr != nil {
			return session, nil, nil, fmt.Errorf("read part %d from minio: expected %d bytes, got %d: %w", partNumber, readSize, n, readErr)
		}
		if int64(n) != readSize {
			return session, nil, nil, fmt.Errorf("read part %d from minio: expected %d bytes, got %d", partNumber, readSize, n)
		}

		partSlice := buf[:n]
		partMD5s = append(partMD5s, MD5DigestBytes(partSlice))

		session, err = u.ensureFreshUploadCredentials(ctx, session)
		if err != nil {
			return session, nil, nil, fmt.Errorf("refresh credentials before upload part %d: %w", partNumber, err)
		}

		etag, err := u.oss.UploadPart(ctx, session, multipartUploadID, partNumber, partSlice)
		if err != nil && isSecurityTokenExpiredError(err) {
			refreshed, refreshErr := u.refreshUploadCredentials(ctx, session)
			if refreshErr != nil {
				return session, nil, nil, fmt.Errorf("refresh credentials after upload part %d token expiry: %w", partNumber, refreshErr)
			}
			session = refreshed
			session.PartSizeBytes = partSizeBytes
			etag, err = u.oss.UploadPart(ctx, session, multipartUploadID, partNumber, partSlice)
		}
		if err != nil {
			return session, nil, nil, fmt.Errorf("upload part %d: %w", partNumber, err)
		}

		parts = append(parts, UploadedPart{
			PartNumber: partNumber,
			ETag:       etag,
		})

		offset += int64(n)
		partNumber++

		logger.Printf("[CLOUD-UPLOAD] Progress: episode=%s parts=%d offset=%d/%d",
			episodeID, len(parts), offset, fileSize)
	}

	return session, parts, partMD5s, nil
}

func (u *Uploader) ensureFreshUploadCredentials(ctx context.Context, session *UploadSession) (*UploadSession, error) {
	if session == nil {
		return nil, fmt.Errorf("missing upload session")
	}
	if time.Until(session.STSExpireAt) > u.stsRefreshWindow() {
		return session, nil
	}
	return u.refreshUploadCredentials(ctx, session)
}

func (u *Uploader) refreshUploadCredentials(ctx context.Context, session *UploadSession) (*UploadSession, error) {
	if u.gateway == nil {
		return nil, fmt.Errorf("gateway client is not configured")
	}
	refreshed, err := u.gateway.ReissueUploadCredentials(ctx, session.UploadID)
	if err != nil {
		return nil, err
	}
	refreshed.PartSizeBytes = normalizedPartSizeBytes(session.PartSizeBytes)
	return refreshed, nil
}

func normalizedPartSizeBytes(partSizeBytes int64) int64 {
	if partSizeBytes <= 0 {
		return 8 * 1024 * 1024
	}
	return partSizeBytes
}

func (u *Uploader) stsRefreshWindow() time.Duration {
	window := u.cfg.RequestTimeout
	if u.cfg.OSSTimeout > window {
		window = u.cfg.OSSTimeout
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	return window + 30*time.Second
}

func isSecurityTokenExpiredError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SecurityTokenExpired")
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

// findPersistedStateByKey scans the active state directory for a state matching the given
// MCAP key and asset id. Upload sessions are device-scoped and must not be reused
// across different Data Platform devices even when the MCAP object key is identical.
func (u *Uploader) findPersistedStateByKey(mcapKey string, assetID string) (*persistedUploadState, error) {
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
		if state.McapKey == mcapKey && state.AssetID == assetID {
			return &state, nil
		}
	}
	return nil, nil
}
