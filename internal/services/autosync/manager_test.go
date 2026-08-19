// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package autosync

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/services/depthnorm"
	"archebase.com/keystone-edge/internal/services/stereosplit"
)

func TestManagerUpdateConfigPersistsEnabledRevision(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()

	manager := NewManager(db, nil, nil, 0)
	current, err := manager.CurrentConfig(context.Background())
	if err != nil {
		t.Fatalf("CurrentConfig() error = %v", err)
	}
	if current.Enabled || current.ID != 1 {
		t.Fatalf("bootstrap config = %+v, want revision 1 disabled", current)
	}

	updated, err := manager.UpdateConfig(context.Background(), true, current.ID, "admin-1")
	if err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}
	if !updated.Enabled || updated.ID != 2 || updated.PreviousEnabled == nil || *updated.PreviousEnabled {
		t.Fatalf("updated config = %+v, want revision 2 enabled after disabled", updated)
	}
	if updated.CreatedBy != "admin-1" {
		t.Fatalf("CreatedBy = %q, want admin-1", updated.CreatedBy)
	}

	reloaded, err := manager.CurrentConfig(context.Background())
	if err != nil {
		t.Fatalf("CurrentConfig() after update error = %v", err)
	}
	if reloaded.ID != updated.ID || !reloaded.Enabled {
		t.Fatalf("reloaded config = %+v, want %+v", reloaded, updated)
	}
}

func TestManagerReconcileOnceStartsStereoSplitAfterEpisodeQA(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 30, DeviceTypeEgoPortalStereo)

	stereo := &fakeStereoSplitter{}
	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, stereo, cloud, 0)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 30); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 30`); err != nil {
		t.Fatalf("approve episode: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if stereo.episodeID != 30 || stereo.actor != "auto-sync" {
		t.Fatalf("stereo start = episode %d actor %q, want 30/auto-sync", stereo.episodeID, stereo.actor)
	}
	if len(cloud.originalEpisodeIDs()) != 0 || len(cloud.stereoEpisodeIDs()) != 0 {
		t.Fatal("cloud sync started before stereo split output was approved")
	}
}

func TestManagerReconcileOnceReenqueuesCapturedPendingQA(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 29, DeviceTypeEgoPortalLite)

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 29); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}

	qa := &fakeQAEnqueuer{}
	restarted := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	restarted.SetQAEnqueuer(qa)
	worked, err := restarted.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := qa.episodeIDs(); len(got) != 1 || got[0] != 29 {
		t.Fatalf("QA episodes = %#v, want [29]", got)
	}
}

func TestManagerReconcileOnceRecoversUploadMissedBeforeRestart(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	config, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1")
	if err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	seedAutoSyncEpisode(t, db, 26, DeviceTypeEgoPortalLite)
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_observed_at = ? WHERE id = 26`, config.CreatedAt.Add(time.Second)); err != nil {
		t.Fatalf("set upload time after enabled revision: %v", err)
	}

	qa := &fakeQAEnqueuer{}
	restarted := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	restarted.SetQAEnqueuer(qa)
	worked, err := restarted.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want recovered QA work")
	}
	if got := qa.episodeIDs(); len(got) != 1 || got[0] != 26 {
		t.Fatalf("QA episodes = %#v, want [26]", got)
	}
	var requested bool
	if err := db.Get(&requested, `SELECT auto_sync_requested FROM episodes WHERE id = 26`); err != nil {
		t.Fatalf("load recovered capture: %v", err)
	}
	if !requested {
		t.Fatal("auto_sync_requested = false, want true")
	}
}

func TestManagerReconcileOnceDoesNotRecoverUploadCreatedBeforeEnable(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 25, DeviceTypeEgoPortalLite)
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_observed_at = ? WHERE id = 25`, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("set historical upload time: %v", err)
	}

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	manager.SetQAEnqueuer(&fakeQAEnqueuer{})
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if worked {
		t.Fatal("ReconcileOnce() worked = true for upload created before enable")
	}
	var requested bool
	if err := db.Get(&requested, `SELECT auto_sync_requested FROM episodes WHERE id = 25`); err != nil {
		t.Fatalf("load historical capture state: %v", err)
	}
	if requested {
		t.Fatal("historical upload was captured after enabling automatic sync")
	}
}

func TestManagerReconcileOnceDoesNotRecoverNonExactDeviceType(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	manager.SetQAEnqueuer(&fakeQAEnqueuer{})
	config, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1")
	if err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	seedAutoSyncEpisode(t, db, 24, strings.ToLower(DeviceTypeEgoPortalLite))
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_observed_at = ? WHERE id = 24`, config.CreatedAt.Add(time.Second)); err != nil {
		t.Fatalf("set upload time after enabled revision: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if worked {
		t.Fatal("ReconcileOnce() recovered a non-exact device type")
	}
}

