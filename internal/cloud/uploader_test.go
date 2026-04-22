// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "archebase.com/keystone-edge/internal/cloud/cloudpb"
)

// --- persistedUploadState helpers ---

func writeTempState(t *testing.T, dir string, state *persistedUploadState) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdirAll %s: %v", dir, err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	p := filepath.Join(dir, state.LogicalUploadID+".json")
	if err := os.WriteFile(p, data, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write state %s: %v", p, err)
	}
	return p
}

func newTestUploader(persistRootDir string) *Uploader {
	return &Uploader{
		oss: NewOSSUploader(5 * time.Second),
		cfg: UploaderConfig{
			RequestTimeout:  5 * time.Second,
			OSSTimeout:      5 * time.Second,
			PersistRootDir:  persistRootDir,
			MaxRestartCount: 3,
		},
	}
}

// TestPersistAndCleanupActiveState verifies round-trip write + remove from active/.
// Mirrors Rust abort_upload_cleans_persisted_state.
func TestPersistAndCleanupActiveState(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	state := &persistedUploadState{
		Version:         1,
		LogicalUploadID: "logical-persist-test",
		UploadID:        "upload-persist-test",
		RestartCount:    0,
		McapKey:         "episodes/42/test.mcap",
		FileSize:        128,
		Bucket:          "bucket",
		Endpoint:        "http://127.0.0.1:9000",
		ObjectKey:       "uploads/42/test",
		UpdatedAt:       time.Now(),
	}
	if err := u.persistActiveState(state); err != nil {
		t.Fatalf("persistActiveState: %v", err)
	}

	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	stateFile := filepath.Join(activeDir, "logical-persist-test.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	u.cleanupPersistedState("logical-persist-test")
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatal("state file should be removed after cleanup")
	}
}

// TestCleanupPersistedState_ClearsAllSubdirs verifies that cleanupPersistedState removes
// state files from active/, terminal/, and completed/ directories.
// Mirrors Rust SDK cleanup_persisted_state which iterates all three subdirectories.
func TestCleanupPersistedState_ClearsAllSubdirs(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)
	logicalID := "logical-multi-dir"
	base := filepath.Join(dir, "data-gateway-client", "uploads")

	// Create a state file in each of the three subdirectories.
	for _, subdir := range []string{"active", "terminal", "completed"} {
		d := filepath.Join(base, subdir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		p := filepath.Join(d, logicalID+".json")
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write %s: %v", p, err)
		}
	}

	u.cleanupPersistedState(logicalID)

	for _, subdir := range []string{"active", "terminal", "completed"} {
		p := filepath.Join(base, subdir, logicalID+".json")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("state file in %s/ should be removed after cleanup", subdir)
		}
	}
}

// TestFindPersistedStateByKey verifies lookup by mcap key.
func TestFindPersistedStateByKey(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	writeTempState(t, activeDir, &persistedUploadState{
		Version:         1,
		LogicalUploadID: "logical-find-test",
		UploadID:        "upload-find-test",
		McapKey:         "episodes/7/find.mcap",
		FileSize:        256,
		UpdatedAt:       time.Now(),
	})

	got, err := u.findPersistedStateByKey("episodes/7/find.mcap")
	if err != nil {
		t.Fatalf("findPersistedStateByKey: %v", err)
	}
	if got == nil {
		t.Fatal("expected to find persisted state, got nil")
	}
	if got.LogicalUploadID != "logical-find-test" {
		t.Errorf("logical_upload_id = %q, want %q", got.LogicalUploadID, "logical-find-test")
	}
}

// TestFindPersistedStateByKey_NotFound verifies nil is returned for unknown keys.
func TestFindPersistedStateByKey_NotFound(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	got, err := u.findPersistedStateByKey("episodes/99/missing.mcap")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestFindPersistedStateByKey_EmptyPersistRootDir verifies persistence is disabled when no root dir.
func TestFindPersistedStateByKey_EmptyPersistRootDir(t *testing.T) {
	u := newTestUploader("")

	got, err := u.findPersistedStateByKey("episodes/1/file.mcap")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil when PersistRootDir empty, got %+v", got)
	}
}

