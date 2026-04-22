// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestPersistAndCleanupActiveState verifies round-trip write + remove.
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
	_, _, _, err := u.prepareUploadSession(
		nil, // context unused in this code path
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

	_, _, _, err := u.prepareUploadSession(
		nil,
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
