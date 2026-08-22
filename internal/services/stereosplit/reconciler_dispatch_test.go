// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"testing"
)

func TestShouldPreferQueuedDispatchUsesAvailableCapacity(t *testing.T) {
	db := newTestDB(t)
	insertTestEpisode(t, db, 901, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
	insertTestEpisode(t, db, 902, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/queued.mcap"}`, "")
	if _, err := db.Exec(`
		INSERT INTO stereo_split_image_configs (image_ref, max_concurrent, created_by)
		VALUES (?, 1, 'admin')
	`, testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	manager := NewManager(db, nil, nil, testManagerConfig())
	if _, _, err := manager.Start(context.Background(), 901, "admin"); err != nil {
		t.Fatalf("start queued derivative: %v", err)
	}
	prefer, err := manager.shouldPreferQueuedDispatch(context.Background())
	if err != nil {
		t.Fatalf("shouldPreferQueuedDispatch() error: %v", err)
	}
	if !prefer {
		t.Fatal("shouldPreferQueuedDispatch() = false, want true")
	}
	if _, err := db.Exec(`
		UPDATE episode_derivatives
		SET processing_status = ?, orbit_job_id = 'job-1'
		WHERE episode_id = ?
	`, ProcessingRunning, 901); err != nil {
		t.Fatalf("mark active derivative: %v", err)
	}
	if _, _, err := manager.Start(context.Background(), 902, "admin"); err != nil {
		t.Fatalf("start queued derivative: %v", err)
	}
	prefer, err = manager.shouldPreferQueuedDispatch(context.Background())
	if err != nil {
		t.Fatalf("shouldPreferQueuedDispatch() at capacity error: %v", err)
	}
	if prefer {
		t.Fatal("shouldPreferQueuedDispatch() = true at capacity, want false")
	}
}

func TestAtOrbitCapacityUsesCentralizedDispatchCapacity(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`
		INSERT INTO stereo_split_image_configs (image_ref, max_concurrent, created_by)
		VALUES (?, 1, 'admin')
	`, testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	manager := NewManager(db, nil, nil, testManagerConfig())
	atCapacity, err := manager.atOrbitCapacity(context.Background())
	if err != nil {
		t.Fatalf("atOrbitCapacity() error: %v", err)
	}
	if atCapacity {
		t.Fatal("atOrbitCapacity() = true with no active derivatives")
	}
}