// TestCleanupPersistedState_NoopWhenMissing verifies cleanup doesn't error on missing files.
func TestCleanupPersistedState_NoopWhenMissing(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)
	// Should not panic or return error
	u.cleanupPersistedState("logical-does-not-exist")
}

// TestPersistActiveState_EmptyPersistRootDir verifies no-op when disabled.
func TestPersistActiveState_EmptyPersistRootDir(t *testing.T) {
	u := newTestUploader("")
	if err := u.persistActiveState(&persistedUploadState{
		LogicalUploadID: "ignored",
		McapKey:         "ignored.mcap",
	}); err != nil {
		t.Fatalf("expected no-op persist with empty PersistRootDir, got: %v", err)
	}
}

// TestPersistedStateRoundTrip verifies JSON serialisation preserves all fields.
func TestPersistedStateRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	original := &persistedUploadState{
		Version:           1,
		LogicalUploadID:   "logical-rt",
		UploadID:          "upload-rt",
		RestartCount:      2,
		MultipartUploadID: "multipart-rt",
		Bucket:            "my-bucket",
		Endpoint:          "https://oss.example.com",
		ObjectKey:         "uploads/1/test",
		McapKey:           "episodes/1/test.mcap",
		FileSize:          4096,
		UpdatedAt:         now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded persistedUploadState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.LogicalUploadID != original.LogicalUploadID {
		t.Errorf("LogicalUploadID = %q, want %q", decoded.LogicalUploadID, original.LogicalUploadID)
	}
	if decoded.RestartCount != original.RestartCount {
		t.Errorf("RestartCount = %d, want %d", decoded.RestartCount, original.RestartCount)
	}
	if decoded.MultipartUploadID != original.MultipartUploadID {
		t.Errorf("MultipartUploadID = %q, want %q", decoded.MultipartUploadID, original.MultipartUploadID)
	}
	if decoded.FileSize != original.FileSize {
		t.Errorf("FileSize = %d, want %d", decoded.FileSize, original.FileSize)
	}
	if decoded.McapKey != original.McapKey {
		t.Errorf("McapKey = %q, want %q", decoded.McapKey, original.McapKey)
	}
}

// TestPrepareUploadSession_PermanentFailure_FileSizeMismatch verifies that a persisted state
// with a different file size triggers permanent failure.
// Mirrors Rust prepare_upload_session_returns_permanent_failure_when_source_missing.
func TestPrepareUploadSession_PermanentFailure_FileSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)
	u.cfg.MaxRestartCount = 3

	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	writeTempState(t, activeDir, &persistedUploadState{
		Version:         1,
		LogicalUploadID: "logical-size-mismatch",
		UploadID:        "upload-size-mismatch",
		McapKey:         "episodes/1/mismatch.mcap",
		FileSize:        1024, // persisted as 1024
		UpdatedAt:       time.Now(),
	})

	// Provide a different file size — no gateway needed, should fail at size check
	_, err := u.prepareUploadSession(
		context.Background(),
		map[string]string{},
		"episodes/1/mismatch.mcap",
		512, // actual size differs
	)
	if err == nil {
		t.Fatal("expected error for file size mismatch, got nil")
	}
}

// TestPrepareUploadSession_PermanentFailure_CleanupOnSizeMismatch verifies that when a
// persisted state has a mismatched file size, the state file is cleaned up and an error
// is returned (permanent failure path, no gateway required).
// Mirrors the permanent-failure + cleanup semantics in the Rust SDK.
func TestPrepareUploadSession_PermanentFailure_CleanupOnSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	stateFile := writeTempState(t, activeDir, &persistedUploadState{
		Version:         1,
		LogicalUploadID: "logical-cleanup-mismatch",
		UploadID:        "upload-cleanup-mismatch",
		McapKey:         "episodes/2/cleanup.mcap",
		FileSize:        1024, // persisted size
		RestartCount:    0,
		UpdatedAt:       time.Now(),
	})

	_, err := u.prepareUploadSession(
		context.Background(),
		map[string]string{},
		"episodes/2/cleanup.mcap",
		512, // different from persisted
	)
	if err == nil {
		t.Fatal("expected error for size mismatch, got nil")
	}

	// State file must be cleaned up on permanent failure
	if _, statErr := os.Stat(stateFile); !os.IsNotExist(statErr) {
		t.Error("state file should be cleaned up on permanent failure")
	}
}

