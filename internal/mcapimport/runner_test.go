// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package mcapimport

import (
	"context"
	"crypto/md5" // #nosec G501 -- test verifies the required compatibility checksum
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunnerUploadsAndCompletes(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{session: validUploadSession()}
	uploader := &fakeObjectUploader{result: ObjectUploadResult{ETag: "object-etag", PartCount: 2}}

	result, err := (Runner{Control: control, Uploader: uploader}).Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ObjectKey != control.session.ObjectKey || result.ObjectETag != "object-etag" {
		t.Fatalf("Run() result = %+v", result)
	}
	if uploader.filePath != cfg.FilePath || uploader.parallel != cfg.Parallel {
		t.Fatalf("uploader inputs = path %q parallel %d", uploader.filePath, uploader.parallel)
	}
	if control.abortCalls != 0 {
		t.Fatalf("AbortUpload() calls = %d, want 0", control.abortCalls)
	}

	contents, err := os.ReadFile(cfg.FilePath)
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	md5Digest := md5.Sum(contents) // #nosec G401 -- compatibility checksum test
	sha256Digest := sha256.Sum256(contents)
	wantHints := map[string]string{
		"product":         "ego_portal_lite",
		"source":          "keystone_import_cli",
		"capture_id":      cfg.CaptureID,
		"task_id":         cfg.TaskID,
		"dc_plan_id":      "41",
		"workspace_id":    "3",
		"checksum_md5":    hex.EncodeToString(md5Digest[:]),
		"checksum_sha256": hex.EncodeToString(sha256Digest[:]),
	}
	if !reflect.DeepEqual(control.hints, wantHints) {
		t.Fatalf("client hints = %#v, want %#v", control.hints, wantHints)
	}
	if !reflect.DeepEqual(control.complete.RawTags, wantHints) {
		t.Fatalf("raw tags = %#v, want %#v", control.complete.RawTags, wantHints)
	}
	if control.complete.FileSize != int64(len(contents)) || control.complete.CompletedPartCount != 2 {
		t.Fatalf("complete request = %+v", control.complete)
	}
	if got := control.calls; !reflect.DeepEqual(got, []string{"create", "complete"}) {
		t.Fatalf("control calls = %v", got)
	}
}

func TestBuildUploadMetadataIncludesCameraSerial(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.CameraSerial = "  CAMERA-SN-001  "
	cfg.DurationSec = 12.345
	facts := fileFacts{MD5: "md5", SHA256: "sha256"}

	hints, rawTags := buildUploadMetadata(cfg, facts)
	if hints["camera_serial"] != "CAMERA-SN-001" {
		t.Fatalf("camera_serial hint = %q, want normalized serial", hints["camera_serial"])
	}
	if rawTags["camera_serial"] != "CAMERA-SN-001" {
		t.Fatalf("camera_serial raw tag = %q, want normalized serial", rawTags["camera_serial"])
	}
	if rawTags["duration_sec"] != "12.345" {
		t.Fatalf("duration_sec raw tag = %q, want 12.345", rawTags["duration_sec"])
	}
}

func TestRunnerAbortsWhenObjectUploadFails(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{session: validUploadSession()}
	uploader := &fakeObjectUploader{err: errors.New("TOS unavailable")}

	_, err := (Runner{Control: control, Uploader: uploader}).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if control.abortCalls != 1 || control.abortedID != control.session.LogicalUploadID {
		t.Fatalf("abort calls = %d, id = %q", control.abortCalls, control.abortedID)
	}
	if got := control.calls; !reflect.DeepEqual(got, []string{"create", "abort"}) {
		t.Fatalf("control calls = %v", got)
	}
}

func TestRunnerAbortsWhenCompleteFails(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{session: validUploadSession(), completeErr: errors.New("complete unavailable")}
	uploader := &fakeObjectUploader{result: ObjectUploadResult{ETag: "object-etag", PartCount: 1}}

	_, err := (Runner{Control: control, Uploader: uploader}).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if control.abortCalls != 1 {
		t.Fatalf("AbortUpload() calls = %d, want 1", control.abortCalls)
	}
	if control.recoveryCalls != 1 {
		t.Fatalf("GetUploadRecovery() calls = %d, want 1", control.recoveryCalls)
	}
}

func TestRunnerAcceptsCompletedUploadAfterLostCompleteResponse(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{
		session:     validUploadSession(),
		completeErr: status.Error(codes.Unavailable, "response lost"),
		recovery: UploadRecovery{
			Completed: true,
			ETag:      "object-etag",
			PartCount: 1,
		},
	}
	uploader := &fakeObjectUploader{result: ObjectUploadResult{ETag: "object-etag", PartCount: 1}}

	result, err := (Runner{Control: control, Uploader: uploader}).Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ObjectETag != "object-etag" || control.abortCalls != 0 {
		t.Fatalf("result = %+v, abort calls = %d", result, control.abortCalls)
	}
}

func TestRunnerDoesNotAbortWhenCompleteOutcomeIsUnknown(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{
		session:     validUploadSession(),
		completeErr: status.Error(codes.DeadlineExceeded, "response lost"),
		recoveryErr: status.Error(codes.Unavailable, "recovery unavailable"),
	}
	uploader := &fakeObjectUploader{result: ObjectUploadResult{ETag: "object-etag", PartCount: 1}}

	_, err := (Runner{Control: control, Uploader: uploader}).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if control.abortCalls != 0 {
		t.Fatalf("AbortUpload() calls = %d, want 0", control.abortCalls)
	}
}

func TestRunnerQueriesRecoveryAfterCallerContextCanceled(t *testing.T) {
	cfg := validTestConfig(t)
	control := &fakeControlPlane{
		session:     validUploadSession(),
		completeErr: context.Canceled,
		recovery: UploadRecovery{
			Completed: true,
			ETag:      "object-etag",
			PartCount: 1,
		},
	}
	uploader := &fakeObjectUploader{result: ObjectUploadResult{ETag: "object-etag", PartCount: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := (Runner{Control: control, Uploader: uploader}).Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ObjectETag != "object-etag" || control.recoveryContextErr != nil {
		t.Fatalf("result = %+v, recovery context error = %v", result, control.recoveryContextErr)
	}
}

func TestRunnerDoesNotPersistDeviceCredentialsForWrongWorkspace(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.DeviceID = ""
	cfg.DeviceCredential = ""
	cfg.DeviceName = "robot-1"
	cfg.DeviceAuthToken = "one-time-token"
	cfg.CredentialsFile = filepath.Join(t.TempDir(), "device.json")
	control := &fakeControlPlane{
		session:        validUploadSession(),
		initCredential: DeviceCredential{DeviceID: "17", Secret: "generated-secret"},
		initWorkspace:  99,
	}

	_, err := (Runner{Control: control, Uploader: &fakeObjectUploader{}}).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if got := control.calls; !reflect.DeepEqual(got, []string{"init"}) {
		t.Fatalf("control calls = %v", got)
	}
	if _, statErr := os.Stat(cfg.CredentialsFile); !os.IsNotExist(statErr) {
		t.Fatalf("credentials file stat error = %v, want not exist", statErr)
	}
}

type fakeControlPlane struct {
	session            UploadSession
	initCredential     DeviceCredential
	initWorkspace      int64
	completeErr        error
	recovery           UploadRecovery
	recoveryErr        error
	hints              map[string]string
	complete           CompleteRequest
	calls              []string
	abortCalls         int
	abortedID          string
	recoveryCalls      int
	recoveryContextErr error
}

func (f *fakeControlPlane) InitDevice(_ context.Context, _, _ string) (DeviceCredential, int64, error) {
	f.calls = append(f.calls, "init")
	return f.initCredential, f.initWorkspace, nil
}

func (f *fakeControlPlane) CreateLogicalUpload(_ context.Context, _ DeviceCredential, hints map[string]string) (UploadSession, error) {
	f.calls = append(f.calls, "create")
	f.hints = cloneStringMap(hints)
	return f.session, nil
}

func (f *fakeControlPlane) CompleteUpload(_ context.Context, _ DeviceCredential, req CompleteRequest) error {
	f.calls = append(f.calls, "complete")
	req.RawTags = cloneStringMap(req.RawTags)
	f.complete = req
	return f.completeErr
}

func (f *fakeControlPlane) GetUploadRecovery(ctx context.Context, _ DeviceCredential, _ string) (UploadRecovery, error) {
	f.recoveryCalls++
	f.recoveryContextErr = ctx.Err()
	if f.recoveryContextErr != nil {
		return UploadRecovery{}, f.recoveryContextErr
	}
	return f.recovery, f.recoveryErr
}

func (f *fakeControlPlane) AbortUpload(_ context.Context, _ DeviceCredential, logicalUploadID, _ string) error {
	f.calls = append(f.calls, "abort")
	f.abortCalls++
	f.abortedID = logicalUploadID
	return nil
}

type fakeObjectUploader struct {
	result   ObjectUploadResult
	err      error
	filePath string
	parallel int
}

func (f *fakeObjectUploader) Upload(_ context.Context, filePath string, _ UploadSession, parallel int) (ObjectUploadResult, error) {
	f.filePath = filePath
	f.parallel = parallel
	return f.result, f.err
}

func validUploadSession() UploadSession {
	return UploadSession{
		LogicalUploadID: "logical-1",
		UploadID:        "upload-1",
		Bucket:          "bucket-1",
		Endpoint:        "https://tos-cn-beijing.volces.com",
		Region:          "cn-beijing",
		ObjectKey:       "device-uploads/17/capture-1/upload-1/capture.mcap",
		AccessKeyID:     "sts-access-key",
		AccessKeySecret: "sts-secret-key",
		SecurityToken:   "sts-token",
		PartSizeBytes:   8 * 1024 * 1024,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
