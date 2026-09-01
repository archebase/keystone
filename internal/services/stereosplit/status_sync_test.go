// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
)

func TestStatusSyncWorkersAdvanceCompletedOrbitJob(t *testing.T) {
	db := newTestDB(t)
	request := orbitapi.SubmitRequest{
		SubmissionID: "derivative-status-sync-stereo-split-g1",
		Image:        testImageDigest,
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processor_image, processing_status,
			orbit_submission_id, orbit_request, orbit_job_id, reconcile_after
		) VALUES (1, 'stereo_split', 1, ?, 'pending', ?, ?, ?, NULL)
	`, testImageDigest, request.SubmissionID, string(requestJSON), "status-job-1"); err != nil {
		t.Fatalf("insert pending derivative: %v", err)
	}
	manager := NewManager(db, &fakeOrbit{job: orbitapi.Job{
		JobID:        "status-job-1",
		SubmissionID: request.SubmissionID,
		Status:       "SUCCEEDED",
		Image:        testImageDigest,
	}}, nil, testManagerConfig())

	id, ok, err := manager.claimStatusSyncCandidate(context.Background())
	if err != nil || !ok {
		t.Fatalf("claimStatusSyncCandidate() id=%d ok=%t err=%v", id, ok, err)
	}
	if err := manager.reconcileOrbitStatus(context.Background(), id); err != nil {
		t.Fatalf("reconcileOrbitStatus() error = %v", err)
	}

	var status string
	if err := db.Get(&status, "SELECT processing_status FROM episode_derivatives WHERE id = ?", id); err != nil {
		t.Fatalf("load derivative status: %v", err)
	}
	if status != ProcessingVerifying {
		t.Fatalf("status=%q, want %q", status, ProcessingVerifying)
	}
}

func TestStatusSyncWorkersReclaimStaleLease(t *testing.T) {
	db := newTestDB(t)
	request := orbitapi.SubmitRequest{
		SubmissionID: "derivative-stale-status-sync-g1",
		Image:        testImageDigest,
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	staleAt := time.Now().Add(-stereoSplitStatusSyncLease - time.Second)
	if _, err := db.Exec(`
		INSERT INTO episode_derivatives (
			episode_id, kind, generation, processor_image, processing_status,
			orbit_submission_id, orbit_request, orbit_job_id, reconcile_after, updated_at
		) VALUES (2, 'stereo_split', 1, ?, 'pending', ?, ?, ?, ?, ?)
	`, testImageDigest, request.SubmissionID, string(requestJSON), "status-job-2",
		time.Now().UTC().Add(time.Hour), staleAt); err != nil {
		t.Fatalf("insert stale derivative: %v", err)
	}
	manager := NewManager(db, &fakeOrbit{}, nil, testManagerConfig())
	manager.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	id, ok, err := manager.claimStatusSyncCandidate(context.Background())
	if err != nil || !ok {
		t.Fatalf("claimStatusSyncCandidate() id=%d ok=%t err=%v", id, ok, err)
	}
	if id != 1 {
		t.Fatalf("claimed derivative id=%d, want 1", id)
	}
}