// TestDecideResumeAction_FileSizeMismatch_ReturnsPermanentFailure verifies that when the
// persisted file size differs from the current size, decideResumeAction returns
// resumePermanentFailure with zero ossCompletePartCount, without contacting the gateway.
func TestDecideResumeAction_FileSizeMismatch_ReturnsPermanentFailure(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	state := &persistedUploadState{
		Version:         1,
		LogicalUploadID: "logical-size-check",
		UploadID:        "upload-size-check",
		McapKey:         "episodes/3/check.mcap",
		FileSize:        2048,
		UpdatedAt:       time.Now(),
	}

	decision, session, ossETag, partCount, err := u.decideResumeAction(
		context.Background(), state, 1024, // different size
	)
	if err != nil {
		t.Fatalf("expected no error for size mismatch path, got: %v", err)
	}
	if decision != resumePermanentFailure {
		t.Errorf("decision = %v, want resumePermanentFailure", decision)
	}
	if session != nil {
		t.Errorf("session should be nil on permanent failure, got %+v", session)
	}
	if ossETag != "" {
		t.Errorf("ossETag should be empty on permanent failure, got %q", ossETag)
	}
	if partCount != 0 {
		t.Errorf("ossCompletePartCount should be 0 on permanent failure, got %d", partCount)
	}
}

// TestValidatePersistDir_CreatesDirectoryAndProbe verifies that validatePersistDir creates
// the active state directory and does not leave behind a probe file.
func TestValidatePersistDir_CreatesDirectoryAndProbe(t *testing.T) {
	dir := t.TempDir()
	u := newTestUploader(dir)

	if err := u.validatePersistDir(); err != nil {
		t.Fatalf("validatePersistDir failed on a writable temp dir: %v", err)
	}

	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("active state dir should exist after validatePersistDir: %v", err)
	}

	// Probe file must be removed
	probe := filepath.Join(activeDir, ".write-probe")
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Error("probe file should be removed after validatePersistDir")
	}
}

// TestValidatePersistDir_FailsOnUnwritableDirectory verifies that validatePersistDir returns
// an error when the active state directory exists but cannot be written to.
func TestValidatePersistDir_FailsOnUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping unwritable-dir test: running as root")
	}

	dir := t.TempDir()
	// Pre-create the full directory path so MkdirAll inside validatePersistDir succeeds,
	// then make it read-only so the probe write fails.
	activeDir := filepath.Join(dir, "data-gateway-client", "uploads", "active")
	if err := os.MkdirAll(activeDir, 0o750); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	if err := os.Chmod(activeDir, 0o555); err != nil { // read+exec only
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(activeDir, 0o750) }) // restore so TempDir cleanup works

	u := newTestUploader(dir)
	if err := u.validatePersistDir(); err == nil {
		t.Error("expected error for unwritable directory, got nil")
	}
}

// =============================================================================
// Fakes for decideResumeAction unit tests
// =============================================================================

// fakeGateway is a configurable fake implementation of gatewayClient.
// Only the methods exercised by decideResumeAction are meaningful here;
// the rest panic if called so that tests fail loudly on unexpected invocations.
type fakeGateway struct {
	// getUploadRecoveryFn is called by GetUploadRecovery; must be set for tests that reach it.
	getUploadRecoveryFn func(ctx context.Context, logicalUploadID string) (*UploadRecoveryInfo, error)
	// reissueCredentialsFn is called by ReissueUploadCredentials; must be set for tests that reach it.
	reissueCredentialsFn func(ctx context.Context, uploadID string) (*UploadSession, error)
}