func TestManagerStartReconcilerProcessesCapturedEpisode(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 28, DeviceTypeEgoPortalLite)

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 10*time.Millisecond)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 28); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	qa := newNotifyingQAEnqueuer()
	manager.SetQAEnqueuer(qa)
	if err := manager.StartReconciler(); err != nil {
		t.Fatalf("StartReconciler() error = %v", err)
	}

	select {
	case episodeID := <-qa.enqueued:
		if episodeID != 28 {
			t.Fatalf("QA episode = %d, want 28", episodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("automatic reconciler did not enqueue QA")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.StopReconciler(ctx); err != nil {
		t.Fatalf("StopReconciler() error = %v", err)
	}
}

func TestManagerStartReconcilerDoesNotOverlapStoppingRunner(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 27, DeviceTypeEgoPortalLite)

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 10*time.Millisecond)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 27); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	qa := newBlockingQAEnqueuer()
	manager.SetQAEnqueuer(qa)
	if err := manager.StartReconciler(); err != nil {
		t.Fatalf("StartReconciler() error = %v", err)
	}
	select {
	case <-qa.entered:
	case <-time.After(time.Second):
		t.Fatal("automatic reconciler did not enter QA enqueue")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stopCancel()
	if err := manager.StopReconciler(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopReconciler() error = %v, want deadline exceeded", err)
	}
	if err := manager.StartReconciler(); err != nil {
		t.Fatalf("StartReconciler() while stopping error = %v", err)
	}

	overlapped := false
	select {
	case <-qa.entered:
		overlapped = true
	case <-time.After(50 * time.Millisecond):
	}
	close(qa.release)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	if err := manager.StopReconciler(cleanupCtx); err != nil {
		t.Fatalf("cleanup StopReconciler() error = %v", err)
	}
	if overlapped {
		t.Fatal("StartReconciler() started a second runner before the first stopped")
	}
}

func TestManagerReconcileOnceEnqueuesApprovedStereoDerivative(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 31, DeviceTypeEgoPortalStereo)

	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, &fakeStereoSplitter{}, cloud, 0)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 31); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`
		UPDATE episodes SET qa_status = 'approved' WHERE id = 31;
		INSERT INTO episode_derivatives (episode_id, kind, processing_status, qa_status)
		VALUES (31, 'stereo_split', 'succeeded', 'approved');
	`); err != nil {
		t.Fatalf("seed approved derivative: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := cloud.stereoEpisodeIDs(); len(got) != 1 || got[0] != 31 {
		t.Fatalf("stereo sync episodes = %#v, want [31]", got)
	}
	if got := cloud.originalEpisodeIDs(); len(got) != 0 {
		t.Fatalf("original sync episodes = %#v, want none", got)
	}
}

func TestManagerReconcileOnceEnqueuesApprovedLiteOriginal(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 20, DeviceTypeEgoPortalLite)

	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, nil, cloud, 0)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 20); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 20`); err != nil {
		t.Fatalf("approve episode: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := cloud.originalEpisodeIDs(); len(got) != 1 || got[0] != 20 {
		t.Fatalf("original sync episodes = %#v, want [20]", got)
	}
	if got := cloud.stereoEpisodeIDs(); len(got) != 0 {
		t.Fatalf("stereo sync episodes = %#v, want none", got)
	}
}

func TestManagerReconcileOnceStartsZJWA1DDepthNormalization(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 40, DeviceTypeZJWA1D)

	cloud := &fakeCloudSyncEnqueuer{}
	normalizer := &fakeDepthNormalizer{}
	manager := NewManager(db, &fakeStereoSplitter{}, cloud, 0, normalizer)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 40); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 40`); err != nil {
		t.Fatalf("approve episode: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if normalizer.episodeID != 40 || normalizer.actor != "auto-sync" {
		t.Fatalf("depth normalizer = episode %d actor %q, want 40/auto-sync", normalizer.episodeID, normalizer.actor)
	}
	if len(cloud.originalEpisodeIDs()) != 0 || len(cloud.stereoEpisodeIDs()) != 0 || len(cloud.depthEpisodeIDs()) != 0 {
		t.Fatal("cloud sync started before depth normalization completed")
	}
}

