// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAssetIDSnapshotMetadata_WritesWhenRobotHasAssetID(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()

	createAssetIDSnapshotSchema(t, db)
	if _, err := db.Exec(`INSERT INTO robots (id, asset_id, deleted_at) VALUES (1, ' asset-1 ', NULL)`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id, deleted_at) VALUES (10, 1, NULL)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	got := assetIDSnapshotMetadata(context.Background(), tx, sql.NullInt64{Int64: 10, Valid: true})
	if !got.Valid {
		t.Fatal("metadata was not written")
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got.String), &decoded); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if decoded["asset_id"] != "asset-1" {
		t.Fatalf("asset_id=%q want asset-1", decoded["asset_id"])
	}
}

func TestAssetIDSnapshotMetadata_MissingDoesNotFailEpisodeCreationPath(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()

	createAssetIDSnapshotSchema(t, db)
	if _, err := db.Exec(`INSERT INTO robots (id, asset_id, deleted_at) VALUES (1, NULL, NULL)`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id, deleted_at) VALUES (10, 1, NULL)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	got := assetIDSnapshotMetadata(context.Background(), tx, sql.NullInt64{Int64: 10, Valid: true})
	if got.Valid {
		t.Fatalf("metadata valid=%t value=%q, want NULL", got.Valid, got.String)
	}
}

func TestSidecarWriterHealthMetadata_ReadsTopLevelObject(t *testing.T) {
	var sc sidecarJSON
	if err := json.Unmarshal([]byte(`{
		"writer_health": {
			"state": "critical",
			"writer_stall_state": "critical",
			"writer_stall_suspected": true,
			"writer_partial_failures": 2,
			"writer_queue_overflows": 1,
			"error": "writer_partial_failures=2"
		}
	}`), &sc); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}

	got, ok := sidecarWriterHealthMetadata(&sc)
	if !ok {
		t.Fatal("writer_health was not detected")
	}
	if got["state"] != "critical" {
		t.Fatalf("state=%v want critical", got["state"])
	}
	if got["writer_stall_suspected"] != true {
		t.Fatalf("writer_stall_suspected=%v want true", got["writer_stall_suspected"])
	}
}

func TestSidecarWriterHealthMetadata_MissingDoesNotWrite(t *testing.T) {
	got, ok := sidecarWriterHealthMetadata(&sidecarJSON{})
	if ok || got != nil {
		t.Fatalf("writer_health=%#v ok=%t, want nil false", got, ok)
	}
}

func TestMergeRecorderWriterHealthMetadata_PreservesExistingFields(t *testing.T) {
	existing := sql.NullString{
		String: `{"asset_id":"asset-1","recorder":{"profile":"high_rate"},"owner":"ops"}`,
		Valid:  true,
	}
	writerHealth := map[string]any{
		"state":                   "critical",
		"writer_stall_state":      "critical",
		"writer_stall_suspected":  true,
		"writer_partial_failures": float64(2),
		"writer_queue_overflows":  float64(1),
		"error":                   "writer_partial_failures=2",
	}

	got := mergeRecorderWriterHealthMetadata(existing, writerHealth)
	if !got.Valid {
		t.Fatal("metadata was not written")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got.String), &decoded); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if decoded["asset_id"] != "asset-1" || decoded["owner"] != "ops" {
		t.Fatalf("existing metadata not preserved: %#v", decoded)
	}
	recorder, ok := decoded["recorder"].(map[string]any)
	if !ok {
		t.Fatalf("recorder metadata type=%T", decoded["recorder"])
	}
	if recorder["profile"] != "high_rate" {
		t.Fatalf("recorder.profile=%v want high_rate", recorder["profile"])
	}
	health, ok := recorder["writer_health"].(map[string]any)
	if !ok {
		t.Fatalf("writer_health type=%T", recorder["writer_health"])
	}
	if health["state"] != "critical" {
		t.Fatalf("writer_health.state=%v want critical", health["state"])
	}
}

func TestMergeRecorderWriterHealthMetadata_OverwritesOnlyWriterHealth(t *testing.T) {
	existing := sql.NullString{
		String: `{"recorder":{"profile":"high_rate","writer_health":{"state":"warning"}}}`,
		Valid:  true,
	}
	writerHealth := map[string]any{
		"state":                   "critical",
		"writer_partial_failures": float64(2),
	}

	got := mergeRecorderWriterHealthMetadata(existing, writerHealth)
	if !got.Valid {
		t.Fatal("metadata was not written")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got.String), &decoded); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	recorder := decoded["recorder"].(map[string]any)
	if recorder["profile"] != "high_rate" {
		t.Fatalf("recorder.profile=%v want high_rate", recorder["profile"])
	}
	health := recorder["writer_health"].(map[string]any)
	if health["state"] != "critical" {
		t.Fatalf("writer_health.state=%v want critical", health["state"])
	}
}

func TestMergeRecorderWriterHealthMetadata_InvalidExistingPreserved(t *testing.T) {
	existing := sql.NullString{String: `{invalid`, Valid: true}
	got := mergeRecorderWriterHealthMetadata(existing, map[string]any{"state": "normal"})
	if !got.Valid || got.String != existing.String {
		t.Fatalf("metadata=%#v want original invalid metadata", got)
	}
}

func createAssetIDSnapshotSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			asset_id TEXT,
			deleted_at TIMESTAMP NULL
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			deleted_at TIMESTAMP NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
}