func (f *fakeGateway) GetUploadRecovery(ctx context.Context, logicalUploadID string) (*UploadRecoveryInfo, error) {
	if f.getUploadRecoveryFn == nil {
		panic("fakeGateway.GetUploadRecovery called unexpectedly")
	}
	return f.getUploadRecoveryFn(ctx, logicalUploadID)
}

func (f *fakeGateway) ReissueUploadCredentials(ctx context.Context, uploadID string) (*UploadSession, error) {
	if f.reissueCredentialsFn == nil {
		panic("fakeGateway.ReissueUploadCredentials called unexpectedly")
	}
	return f.reissueCredentialsFn(ctx, uploadID)
}

func (f *fakeGateway) CreateLogicalUpload(_ context.Context, _ map[string]string, _ string) (*UploadSession, error) {
	panic("fakeGateway.CreateLogicalUpload called unexpectedly")
}

func (f *fakeGateway) AbortUpload(_ context.Context, _ string, _ string) error {
	// best-effort; silently succeed so permanent-failure tests can proceed.
	return nil
}

func (f *fakeGateway) CompleteUpload(_ context.Context, _ string, _ int64, _ map[string]string, _ int32, _ string) error {
	panic("fakeGateway.CompleteUpload called unexpectedly")
}

// fakeOSS is a configurable fake implementation of ossClient.
type fakeOSS struct {
	// listPartsFn is called by ListParts; must be set for tests that reach it.
	listPartsFn func(ctx context.Context, session *UploadSession, multipartUploadID string) error
	// headObjectETagFn is called by HeadObjectETag; must be set for tests that reach it.
	headObjectETagFn func(ctx context.Context, session *UploadSession) (string, error)
}

func (f *fakeOSS) ListParts(ctx context.Context, session *UploadSession, multipartUploadID string) error {
	if f.listPartsFn == nil {
		panic("fakeOSS.ListParts called unexpectedly")
	}
	return f.listPartsFn(ctx, session, multipartUploadID)
}

func (f *fakeOSS) HeadObjectETag(ctx context.Context, session *UploadSession) (string, error) {
	if f.headObjectETagFn == nil {
		panic("fakeOSS.HeadObjectETag called unexpectedly")
	}
	return f.headObjectETagFn(ctx, session)
}

func (f *fakeOSS) InitiateMultipartUpload(_ context.Context, _ *UploadSession) (string, error) {
	panic("fakeOSS.InitiateMultipartUpload called unexpectedly")
}

func (f *fakeOSS) UploadPart(_ context.Context, _ *UploadSession, _ string, _ int, _ []byte) (string, error) {
	panic("fakeOSS.UploadPart called unexpectedly")
}

func (f *fakeOSS) CompleteMultipartUpload(_ context.Context, _ *UploadSession, _ string, _ []UploadedPart) (string, error) {
	panic("fakeOSS.CompleteMultipartUpload called unexpectedly")
}

func (f *fakeOSS) AbortMultipartUpload(_ context.Context, _ *UploadSession, _ string) {}

// newDecideResumeUploader builds an Uploader wired with the given fakes for
// decideResumeAction tests. gateway and oss must not be nil.
func newDecideResumeUploader(dir string, gw gatewayClient, oss ossClient) *Uploader {
	return &Uploader{
		gateway: gw,
		oss:     oss,
		cfg: UploaderConfig{
			RequestTimeout:  5 * time.Second,
			OSSTimeout:      5 * time.Second,
			PersistRootDir:  dir,
			MaxRestartCount: 3,
		},
	}
}

// makeSession returns a minimal UploadSession for use in fake responses.
func makeSession(logicalID, uploadID string) *UploadSession {
	return &UploadSession{
		LogicalUploadID: logicalID,
		UploadID:        uploadID,
		Bucket:          "test-bucket",
		Endpoint:        "http://127.0.0.1:9000",
		ObjectKey:       "uploads/test/obj",
		STSExpireAt:     time.Now().Add(1 * time.Hour),
		PartSizeBytes:   8 * 1024 * 1024,
	}
}