func TestManagerReconcileOnceEnqueuesApprovedZJWA1DDerivative(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 41, DeviceTypeZJWA1D)

	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, nil, cloud, 0, &fakeDepthNormalizer{})
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 41); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`
		UPDATE episodes SET qa_status = 'approved' WHERE id = 41;
		INSERT INTO episode_derivatives (episode_id, kind, processing_status, qa_status)
		VALUES (41, 'depth_normalization', 'succeeded', 'approved');
	`); err != nil {
		t.Fatalf("seed approved derivative: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := cloud.depthEpisodeIDs(); len(got) != 1 || got[0] != 41 {
		t.Fatalf("depth sync episodes = %#v, want [41]", got)
	}
}

func TestManagerReconcileOnceEnqueuesZJWA1DAlreadyTargetOriginal(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 42, DeviceTypeZJWA1D)

	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, nil, cloud, 0, &fakeDepthNormalizer{})
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 42); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	metadata := `{"depth_normalization":{"required":false,"reason":"already_target"}}`
	if _, err := db.Exec(`UPDATE episodes SET qa_status='approved', metadata=? WHERE id=?`, metadata, 42); err != nil {
		t.Fatalf("seed already-compressed episode: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := cloud.originalEpisodeIDs(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("original sync episodes = %#v, want [42]", got)
	}
}

func TestManagerReconcileOnceAdvancesApprovedBeforeRecoveringPendingQA(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 15, DeviceTypeEgoPortalLite)
	seedAutoSyncEpisode(t, db, 16, DeviceTypeEgoPortalLite)

	cloud := &fakeCloudSyncEnqueuer{}
	qa := &fakeQAEnqueuer{}
	manager := NewManager(db, nil, cloud, 0)
	manager.SetQAEnqueuer(qa)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 15); err != nil || !captured {
		t.Fatalf("CaptureEpisode(15) = %t, %v; want true, nil", captured, err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 16); err != nil || !captured {
		t.Fatalf("CaptureEpisode(16) = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 16`); err != nil {
		t.Fatalf("approve episode: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want true")
	}
	if got := cloud.originalEpisodeIDs(); len(got) != 1 || got[0] != 16 {
		t.Fatalf("original sync episodes = %#v, want [16]", got)
	}
	if got := qa.episodeIDs(); len(got) != 0 {
		t.Fatalf("QA recovery episodes = %#v, want none before approved work", got)
	}
}

func TestManagerCaptureEpisodeMarksSupportedUpload(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 10, DeviceTypeEgoPortalLite)
	seedAutoSyncEpisode(t, db, 11, "Other Device")
	seedAutoSyncEpisode(t, db, 9, " "+DeviceTypeEgoPortalLite+" ")

	manager := NewManager(db, nil, nil, 0)
	if _, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1"); err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}

	captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 10)
	if err != nil {
		t.Fatalf("CaptureEpisode() error = %v", err)
	}
	if !captured {
		t.Fatal("CaptureEpisode() captured = false, want true")
	}
	var marked struct {
		Requested   bool           `db:"auto_sync_requested"`
		DeviceType  sql.NullString `db:"auto_sync_device_type"`
		RequestedAt sql.NullTime   `db:"auto_sync_requested_at"`
	}
	if err := db.Get(&marked, `
		SELECT auto_sync_requested, auto_sync_device_type, auto_sync_requested_at
		FROM episodes WHERE id = 10
	`); err != nil {
		t.Fatalf("query captured episode: %v", err)
	}
	if !marked.Requested || !marked.DeviceType.Valid || marked.DeviceType.String != DeviceTypeEgoPortalLite || !marked.RequestedAt.Valid {
		t.Fatalf("captured episode = %+v", marked)
	}

	captured, err = captureEpisodeAtCurrentConfig(t, manager, db, 11)
	if err != nil {
		t.Fatalf("CaptureEpisode(unsupported) error = %v", err)
	}
	if captured {
		t.Fatal("unsupported device captured = true, want false")
	}

	captured, err = captureEpisodeAtCurrentConfig(t, manager, db, 9)
	if err != nil {
		t.Fatalf("CaptureEpisode(non-exact) error = %v", err)
	}
	if captured {
		t.Fatal("non-exact device type captured = true, want false")
	}
}

func TestManagerCaptureEpisodeDoesNotCaptureWhenDisabled(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 12, DeviceTypeEgoPortalLite)

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 12)
	if err != nil {
		t.Fatalf("CaptureEpisode() while disabled error = %v", err)
	}
	if captured {
		t.Fatal("CaptureEpisode() while disabled = true, want false")
	}
}

