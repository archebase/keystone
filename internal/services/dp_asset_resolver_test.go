// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func newTestAssetResolverDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
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
			_ = db.Close()
			t.Fatalf("create schema: %v", err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestResolveAssetIDForEpisode_MetadataWins(t *testing.T) {
	db := newTestAssetResolverDB(t)
	if _, err := db.Exec(`INSERT INTO robots (id, device_id, asset_id) VALUES (1, 'local-device', 'fallback-asset')`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id) VALUES (10, 1)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	got, err := resolveAssetIDForEpisode(
		context.Background(),
		db,
		1,
		sql.NullString{String: `{"asset_id":" snapshot-asset "}`, Valid: true},
		sql.NullInt64{Int64: 10, Valid: true},
	)
	if err != nil {
		t.Fatalf("resolveAssetIDForEpisode() error = %v", err)
	}
	if got != "snapshot-asset" {
		t.Fatalf("asset_id=%q want snapshot-asset", got)
	}
}

func TestResolveAssetIDForEpisode_FallbackReadsSoftDeletedWorkstation(t *testing.T) {
	db := newTestAssetResolverDB(t)
	if _, err := db.Exec(`INSERT INTO robots (id, device_id, asset_id) VALUES (1, 'local-device', 'fallback-asset')`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id, deleted_at) VALUES (10, 1, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	got, err := resolveAssetIDForEpisode(
		context.Background(),
		db,
		1,
		sql.NullString{},
		sql.NullInt64{Int64: 10, Valid: true},
	)
	if err != nil {
		t.Fatalf("resolveAssetIDForEpisode() error = %v", err)
	}
	if got != "fallback-asset" {
		t.Fatalf("asset_id=%q want fallback-asset", got)
	}
}

func TestResolveAssetIDForEpisode_MissingDoesNotFallbackToLocalDeviceID(t *testing.T) {
	db := newTestAssetResolverDB(t)
	if _, err := db.Exec(`INSERT INTO robots (id, device_id, asset_id) VALUES (1, 'local-device', NULL)`); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id) VALUES (10, 1)`); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}

	_, err := resolveAssetIDForEpisode(
		context.Background(),
		db,
		1,
		sql.NullString{},
		sql.NullInt64{Int64: 10, Valid: true},
	)
	if err == nil || !strings.Contains(err.Error(), "asset_id") {
		t.Fatalf("error=%v want asset_id missing error", err)
	}
}