// =============================================================================
// decideResumeAction unit tests
// =============================================================================

// TestDecideResumeAction_LogicalUploadNotFound_ReturnsPermanentFailure verifies that when
// GetUploadRecovery returns ErrLogicalUploadNotFound, the decision is resumePermanentFailure
// and no error is propagated (the caller should clean up and start fresh).
func TestDecideResumeAction_LogicalUploadNotFound_ReturnsPermanentFailure(t *testing.T) {
	dir := t.TempDir()
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return nil, ErrLogicalUploadNotFound
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID: "logical-not-found",
		UploadID:        "upload-not-found",
		FileSize:        512,
	}
	decision, session, ossETag, partCount, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("expected no error for NotFound path, got: %v", err)
	}
	if decision != resumePermanentFailure {
		t.Errorf("decision = %v, want resumePermanentFailure", decision)
	}
	if session != nil || ossETag != "" || partCount != 0 {
		t.Errorf("expected zero values on permanent failure, got session=%v etag=%q partCount=%d", session, ossETag, partCount)
	}
}

// TestDecideResumeAction_TransientRPCError_ReturnsContinueWithError verifies that a generic
// RPC error from GetUploadRecovery is treated as transient: the decision is resumeContinue
// and the error is propagated so the caller preserves local state for the next retry.
func TestDecideResumeAction_TransientRPCError_ReturnsContinueWithError(t *testing.T) {
	dir := t.TempDir()
	rpcErr := errors.New("rpc: connection refused")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return nil, rpcErr
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID: "logical-transient",
		UploadID:        "upload-transient",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err == nil {
		t.Fatal("expected error for transient RPC failure, got nil")
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
}

// TestDecideResumeAction_ServerSaysRestart_ReturnsRestart verifies that when the server
// returns UPLOAD_RECOVERY_ACTION_RESTART, the decision is resumeRestart.
func TestDecideResumeAction_ServerSaysRestart_ReturnsRestart(t *testing.T) {
	dir := t.TempDir()
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-restart",
				CurrentUploadID: "upload-restart",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_RESTART,
			}, nil
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID: "logical-restart",
		UploadID:        "upload-restart",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeRestart {
		t.Errorf("decision = %v, want resumeRestart", decision)
	}
}

// TestDecideResumeAction_ServerSaysAbort_ReturnsPermanentFailure verifies that when the
// server returns UPLOAD_RECOVERY_ACTION_ABORT (or UNSPECIFIED), the decision is
// resumePermanentFailure.
func TestDecideResumeAction_ServerSaysAbort_ReturnsPermanentFailure(t *testing.T) {
	dir := t.TempDir()
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-abort",
				CurrentUploadID: "upload-abort",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_ABORT,
			}, nil
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID: "logical-abort",
		UploadID:        "upload-abort",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumePermanentFailure {
		t.Errorf("decision = %v, want resumePermanentFailure", decision)
	}
}

// TestDecideResumeAction_Continue_NoMultipart_ReturnsContinue verifies the happy path where
// the server says CONTINUE, there is no persisted multipart upload ID, and credentials are
// reissued successfully. The decision should be resumeContinue with a valid session.
func TestDecideResumeAction_Continue_NoMultipart_ReturnsContinue(t *testing.T) {
	dir := t.TempDir()
	sess := makeSession("logical-cont", "upload-cont")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-cont",
				CurrentUploadID: "upload-cont",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID:   "logical-cont",
		UploadID:          "upload-cont",
		MultipartUploadID: "", // no multipart yet
		FileSize:          512,
	}
	decision, gotSession, ossETag, partCount, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
	if gotSession == nil || gotSession.UploadID != "upload-cont" {
		t.Errorf("expected valid session, got %+v", gotSession)
	}
	if ossETag != "" || partCount != 0 {
		t.Errorf("expected empty ossETag and zero partCount, got etag=%q partCount=%d", ossETag, partCount)
	}
}