func TestManagerCaptureEpisodeUsesUploadTimeConfigRevision(t *testing.T) {
	tests := []struct {
		name               string
		uploadWhileEnabled bool
		wantCaptured       bool
	}{
		{name: "disabled upload followed by enable", uploadWhileEnabled: false, wantCaptured: false},
		{name: "enabled upload followed by disable", uploadWhileEnabled: true, wantCaptured: true},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newAutoSyncTestDB(t)
			defer db.Close()
			manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)

			config, err := manager.CurrentConfig(context.Background())
			if err != nil {
				t.Fatalf("load bootstrap config: %v", err)
			}
			if test.uploadWhileEnabled {
				config, err = manager.UpdateConfig(context.Background(), true, config.ID, "admin-1")
				if err != nil {
					t.Fatalf("enable auto sync: %v", err)
				}
			}

			episodeID := int64(60 + index)
			seedAutoSyncEpisode(t, db, episodeID, DeviceTypeEgoPortalLite)
			setAutoSyncObservedAt(t, db, episodeID, config.CreatedAt)

			if test.uploadWhileEnabled {
				if _, err := manager.UpdateConfig(context.Background(), false, config.ID, "admin-1"); err != nil {
					t.Fatalf("disable auto sync: %v", err)
				}
			} else if _, err := manager.UpdateConfig(context.Background(), true, config.ID, "admin-1"); err != nil {
				t.Fatalf("enable auto sync after upload: %v", err)
			}

			captured, err := manager.CaptureEpisode(context.Background(), episodeID)
			if err != nil {
				t.Fatalf("CaptureEpisode() error = %v", err)
			}
			if captured != test.wantCaptured {
				t.Fatalf("CaptureEpisode() captured = %t, want %t", captured, test.wantCaptured)
			}
		})
	}
}

