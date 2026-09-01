// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"testing"
	"time"
)

func TestClaimDispatchCandidateReservesDistinctQueuedRows(t *testing.T) {
	db := newTestDB(t)
	for id := int64(1); id <= 2; id++ {
		insertTestEpisode(t, db, id, "keystone_tos", `{"bucket":"source-bucket","object_key":"raw/source.mcap"}`, "")
		if _, err := db.Exec(`
			INSERT INTO episode_derivatives (
				episode_id, kind, generation, processing_status,
				orbit_delete_status, qa_status, created_at, updated_at
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?)
		`, id, Kind, ProcessingQueued, DeleteNotRequired, QANotStarted, time.Now(), time.Now()); err != nil {
			t.Fatalf("insert queued derivative %d: %v", id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO stereo_split_image_configs (image_ref, max_concurrent, created_by) VALUES (?, 2, 'admin')`, testImageDigest); err != nil {
		t.Fatalf("insert image config: %v", err)
	}
	manager := NewManager(db, &fakeOrbit{}, &fakeObjectStore{}, testManagerConfig())

	first, ok, err := manager.claimDispatchCandidate(context.Background())
	if err != nil || !ok {
		t.Fatalf("first claim id=%d ok=%t err=%v", first.ID, ok, err)
	}
	second, ok, err := manager.claimDispatchCandidate(context.Background())
	if err != nil || !ok {
		t.Fatalf("second claim id=%d ok=%t err=%v", second.ID, ok, err)
	}
	if first.ID == second.ID {
		t.Fatalf("dispatch claims reused derivative id=%d", first.ID)
	}
	if _, ok, err := manager.claimDispatchCandidate(context.Background()); err != nil || ok {
		t.Fatalf("capacity claim ok=%t err=%v, want no third claim", ok, err)
	}
}
