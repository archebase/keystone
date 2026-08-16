// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package mcapimport

import (
	"context"
	"crypto/md5" // #nosec G501 -- the server explicitly requires MD5 as a non-security object checksum
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DeviceCredential is the device identity used to obtain short-lived Data Gateway JWTs.
type DeviceCredential struct {
	DeviceID string
	Secret   string
}

// UploadSession contains the object-store destination and temporary credentials issued by Keystone.
type UploadSession struct {
	LogicalUploadID string
	UploadID        string
	Bucket          string
	Endpoint        string
	Region          string
	ObjectKey       string
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string
	PartSizeBytes   int64
}

// CompleteRequest contains the object facts sent to Keystone after the TOS upload completes.
type CompleteRequest struct {
	UploadID           string
	FileSize           int64
	RawTags            map[string]string
	CompletedPartCount int32
	ObjectETag         string
	PartSizeBytes      int64
}

// ObjectUploadResult describes a completed TOS multipart upload.
type ObjectUploadResult struct {
	ETag      string
	PartCount int32
}

// UploadRecovery describes the server-side completion state of a logical upload.
type UploadRecovery struct {
	Completed bool
	ETag      string
	PartCount int32
}

// Result describes a successfully imported MCAP.
type Result struct {
	LogicalUploadID string `json:"logical_upload_id"`
	UploadID        string `json:"upload_id"`
	Bucket          string `json:"bucket"`
	ObjectKey       string `json:"object_key"`
	FileSize        int64  `json:"file_size"`
	SHA256          string `json:"sha256"`
	ObjectETag      string `json:"object_etag"`
}

// ControlPlane is the subset of Keystone's Data Gateway used by the importer.
type ControlPlane interface {
	InitDevice(ctx context.Context, deviceName, authToken string) (DeviceCredential, int64, error)
	CreateLogicalUpload(ctx context.Context, credential DeviceCredential, clientHints map[string]string) (UploadSession, error)
	CompleteUpload(ctx context.Context, credential DeviceCredential, req CompleteRequest) error
	GetUploadRecovery(ctx context.Context, credential DeviceCredential, logicalUploadID string) (UploadRecovery, error)
	AbortUpload(ctx context.Context, credential DeviceCredential, logicalUploadID, reason string) error
}

// ObjectUploader uploads the file to the destination issued by the Data Gateway.
type ObjectUploader interface {
	Upload(ctx context.Context, filePath string, session UploadSession, parallel int) (ObjectUploadResult, error)
}

// Runner coordinates device authentication, Data Gateway calls, and the TOS upload.
type Runner struct {
	Control  ControlPlane
	Uploader ObjectUploader
	Progress func(format string, args ...any)
}

type fileFacts struct {
	Size   int64
	MD5    string
	SHA256 string
}

// Run imports exactly one MCAP file.
func (r Runner) Run(ctx context.Context, cfg Config) (*Result, error) {
	if r.Control == nil || r.Uploader == nil {
		return nil, fmt.Errorf("importer dependencies are not configured")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	r.logf("Computing checksums for %s", cfg.FilePath)
	facts, err := inspectFile(cfg.FilePath)
	if err != nil {
		return nil, err
	}

	credential, err := r.resolveDevice(ctx, cfg)
	if err != nil {
		return nil, err
	}
	hints, rawTags := buildUploadMetadata(cfg, facts)
	r.logf("Creating logical upload for task %s", cfg.TaskID)
	session, err := r.Control.CreateLogicalUpload(ctx, credential, hints)
	if err != nil {
		return nil, fmt.Errorf("create logical upload: %w", err)
	}
	if err := validateUploadSession(session); err != nil {
		r.abort(credential, session.LogicalUploadID)
		return nil, err
	}

	finished := false
	defer func() {
		if !finished {
			r.abort(credential, session.LogicalUploadID)
		}
	}()

	r.logf("Uploading %d bytes to tos://%s/%s", facts.Size, session.Bucket, session.ObjectKey)
	objectResult, err := r.Uploader.Upload(ctx, cfg.FilePath, session, cfg.Parallel)
	if err != nil {
		return nil, fmt.Errorf("upload MCAP to TOS: %w", err)
	}
	if objectResult.PartCount <= 0 || strings.TrimSpace(objectResult.ETag) == "" {
		return nil, fmt.Errorf("TOS upload returned incomplete result")
	}

	r.logf("Confirming upload with Keystone")
	err = r.Control.CompleteUpload(ctx, credential, CompleteRequest{
		UploadID:           session.UploadID,
		FileSize:           facts.Size,
		RawTags:            rawTags,
		CompletedPartCount: objectResult.PartCount,
		ObjectETag:         objectResult.ETag,
		PartSizeBytes:      session.PartSizeBytes,
	})
	if err != nil {
		completeErr := err
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
		recovery, recoveryErr := r.Control.GetUploadRecovery(recoveryCtx, credential, session.LogicalUploadID)
		recoveryCancel()
		if recoveryErr == nil && recovery.Completed {
			finished = true
			if strings.TrimSpace(recovery.ETag) != strings.TrimSpace(objectResult.ETag) || recovery.PartCount != objectResult.PartCount {
				return nil, fmt.Errorf(
					"completed upload recovery facts differ from local upload result (logical_upload_id=%s upload_id=%s)",
					session.LogicalUploadID,
					session.UploadID,
				)
			}
			r.logf("Keystone reports the upload was already completed")
		} else {
			if isIndeterminateCompletionError(completeErr) {
				// The server may have committed the Episode before the response was lost.
				// Keep the logical upload recoverable instead of incorrectly aborting it.
				finished = true
			}
			if recoveryErr != nil {
				return nil, fmt.Errorf(
					"complete logical upload: %w (recovery query failed: %v; logical_upload_id=%s upload_id=%s)",
					completeErr,
					recoveryErr,
					session.LogicalUploadID,
					session.UploadID,
				)
			}
			return nil, fmt.Errorf(
				"complete logical upload: %w (server does not report completion; logical_upload_id=%s upload_id=%s)",
				completeErr,
				session.LogicalUploadID,
				session.UploadID,
			)
		}
	}
	finished = true

	return &Result{
		LogicalUploadID: session.LogicalUploadID,
		UploadID:        session.UploadID,
		Bucket:          session.Bucket,
		ObjectKey:       session.ObjectKey,
		FileSize:        facts.Size,
		SHA256:          facts.SHA256,
		ObjectETag:      objectResult.ETag,
	}, nil
}

func (r Runner) resolveDevice(ctx context.Context, cfg Config) (DeviceCredential, error) {
	if strings.TrimSpace(cfg.DeviceID) != "" {
		return DeviceCredential{DeviceID: strings.TrimSpace(cfg.DeviceID), Secret: strings.TrimSpace(cfg.DeviceCredential)}, nil
	}
	r.logf("Initializing device %s", cfg.DeviceName)
	credential, workspaceID, err := r.Control.InitDevice(ctx, cfg.DeviceName, cfg.DeviceAuthToken)
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("initialize device: %w", err)
	}
	if strings.TrimSpace(credential.DeviceID) == "" || strings.TrimSpace(credential.Secret) == "" {
		return DeviceCredential{}, fmt.Errorf("device initialization returned incomplete credentials")
	}
	if workspaceID != cfg.WorkspaceID {
		return DeviceCredential{}, fmt.Errorf("initialized device belongs to workspace %d, not %d", workspaceID, cfg.WorkspaceID)
	}
	if err := saveDeviceCredential(cfg.CredentialsFile, credential, workspaceID); err != nil {
		return DeviceCredential{}, err
	}
	r.logf("Saved device credentials to %s", cfg.CredentialsFile)
	return credential, nil
}

func isIndeterminateCompletionError(err error) bool {
	code := status.Code(err)
	return code == codes.Unavailable || code == codes.DeadlineExceeded ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (r Runner) abort(credential DeviceCredential, logicalUploadID string) {
	if strings.TrimSpace(logicalUploadID) == "" {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Control.AbortUpload(abortCtx, credential, logicalUploadID, "keystone-import-mcap failed"); err != nil {
		r.logf("Warning: failed to abort logical upload %s: %v", logicalUploadID, err)
	}
}

func (r Runner) logf(format string, args ...any) {
	if r.Progress != nil {
		r.Progress(format, args...)
	}
}

func inspectFile(path string) (fileFacts, error) {
	// #nosec G304 -- path is explicitly supplied by the CLI operator.
	file, err := os.Open(path)
	if err != nil {
		return fileFacts{}, fmt.Errorf("open MCAP file: %w", err)
	}

	md5Hash := md5.New() // #nosec G401 -- compatibility checksum, not a security primitive
	sha256Hash := sha256.New()
	written, readErr := io.Copy(io.MultiWriter(md5Hash, sha256Hash), file)
	closeErr := file.Close()
	if readErr != nil {
		return fileFacts{}, errors.Join(fmt.Errorf("read MCAP file: %w", readErr), wrapCloseError("MCAP file", closeErr))
	}
	if closeErr != nil {
		return fileFacts{}, fmt.Errorf("close MCAP file: %w", closeErr)
	}
	return fileFacts{
		Size:   written,
		MD5:    hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256: hex.EncodeToString(sha256Hash.Sum(nil)),
	}, nil
}

func buildUploadMetadata(cfg Config, facts fileFacts) (map[string]string, map[string]string) {
	workspaceID := strconv.FormatInt(cfg.WorkspaceID, 10)
	dcPlanID := strconv.FormatInt(cfg.DCPlanID, 10)
	hints := map[string]string{
		"product":         "ego_portal_lite",
		"source":          "keystone_import_cli",
		"capture_id":      strings.TrimSpace(cfg.CaptureID),
		"task_id":         strings.TrimSpace(cfg.TaskID),
		"dc_plan_id":      dcPlanID,
		"workspace_id":    workspaceID,
		"checksum_md5":    facts.MD5,
		"checksum_sha256": facts.SHA256,
	}
	if cameraSerial := strings.TrimSpace(cfg.CameraSerial); cameraSerial != "" {
		hints["camera_serial"] = cameraSerial
	}
	rawTags := make(map[string]string, len(hints))
	for key, value := range hints {
		rawTags[key] = value
	}
	if cfg.DurationSec > 0 {
		rawTags["duration_sec"] = strconv.FormatFloat(cfg.DurationSec, 'f', -1, 64)
	}
	return hints, rawTags
}

func validateUploadSession(session UploadSession) error {
	switch {
	case strings.TrimSpace(session.LogicalUploadID) == "":
		return fmt.Errorf("create logical upload returned no logical upload ID")
	case strings.TrimSpace(session.UploadID) == "":
		return fmt.Errorf("create logical upload returned no upload ID")
	case strings.TrimSpace(session.Bucket) == "":
		return fmt.Errorf("create logical upload returned no bucket")
	case strings.TrimSpace(session.Endpoint) == "":
		return fmt.Errorf("create logical upload returned no TOS endpoint")
	case strings.TrimSpace(session.Region) == "":
		return fmt.Errorf("create logical upload returned no TOS region")
	case strings.TrimSpace(session.ObjectKey) == "":
		return fmt.Errorf("create logical upload returned no object key")
	case strings.TrimSpace(session.AccessKeyID) == "" || strings.TrimSpace(session.AccessKeySecret) == "" || strings.TrimSpace(session.SecurityToken) == "":
		return fmt.Errorf("create logical upload returned incomplete STS credentials")
	case session.PartSizeBytes <= 0:
		return fmt.Errorf("create logical upload returned invalid part size")
	default:
		return nil
	}
}

func saveDeviceCredential(path string, credential DeviceCredential, workspaceID int64) (retErr error) {
	payload, err := json.MarshalIndent(persistedDeviceCredential{
		DeviceID:     credential.DeviceID,
		DeviceAPIKey: credential.Secret,
		WorkspaceID:  workspaceID,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode device credentials: %w", err)
	}
	// #nosec G304 -- path is explicitly supplied by the CLI operator.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create device credentials file: %w", err)
	}
	removeIncomplete := true
	defer func() {
		if removeIncomplete {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				retErr = errors.Join(retErr, fmt.Errorf("remove incomplete device credentials file: %w", removeErr))
			}
		}
	}()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		writeErr := fmt.Errorf("write device credentials file: %w", err)
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("close incomplete device credentials file: %w", closeErr))
		}
		return writeErr
	}
	if err := file.Sync(); err != nil {
		syncErr := fmt.Errorf("sync device credentials file: %w", err)
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("close incomplete device credentials file: %w", closeErr))
		}
		return syncErr
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close device credentials file: %w", err)
	}
	removeIncomplete = false
	return nil
}