func TestManagerReconcileOnceDoesNotAdvanceWhenDisabled(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 13, DeviceTypeEgoPortalLite)

	cloud := &fakeCloudSyncEnqueuer{}
	manager := NewManager(db, nil, cloud, 0)
	config, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1")
	if err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 13); err != nil || !captured {
		t.Fatalf("CaptureEpisode(13) = %t, %v; want true, nil", captured, err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 13`); err != nil {
		t.Fatalf("approve episode: %v", err)
	}
	if _, err := manager.UpdateConfig(context.Background(), false, config.ID, "admin-1"); err != nil {
		t.Fatalf("disable auto sync: %v", err)
	}

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() while disabled error = %v", err)
	}
	if worked {
		t.Fatal("ReconcileOnce() while disabled worked = true, want false")
	}
	if len(cloud.originalEpisodeIDs()) != 0 {
		t.Fatal("disabled automatic sync enqueued cloud work")
	}
}

func TestManagerReconcileOnceRecoversCapturedQAWhenDisabled(t *testing.T) {
	db := newAutoSyncTestDB(t)
	defer db.Close()
	seedAutoSyncEpisode(t, db, 14, DeviceTypeEgoPortalLite)

	manager := NewManager(db, nil, &fakeCloudSyncEnqueuer{}, 0)
	config, err := manager.UpdateConfig(context.Background(), true, 1, "admin-1")
	if err != nil {
		t.Fatalf("enable auto sync: %v", err)
	}
	if captured, err := captureEpisodeAtCurrentConfig(t, manager, db, 14); err != nil || !captured {
		t.Fatalf("CaptureEpisode() = %t, %v; want true, nil", captured, err)
	}
	if _, err := manager.UpdateConfig(context.Background(), false, config.ID, "admin-1"); err != nil {
		t.Fatalf("disable auto sync: %v", err)
	}
	qa := &fakeQAEnqueuer{}
	manager.SetQAEnqueuer(qa)

	worked, err := manager.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce() error = %v", err)
	}
	if !worked {
		t.Fatal("ReconcileOnce() worked = false, want QA recovery")
	}
	if got := qa.episodeIDs(); len(got) != 1 || got[0] != 14 {
		t.Fatalf("QA episodes = %#v, want [14]", got)
	}
}

func newAutoSyncTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE auto_sync_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			enabled BOOLEAN NOT NULL,
			previous_enabled BOOLEAN,
			created_by TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO auto_sync_configs (enabled, created_by)
		VALUES (FALSE, 'migration-bootstrap');

		CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_type TEXT,
			deleted_at TIMESTAMP
		);
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			deleted_at TIMESTAMP
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			workstation_id INTEGER,
			deleted_at TIMESTAMP
		);
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			task_id INTEGER,
			workstation_id INTEGER,
			qa_status TEXT NOT NULL DEFAULT 'pending_qa',
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			cloud_publish_source TEXT,
			metadata TEXT,
			auto_sync_requested BOOLEAN NOT NULL DEFAULT FALSE,
			auto_sync_device_type TEXT,
			auto_sync_requested_at TIMESTAMP,
			auto_sync_observed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at TIMESTAMP
		);
		CREATE TABLE episode_derivatives (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			processing_status TEXT NOT NULL,
			qa_status TEXT NOT NULL
		);
		CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create autosync schema: %v", err)
	}
	return db
}

func seedAutoSyncEpisode(t *testing.T, db *sqlx.DB, episodeID int64, deviceType string) {
	t.Helper()
	robotID := episodeID
	workstationID := episodeID
	taskID := episodeID
	if _, err := db.Exec(`INSERT INTO robots (id, device_type) VALUES (?, ?)`, robotID, deviceType); err != nil {
		t.Fatalf("seed robot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workstations (id, robot_id) VALUES (?, ?)`, workstationID, robotID); err != nil {
		t.Fatalf("seed workstation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, workstation_id) VALUES (?, ?)`, taskID, workstationID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO episodes (id, task_id, workstation_id) VALUES (?, ?, ?)`, episodeID, taskID, workstationID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
}

func setAutoSyncObservedAt(t *testing.T, db *sqlx.DB, episodeID int64, observedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE episodes SET auto_sync_observed_at = ? WHERE id = ?`, observedAt, episodeID); err != nil {
		t.Fatalf("set auto sync observed time: %v", err)
	}
}

func captureEpisodeAtCurrentConfig(t *testing.T, manager *Manager, db *sqlx.DB, episodeID int64) (bool, error) {
	t.Helper()
	config, err := manager.CurrentConfig(context.Background())
	if err != nil {
		t.Fatalf("load current auto sync config: %v", err)
	}
	setAutoSyncObservedAt(t, db, episodeID, config.CreatedAt)
	return manager.CaptureEpisode(context.Background(), episodeID)
}

type fakeCloudSyncEnqueuer struct {
	mu       sync.Mutex
	original []int64
	stereo   []int64
	depth    []int64
}

type fakeStereoSplitter struct {
	episodeID int64
	actor     string
}

type fakeDepthNormalizer struct {
	episodeID int64
	actor     string
}

func (f *fakeDepthNormalizer) Start(_ context.Context, episodeID int64, actor string) (depthnorm.Derivative, bool, error) {
	f.episodeID = episodeID
	f.actor = actor
	return depthnorm.Derivative{EpisodeID: episodeID}, true, nil
}

type fakeQAEnqueuer struct {
	mu       sync.Mutex
	episodes []int64
}

type notifyingQAEnqueuer struct {
	enqueued chan int64
}

type blockingQAEnqueuer struct {
	entered chan struct{}
	release chan struct{}
}

func newNotifyingQAEnqueuer() *notifyingQAEnqueuer {
	return &notifyingQAEnqueuer{enqueued: make(chan int64, 1)}
}

func newBlockingQAEnqueuer() *blockingQAEnqueuer {
	return &blockingQAEnqueuer{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
}

func (f *blockingQAEnqueuer) EnqueueEpisode(_ int64) {
	f.entered <- struct{}{}
	<-f.release
}

func (f *notifyingQAEnqueuer) EnqueueEpisode(episodeID int64) {
	select {
	case f.enqueued <- episodeID:
	default:
	}
}

func (f *fakeQAEnqueuer) EnqueueEpisode(episodeID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.episodes = append(f.episodes, episodeID)
}

func (f *fakeQAEnqueuer) episodeIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.episodes...)
}

func (f *fakeStereoSplitter) Start(_ context.Context, episodeID int64, actor string) (stereosplit.Derivative, bool, error) {
	f.episodeID = episodeID
	f.actor = actor
	return stereosplit.Derivative{EpisodeID: episodeID}, true, nil
}

func (f *fakeCloudSyncEnqueuer) EnqueueOriginalAutomatic(_ context.Context, episodeID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.original = append(f.original, episodeID)
	return nil
}

func (f *fakeCloudSyncEnqueuer) EnqueueDepthNormalizationAutomatic(_ context.Context, episodeID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depth = append(f.depth, episodeID)
	return nil
}

func (f *fakeCloudSyncEnqueuer) EnqueueStereoSplitManual(_ context.Context, episodeID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stereo = append(f.stereo, episodeID)
	return nil
}

func (f *fakeCloudSyncEnqueuer) depthEpisodeIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.depth...)
}

func (f *fakeCloudSyncEnqueuer) originalEpisodeIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.original...)
}

func (f *fakeCloudSyncEnqueuer) stereoEpisodeIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.stereo...)
}