// TestDecideResumeAction_Continue_MultipartStale_ReturnsRestart verifies that when the
// server says CONTINUE but ListParts returns ErrOSSNotFound (stale multipart upload ID),
// the decision is resumeRestart.
func TestDecideResumeAction_Continue_MultipartStale_ReturnsRestart(t *testing.T) {
	dir := t.TempDir()
	sess := makeSession("logical-stale", "upload-stale")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-stale",
				CurrentUploadID: "upload-stale",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	oss := &fakeOSS{
		listPartsFn: func(_ context.Context, _ *UploadSession, _ string) error {
			return ErrOSSNotFound
		},
	}
	u := newDecideResumeUploader(dir, gw, oss)

	state := &persistedUploadState{
		LogicalUploadID:   "logical-stale",
		UploadID:          "upload-stale",
		MultipartUploadID: "multipart-old",
		FileSize:          512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeRestart {
		t.Errorf("decision = %v, want resumeRestart", decision)
	}
}

// TestDecideResumeAction_Continue_MultipartActive_ReturnsContinue verifies that when the
// server says CONTINUE and ListParts succeeds (multipart still active), the decision is
// resumeContinue with a valid session.
func TestDecideResumeAction_Continue_MultipartActive_ReturnsContinue(t *testing.T) {
	dir := t.TempDir()
	sess := makeSession("logical-active", "upload-active")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-active",
				CurrentUploadID: "upload-active",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	oss := &fakeOSS{
		listPartsFn: func(_ context.Context, _ *UploadSession, _ string) error {
			return nil // multipart still active
		},
	}
	u := newDecideResumeUploader(dir, gw, oss)

	state := &persistedUploadState{
		LogicalUploadID:   "logical-active",
		UploadID:          "upload-active",
		MultipartUploadID: "multipart-ok",
		FileSize:          512,
	}
	decision, gotSession, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
	if gotSession == nil || gotSession.UploadID != "upload-active" {
		t.Errorf("expected valid session with upload-active, got %+v", gotSession)
	}
}

// TestDecideResumeAction_CompleteOnly_ETagMatch_ReturnsOSSAlreadyComplete verifies that when
// the server says COMPLETE_ONLY and HeadObject returns an ETag matching the server's record,
// the decision is resumeOSSAlreadyComplete with the correct ETag and part count.
func TestDecideResumeAction_CompleteOnly_ETagMatch_ReturnsOSSAlreadyComplete(t *testing.T) {
	dir := t.TempDir()
	const expectedETag = `"abc123-3"`
	const expectedPartCount = int32(3)
	sess := makeSession("logical-complete", "upload-complete")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID:    "logical-complete",
				CurrentUploadID:    "upload-complete",
				NextAction:         pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY,
				OSSObjectETag:      expectedETag,
				CompletedPartCount: expectedPartCount,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	oss := &fakeOSS{
		headObjectETagFn: func(_ context.Context, _ *UploadSession) (string, error) {
			return expectedETag, nil // ETag matches server record
		},
	}
	u := newDecideResumeUploader(dir, gw, oss)

	state := &persistedUploadState{
		LogicalUploadID: "logical-complete",
		UploadID:        "upload-complete",
		FileSize:        512,
	}
	decision, gotSession, ossETag, partCount, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeOSSAlreadyComplete {
		t.Errorf("decision = %v, want resumeOSSAlreadyComplete", decision)
	}
	if gotSession == nil || gotSession.UploadID != "upload-complete" {
		t.Errorf("expected valid session, got %+v", gotSession)
	}
	if ossETag != expectedETag {
		t.Errorf("ossETag = %q, want %q", ossETag, expectedETag)
	}
	if partCount != expectedPartCount {
		t.Errorf("partCount = %d, want %d", partCount, expectedPartCount)
	}
}

// TestDecideResumeAction_CompleteOnly_ObjectMissing_ReturnsRestart verifies that when the
// server says COMPLETE_ONLY but HeadObject returns ErrOSSNotFound (object not on OSS yet),
// the decision is resumeRestart.
func TestDecideResumeAction_CompleteOnly_ObjectMissing_ReturnsRestart(t *testing.T) {
	dir := t.TempDir()
	sess := makeSession("logical-missing", "upload-missing")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID:    "logical-missing",
				CurrentUploadID:    "upload-missing",
				NextAction:         pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY,
				OSSObjectETag:      `"abc123-3"`,
				CompletedPartCount: 3,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	oss := &fakeOSS{
		headObjectETagFn: func(_ context.Context, _ *UploadSession) (string, error) {
			return "", ErrOSSNotFound
		},
	}
	u := newDecideResumeUploader(dir, gw, oss)

	state := &persistedUploadState{
		LogicalUploadID: "logical-missing",
		UploadID:        "upload-missing",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeRestart {
		t.Errorf("decision = %v, want resumeRestart", decision)
	}
}

// TestDecideResumeAction_CompleteOnly_ETagMismatch_ReturnsRestart verifies that when the
// server says COMPLETE_ONLY but the object on OSS has a different ETag (partial / corrupt
// upload), the decision is resumeRestart.
func TestDecideResumeAction_CompleteOnly_ETagMismatch_ReturnsRestart(t *testing.T) {
	dir := t.TempDir()
	sess := makeSession("logical-mismatch-etag", "upload-mismatch-etag")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID:    "logical-mismatch-etag",
				CurrentUploadID:    "upload-mismatch-etag",
				NextAction:         pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_COMPLETE_ONLY,
				OSSObjectETag:      `"expected-etag"`,
				CompletedPartCount: 2,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return sess, nil
		},
	}
	oss := &fakeOSS{
		headObjectETagFn: func(_ context.Context, _ *UploadSession) (string, error) {
			return `"different-etag"`, nil // ETag does not match
		},
	}
	u := newDecideResumeUploader(dir, gw, oss)

	state := &persistedUploadState{
		LogicalUploadID: "logical-mismatch-etag",
		UploadID:        "upload-mismatch-etag",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision != resumeRestart {
		t.Errorf("decision = %v, want resumeRestart", decision)
	}
}

// TestDecideResumeAction_ReissueCredentialsFails_ReturnsContinueWithError verifies that a
// failure from ReissueUploadCredentials is treated as transient: the error is propagated
// and the decision is resumeContinue so the caller preserves local state for the next retry.
func TestDecideResumeAction_ReissueCredentialsFails_ReturnsContinueWithError(t *testing.T) {
	dir := t.TempDir()
	reissueErr := errors.New("rpc: deadline exceeded")
	gw := &fakeGateway{
		getUploadRecoveryFn: func(_ context.Context, _ string) (*UploadRecoveryInfo, error) {
			return &UploadRecoveryInfo{
				LogicalUploadID: "logical-reissue-fail",
				CurrentUploadID: "upload-reissue-fail",
				NextAction:      pb.UploadRecoveryAction_UPLOAD_RECOVERY_ACTION_CONTINUE,
			}, nil
		},
		reissueCredentialsFn: func(_ context.Context, _ string) (*UploadSession, error) {
			return nil, reissueErr
		},
	}
	u := newDecideResumeUploader(dir, gw, &fakeOSS{})

	state := &persistedUploadState{
		LogicalUploadID: "logical-reissue-fail",
		UploadID:        "upload-reissue-fail",
		FileSize:        512,
	}
	decision, _, _, _, err := u.decideResumeAction(context.Background(), state, 512)
	if err == nil {
		t.Fatal("expected error for ReissueUploadCredentials failure, got nil")
	}
	if !errors.Is(err, reissueErr) {
		t.Errorf("error = %v, want to wrap %v", err, reissueErr)
	}
	if decision != resumeContinue {
		t.Errorf("decision = %v, want resumeContinue", decision)
	}
}
