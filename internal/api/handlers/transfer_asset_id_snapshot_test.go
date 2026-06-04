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
