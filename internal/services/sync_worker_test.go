// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestObjectKeyFromStoredPath(t *testing.T) {
	tests := []struct {
		input  string
		bucket string
		want   string
	}{
		{input: "edge-factory-default/factory-default/device/2024-01-01/task.mcap", bucket: "edge-factory-default", want: "factory-default/device/2024-01-01/task.mcap"},
		{input: "/edge-factory-default/factory-default/device/2024-01-01/task.mcap", bucket: "edge-factory-default", want: "factory-default/device/2024-01-01/task.mcap"},
		{input: "bucket/key", bucket: "bucket", want: "key"},
		{input: "device-uploads/3/capture/capture.mcap", bucket: "archebase-keystone-device-upload-2116584179", want: "device-uploads/3/capture/capture.mcap"},
		{input: "just-a-file.mcap", bucket: "bucket", want: "just-a-file.mcap"},
		{input: "  ", bucket: "bucket", want: ""},
		{input: "", bucket: "bucket", want: ""},
	}

	for _, tt := range tests {
		got := objectKeyFromStoredPath(tt.input, tt.bucket)
		if got != tt.want {
			t.Errorf("objectKeyFromStoredPath(%q, %q) = %q, want %q", tt.input, tt.bucket, got, tt.want)
		}
	}
}

func TestHilbertUploadContextRejectsMissingDCPlanID(t *testing.T) {
	_, err := hilbertUploadContext(syncEpisodeUploadRow{ID: 4181})
	if err == nil {
		t.Fatal("hilbertUploadContext() error = nil, want missing dc_plan_id")
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("error=%v want non-retryable", err)
	}
	if !strings.Contains(err.Error(), "episode 4181 missing dc_plan_id") {
		t.Fatalf("error=%q want missing dc_plan_id detail", err)
	}
}

func TestHilbertUploadContextRejectsLocalPlanOnly(t *testing.T) {
	_, err := hilbertUploadContext(syncEpisodeUploadRow{
		ID:            4181,
		LocalDCPlanID: sql.NullInt64{Int64: 27, Valid: true},
	})
	if err == nil {
		t.Fatal("hilbertUploadContext() error = nil, want local plan rejection")
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("error=%v want non-retryable", err)
	}
	if !strings.Contains(err.Error(), "episode 4181 has local_dc_plan_id 27 but Hilbert upload requires dc_plan_id") {
		t.Fatalf("error=%q want local plan detail", err)
	}
}

func TestHilbertUploadContextRejectsMissingOrDeletedProjection(t *testing.T) {
	_, err := hilbertUploadContext(syncEpisodeUploadRow{
		ID:       4181,
		DCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
	})
	if err == nil {
		t.Fatal("hilbertUploadContext() error = nil, want missing projection rejection")
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("error=%v want non-retryable", err)
	}
	if !strings.Contains(err.Error(), "dc_plan 1001 not found or deleted") {
		t.Fatalf("error=%q want missing projection detail", err)
	}
}

func TestHilbertUploadContextRejectsInvalidWorkspace(t *testing.T) {
	_, err := hilbertUploadContext(syncEpisodeUploadRow{
		ID:                4181,
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: -9, Valid: true},
	})
	if err == nil {
		t.Fatal("hilbertUploadContext() error = nil, want invalid workspace rejection")
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("error=%v want non-retryable", err)
	}
	if !strings.Contains(err.Error(), "dc_plan 1001 has invalid workspace_id -9") {
		t.Fatalf("error=%q want invalid workspace detail", err)
	}
}

func TestHilbertUploadContextReturnsValidatedIDs(t *testing.T) {
	got, err := hilbertUploadContext(syncEpisodeUploadRow{
		ID:                4181,
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
	})
	if err != nil {
		t.Fatalf("hilbertUploadContext() error = %v", err)
	}
	if got.DCPlanID != 1001 || got.WorkspaceID != 123 {
		t.Fatalf("context=%+v want dc_plan_id=1001 workspace_id=123", got)
	}
}

func TestHilbertUploadContextClientHintsContainPlanAndWorkspace(t *testing.T) {
	hints := (hilbertEpisodeUploadContext{DCPlanID: 1001, WorkspaceID: 123}).clientHints()
	if hints["dc_plan_id"] != "1001" {
		t.Fatalf("dc_plan_id hint=%q want 1001", hints["dc_plan_id"])
	}
	if hints["workspace_id"] != "123" {
		t.Fatalf("workspace_id hint=%q want 123", hints["workspace_id"])
	}
}

func TestUploadEpisodeDirectStopsAtHilbertGuard(t *testing.T) {
	w := &SyncWorker{}
	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 0, Valid: true},
	})
	if err == nil || !strings.Contains(err.Error(), "dc_plan 1001 has invalid workspace_id 0") {
		t.Fatalf("uploadEpisodeDirect() error=%v want Hilbert guard failure", err)
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("error=%v want non-retryable", err)
	}
}

func TestUploadEpisodeDirectUsesHilbertRawDataPath(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-s3-cn-beijing.ivolces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	credentials.Credentials.TemporaryToken = "temp-token"
	hilbert := &fakeHilbertRawDataClient{
		credentials: credentials,
	}
	uploader := &fakeTOSObjectUploader{}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: uploader,
	}
	createdAt := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)

	result, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum: sql.NullString{
			String: "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
			Valid:  true,
		},
		DurationSec: sql.NullFloat64{Float64: 2.5, Valid: true},
		CreatedAt:   createdAt,
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}

	if hilbert.register.WorkspaceID != 123 || hilbert.register.DCPlanID != 1001 {
		t.Fatalf("register request ids = %+v, want workspace=123 dc_plan=1001", hilbert.register)
	}
	if hilbert.register.BagName != "episode-uuid.mcap" || hilbert.register.BagSize != int64(len(source.data)) {
		t.Fatalf("register bag fields = %+v", hilbert.register)
	}
	if hilbert.register.BagDigest != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("register BagDigest = %q, want persisted SHA-256", hilbert.register.BagDigest)
	}
	if !hilbert.register.BagStartTime.Equal(createdAt) || !hilbert.register.BagEndTime.Equal(createdAt.Add(2500*time.Millisecond)) {
		t.Fatalf("register bag times = %s-%s", hilbert.register.BagStartTime, hilbert.register.BagEndTime)
	}
	if hilbert.credentialsWorkspaceID != 123 || hilbert.credentialsRawDataID != 9876 || !hilbert.finished {
		t.Fatalf("Hilbert calls not completed: credentials workspace=%d raw=%d finished=%t",
			hilbert.credentialsWorkspaceID, hilbert.credentialsRawDataID, hilbert.finished)
	}
	if uploader.target.Bucket != "hilbert-bucket" || uploader.target.Key != "raw-data/123/capture.mcap" {
		t.Fatalf("upload target = %+v", uploader.target)
	}
	if uploader.size != int64(len(source.data)) || string(uploader.body) != string(source.data) {
		t.Fatalf("uploaded size/body = %d/%q", uploader.size, string(uploader.body))
	}
	if uploader.payloadHash != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("uploaded payload hash = %q, want persisted SHA-256", uploader.payloadHash)
	}
	if source.statCount != 1 || source.openCount != 1 {
		t.Fatalf("source reader calls stat=%d open=%d, want stat=1 open=1", source.statCount, source.openCount)
	}
	if source.statBucket != "source-bucket" || source.statObject != "device/capture.mcap" {
		t.Fatalf("source stat location=%s/%s", source.statBucket, source.statObject)
	}
	if source.openBucket != source.statBucket || source.openObject != source.statObject {
		t.Fatalf("source open location=%s/%s, want stat location", source.openBucket, source.openObject)
	}
	if result.UploadID != "9876" || result.Bucket != "hilbert-bucket" || result.ObjectKey != "raw-data/123/capture.mcap" {
		t.Fatalf("result = %+v", result)
	}
}

func TestUploadEpisodeDirectRecoversHilbertRawDataByCanonicalBagName(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	createdAt := time.Date(2026, 7, 15, 1, 2, 3, 456789123, time.UTC)
	digest := strings.Repeat("a", 64)
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-s3-cn-beijing.ivolces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	hilbert := &fakeHilbertRawDataClient{
		registerErr: errors.New("register response was lost"),
		lookup: &auth.HilbertRawData{
			ID:           9876,
			WorkspaceID:  123,
			DCPlanID:     1001,
			BagName:      "episode-uuid.mcap",
			BagStartTime: createdAt.Truncate(time.Microsecond),
			BagEndTime:   createdAt.Add(time.Second).Truncate(time.Microsecond),
			BagSize:      int64(len(source.data)),
			BagDigest:    strings.ToUpper(digest),
		},
		credentials: credentials,
	}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: &fakeTOSObjectUploader{},
	}

	result, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: digest, Valid: true},
		CreatedAt:         createdAt,
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}
	if hilbert.registerCount != 1 || hilbert.lookupCount != 1 || hilbert.lookupWorkspaceID != 123 || hilbert.lookupBagName != "episode-uuid.mcap" {
		t.Fatalf("registration recovery calls = register:%d lookup:%d workspace:%d bag:%q",
			hilbert.registerCount, hilbert.lookupCount, hilbert.lookupWorkspaceID, hilbert.lookupBagName)
	}
	if hilbert.credentialsRawDataID != 9876 || result.UploadID != "9876" {
		t.Fatalf("recovered raw data IDs = credentials:%d result:%q", hilbert.credentialsRawDataID, result.UploadID)
	}
	var rawDataID sql.NullInt64
	if err := db.Get(&rawDataID, "SELECT hilbert_raw_data_id FROM episodes WHERE id = 4181"); err != nil {
		t.Fatalf("load persisted Hilbert raw data ID: %v", err)
	}
	if !rawDataID.Valid || rawDataID.Int64 != 9876 {
		t.Fatalf("persisted Hilbert raw data ID = %#v, want 9876", rawDataID)
	}
}

func TestUploadEpisodeDirectRejectsCanonicalBagNameRegistrationConflict(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	createdAt := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	hilbert := &fakeHilbertRawDataClient{
		registerErr: errors.New("raw data already exists"),
		lookup: &auth.HilbertRawData{
			ID:           9876,
			WorkspaceID:  123,
			DCPlanID:     1001,
			BagName:      "episode-uuid.mcap",
			BagStartTime: createdAt,
			BagEndTime:   createdAt.Add(time.Second),
			BagSize:      int64(len(source.data)),
			BagDigest:    strings.Repeat("b", 64),
		},
	}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: &fakeTOSObjectUploader{},
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         createdAt,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts on bag_digest") {
		t.Fatalf("uploadEpisodeDirect() error = %v, want bag digest conflict", err)
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("uploadEpisodeDirect() error = %v, want non-retryable", err)
	}
	if hilbert.credentialsRawDataID != 0 || hilbert.finished {
		t.Fatalf("Hilbert continued after conflict: credentials rawDataID=%d finished=%t", hilbert.credentialsRawDataID, hilbert.finished)
	}
}

func TestUploadEpisodeDirectComputesAndPersistsMissingSHA256(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)

	source := &fakeSourceObjectReader{data: []byte("ego portal mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-s3-cn-beijing.ivolces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	credentials.Credentials.TemporaryToken = "temp-token"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	uploader := &fakeTOSObjectUploader{}
	w := &SyncWorker{
		db:                    db,
		minioBucket:           "source-bucket",
		hilbert:               hilbert,
		source:                source,
		tosUploader:           uploader,
		sourceObjectRangeSize: 4,
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}

	wantDigest := fmt.Sprintf("%x", sha256.Sum256(source.data))
	if hilbert.register.BagDigest != wantDigest {
		t.Fatalf("register BagDigest = %q, want computed SHA-256 %q", hilbert.register.BagDigest, wantDigest)
	}
	if uploader.payloadHash != wantDigest {
		t.Fatalf("uploaded payload hash = %q, want computed SHA-256 %q", uploader.payloadHash, wantDigest)
	}
	wantRangesPerPass := (len(source.data) + int(w.sourceObjectRangeSize) - 1) / int(w.sourceObjectRangeSize)
	if source.statCount != 1 || source.openCount != 2*wantRangesPerPass {
		t.Fatalf("source reader calls stat=%d open=%d, want stat=1 open=%d", source.statCount, source.openCount, 2*wantRangesPerPass)
	}
	for pass := 0; pass < 2; pass++ {
		for part := 0; part < wantRangesPerPass; part++ {
			call := source.ranges[pass*wantRangesPerPass+part]
			wantOffset := int64(part) * w.sourceObjectRangeSize
			wantLength := min(w.sourceObjectRangeSize, int64(len(source.data))-wantOffset)
			if call.offset != wantOffset || call.length != wantLength ||
				call.totalSize != int64(len(source.data)) || call.etag != source.objectETag() {
				t.Fatalf("pass %d range %d = offset:%d length:%d total:%d etag:%q, want offset:%d length:%d total:%d etag:%q",
					pass, part, call.offset, call.length, call.totalSize, call.etag,
					wantOffset, wantLength, len(source.data), source.objectETag())
			}
		}
	}

	var storedDigest string
	if err := db.Get(&storedDigest, "SELECT checksum FROM episodes WHERE id = ?", 4181); err != nil {
		t.Fatalf("query episode checksum: %v", err)
	}
	if storedDigest != wantDigest {
		t.Fatalf("stored episode checksum = %q, want %q", storedDigest, wantDigest)
	}
}

func TestUploadEpisodeDirectFailsWhenSourceChangesBetweenRanges(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	source := &changingSourceObjectReader{
		data:       []byte("abcdefgh"),
		etag:       "original-etag",
		changeAt:   1,
		changedErr: errors.New("source object no longer matches ETag"),
	}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-s3-cn-beijing.ivolces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	w := &SyncWorker{
		db:                    db,
		minioBucket:           "source-bucket",
		hilbert:               hilbert,
		source:                source,
		tosUploader:           &fakeTOSObjectUploader{},
		sourceObjectRangeSize: 4,
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if !errors.Is(err, source.changedErr) {
		t.Fatalf("uploadEpisodeDirect() error = %v, want source ETag change error", err)
	}
	if source.openCount != 2 {
		t.Fatalf("source range calls = %d, want first range followed by rejected second range", source.openCount)
	}
	if hilbert.finished {
		t.Fatal("FinishRawDataUpload called after source object changed")
	}
}

func TestUploadEpisodeDirectTreatsPayloadChecksumMismatchAsNonRetryable(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-cn-beijing.volces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: &fakeTOSObjectUploader{err: fmt.Errorf("%w: changed source", cloud.ErrTOSPayloadChecksumMismatch)},
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if !isNonRetryableSyncError(err) || !errors.Is(err, cloud.ErrTOSPayloadChecksumMismatch) {
		t.Fatalf("uploadEpisodeDirect() error = %v, want non-retryable checksum mismatch", err)
	}
	if hilbert.finished {
		t.Fatal("FinishRawDataUpload called after checksum mismatch")
	}
}

func TestUploadEpisodeDirectReadsDGWCompatEpisodeFromTOS(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	minioSource := &fakeSourceObjectReader{data: []byte("wrong minio bytes")}
	tosSource := &fakeSourceObjectReader{data: []byte("tos mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-s3-cn-beijing.ivolces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	credentials.Credentials.TemporaryToken = "temp-token"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	uploader := &fakeTOSObjectUploader{}
	w := &SyncWorker{
		db:          db,
		minioBucket: "edge-factory-archebase",
		hilbert:     hilbert,
		source:      minioSource,
		tosUploader: uploader,
	}
	w.SetTOSSourceObjectReader("archebase-keystone-device-upload-2116584179", tosSource)

	_, err := w.uploadEpisodeDirect(context.Background(), 0, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "device-uploads/5/capture/capture.mcap",
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"source":"dgwcompat","bucket":"archebase-keystone-device-upload-2116584179","object_key":"device-uploads/5/capture/capture.mcap"}`,
		},
		CreatedAt: time.Date(2026, 7, 23, 5, 38, 41, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}

	if minioSource.statCount != 0 || minioSource.openCount != 0 {
		t.Fatalf("MinIO source calls stat=%d open=%d, want 0", minioSource.statCount, minioSource.openCount)
	}
	if tosSource.statCount != 1 || tosSource.openCount != 2 {
		t.Fatalf("TOS source calls stat=%d open=%d, want stat=1 open=2", tosSource.statCount, tosSource.openCount)
	}
	if tosSource.statBucket != "archebase-keystone-device-upload-2116584179" || tosSource.statObject != "device-uploads/5/capture/capture.mcap" {
		t.Fatalf("TOS stat location=%s/%s", tosSource.statBucket, tosSource.statObject)
	}
	if tosSource.openBucket != tosSource.statBucket || tosSource.openObject != tosSource.statObject {
		t.Fatalf("TOS open location=%s/%s, want stat location", tosSource.openBucket, tosSource.openObject)
	}
	if string(uploader.body) != string(tosSource.data) {
		t.Fatalf("uploaded body=%q, want TOS body %q", string(uploader.body), string(tosSource.data))
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(tosSource.data))
	if hilbert.register.BagDigest != wantDigest || uploader.payloadHash != wantDigest {
		t.Fatalf("computed TOS digest register=%q upload=%q, want %q", hilbert.register.BagDigest, uploader.payloadHash, wantDigest)
	}
}

func TestEpisodeSHA256HexRejectsMissingOrInvalidChecksum(t *testing.T) {
	tests := []struct {
		name     string
		checksum sql.NullString
	}{
		{name: "missing"},
		{name: "wrong length", checksum: sql.NullString{String: "abc", Valid: true}},
		{name: "non hex", checksum: sql.NullString{String: strings.Repeat("z", 64), Valid: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := episodeSHA256Hex(syncEpisodeUploadRow{ID: 4181, Checksum: tt.checksum})
			if err == nil || !strings.Contains(err.Error(), "missing valid SHA-256 checksum") {
				t.Fatalf("episodeSHA256Hex() error = %v", err)
			}
			if !isNonRetryableSyncError(err) {
				t.Fatalf("episodeSHA256Hex() error = %v, want non-retryable", err)
			}
		})
	}
}

func TestDirectCloudUploadRequestContainsPlanMetadata(t *testing.T) {
	rawTags := map[string]string{"existing": "tag"}
	req := directCloudUploadRequest(
		syncEpisodeUploadRow{EpisodeUUID: "episode-1"},
		"path/file.mcap",
		"asset-1",
		rawTags,
		hilbertEpisodeUploadContext{DCPlanID: 1001, WorkspaceID: 123},
		nil,
	)
	if req.ClientHints["dc_plan_id"] != "1001" || req.ClientHints["workspace_id"] != "123" {
		t.Fatalf("client hints=%+v want plan/workspace ids", req.ClientHints)
	}
	if req.RawTags["existing"] != "tag" {
		t.Fatalf("raw tags=%+v want original tags", req.RawTags)
	}
}

type fakeHilbertRawDataClient struct {
	register               auth.HilbertRawDataRegisterRequest
	registerCount          int
	registerID             int64
	registerErr            error
	lookup                 *auth.HilbertRawData
	lookupCount            int
	lookupWorkspaceID      int64
	lookupBagName          string
	lookupErr              error
	credentials            *auth.HilbertRawDataUploadCredentials
	credentialsWorkspaceID int64
	credentialsRawDataID   int64
	credentialsErr         error
	finishWorkspaceID      int64
	finishRawDataID        int64
	finished               bool
}

func (c *fakeHilbertRawDataClient) RegisterRawData(_ context.Context, request auth.HilbertRawDataRegisterRequest) (int64, error) {
	c.registerCount++
	c.register = request
	if c.registerErr != nil {
		return 0, c.registerErr
	}
	if c.registerID > 0 {
		return c.registerID, nil
	}
	return 9876, nil
}

func (c *fakeHilbertRawDataClient) FindRawDataByBagName(_ context.Context, workspaceID int64, bagName string) (*auth.HilbertRawData, error) {
	c.lookupCount++
	c.lookupWorkspaceID = workspaceID
	c.lookupBagName = bagName
	if c.lookupErr != nil {
		return nil, c.lookupErr
	}
	return c.lookup, nil
}

func (c *fakeHilbertRawDataClient) GetRawDataUploadCredentials(_ context.Context, workspaceID, rawDataID int64) (*auth.HilbertRawDataUploadCredentials, error) {
	c.credentialsWorkspaceID = workspaceID
	c.credentialsRawDataID = rawDataID
	if c.credentialsErr != nil {
		return nil, c.credentialsErr
	}
	return c.credentials, nil
}

func (c *fakeHilbertRawDataClient) FinishRawDataUpload(_ context.Context, workspaceID, rawDataID int64) error {
	c.finishWorkspaceID = workspaceID
	c.finishRawDataID = rawDataID
	c.finished = true
	return nil
}

type fakeSourceObjectReader struct {
	data       []byte
	etag       string
	statCount  int
	openCount  int
	statBucket string
	statObject string
	openBucket string
	openObject string
	ranges     []sourceObjectRangeCall
}

type sourceObjectRangeCall struct {
	ctx       context.Context
	offset    int64
	length    int64
	totalSize int64
	etag      string
}

func (r *fakeSourceObjectReader) StatObject(_ context.Context, bucket, objectName string) (int64, string, error) {
	r.statCount++
	r.statBucket = bucket
	r.statObject = objectName
	return int64(len(r.data)), r.objectETag(), nil
}

func (r *fakeSourceObjectReader) OpenObjectRange(ctx context.Context, bucket, objectName string, offset, length, totalSize int64, etag string) (io.ReadCloser, error) {
	r.openCount++
	r.openBucket = bucket
	r.openObject = objectName
	r.ranges = append(r.ranges, sourceObjectRangeCall{ctx: ctx, offset: offset, length: length, totalSize: totalSize, etag: etag})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if totalSize != int64(len(r.data)) || etag != r.objectETag() {
		return nil, fmt.Errorf("fake source snapshot mismatch size=%d etag=%q", totalSize, etag)
	}
	if offset < 0 || length <= 0 || offset > int64(len(r.data)) || length > int64(len(r.data))-offset {
		return nil, fmt.Errorf("invalid fake source range offset=%d length=%d size=%d", offset, length, len(r.data))
	}
	return io.NopCloser(bytes.NewReader(r.data[offset : offset+length])), nil
}

func (r *fakeSourceObjectReader) objectETag() string {
	if r.etag != "" {
		return r.etag
	}
	return "fake-source-etag"
}

type changingSourceObjectReader struct {
	data       []byte
	etag       string
	openCount  int
	changeAt   int
	changedErr error
}

func (r *changingSourceObjectReader) StatObject(context.Context, string, string) (int64, string, error) {
	return int64(len(r.data)), r.etag, nil
}

func (r *changingSourceObjectReader) OpenObjectRange(ctx context.Context, _, _ string, offset, length, totalSize int64, etag string) (io.ReadCloser, error) {
	call := r.openCount
	r.openCount++
	if call >= r.changeAt {
		return nil, r.changedErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if totalSize != int64(len(r.data)) || etag != r.etag {
		return nil, fmt.Errorf("unexpected source snapshot size=%d etag=%q", totalSize, etag)
	}
	return io.NopCloser(bytes.NewReader(r.data[offset : offset+length])), nil
}

type fakeTOSObjectUploader struct {
	target      cloud.TOSS3UploadTarget
	size        int64
	payloadHash string
	body        []byte
	err         error
}

func (u *fakeTOSObjectUploader) PutObject(_ context.Context, target cloud.TOSS3UploadTarget, reader io.Reader, size int64, payloadHash string, progress cloud.UploadProgressFunc) (string, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	u.target = target
	u.size = size
	u.payloadHash = payloadHash
	u.body = data
	if u.err != nil {
		return "", u.err
	}
	if progress != nil {
		progress(size, size)
	}
	return "etag-1", nil
}

func TestSyncEpisodeUploadRowBagTimes(t *testing.T) {
	createdAt := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	row := syncEpisodeUploadRow{
		CreatedAt:   createdAt,
		DurationSec: sql.NullFloat64{Float64: 1.5, Valid: true},
	}
	if got := row.bagStartTime(); !got.Equal(createdAt) {
		t.Fatalf("bagStartTime() = %s, want %s", got, createdAt)
	}
	wantEnd := createdAt.Add(1500 * time.Millisecond)
	if got := row.bagEndTime(); !got.Equal(wantEnd) {
		t.Fatalf("bagEndTime() = %s, want %s", got, wantEnd)
	}

	row.DurationSec = sql.NullFloat64{}
	if got := row.bagEndTime(); !got.Equal(createdAt.Add(time.Second)) {
		t.Fatalf("bagEndTime() without duration = %s, want created+1s", got)
	}
}

func TestHilbertBagNameIsStableAndUnique(t *testing.T) {
	row := syncEpisodeUploadRow{ID: 4181, EpisodeUUID: "episode-uuid"}
	got := hilbertBagName(row, "device-uploads/3/capture_20260715T044250Z_b2d9911e/5b9e8785-d568-4b1e-82fe-26cbc1320e44/capture.mcap")
	want := "episode-uuid.mcap"
	if got != want {
		t.Fatalf("hilbertBagName() = %q, want %q", got, want)
	}
}

func TestHilbertBagNameFallsBackToEpisodeID(t *testing.T) {
	row := syncEpisodeUploadRow{ID: 4181}
	got := hilbertBagName(row, "device-uploads/3/capture.mcap")
	want := "episode_4181.mcap"
	if got != want {
		t.Fatalf("hilbertBagName() = %q, want %q", got, want)
	}
}

func TestUploadEpisodeDirectReusesPersistedHilbertRawDataID(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	if _, err := db.Exec(`UPDATE episodes SET hilbert_raw_data_id = 9876 WHERE id = 4181`); err != nil {
		t.Fatalf("set episode Hilbert raw data ID: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, started_at)
		VALUES (123, 4181, 'in_progress', 2, ?)
	`, time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-cn-beijing.volces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	uploader := &fakeTOSObjectUploader{}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: uploader,
	}

	result, err := w.uploadEpisodeDirect(context.Background(), 123, syncEpisodeUploadRow{
		ID:                4181,
		HilbertRawDataID:  sql.NullInt64{Int64: 9876, Valid: true},
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}
	if hilbert.registerCount != 0 {
		t.Fatalf("RegisterRawData calls = %d, want 0", hilbert.registerCount)
	}
	if hilbert.credentialsRawDataID != 9876 {
		t.Fatalf("credentials rawDataID = %d, want 9876", hilbert.credentialsRawDataID)
	}
	if result.UploadID != "9876" {
		t.Fatalf("UploadID = %q, want 9876", result.UploadID)
	}
}

func TestUploadEpisodeDirectPersistsHilbertRawDataIDBeforeCredentials(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, started_at)
		VALUES (123, 4181, 'in_progress', 1, ?)
	`, time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert: &fakeHilbertRawDataClient{
			credentialsErr: errors.New("credentials unavailable"),
		},
		source:      &fakeSourceObjectReader{data: []byte("mcap bytes")},
		tosUploader: &fakeTOSObjectUploader{},
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 123, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "credentials unavailable") {
		t.Fatalf("uploadEpisodeDirect() error = %v, want credentials failure", err)
	}

	var rawDataID sql.NullInt64
	if err := db.Get(&rawDataID, `SELECT hilbert_raw_data_id FROM episodes WHERE id = 4181`); err != nil {
		t.Fatalf("query episode Hilbert raw data ID: %v", err)
	}
	if !rawDataID.Valid || rawDataID.Int64 != 9876 {
		t.Fatalf("hilbert_raw_data_id=%#v want 9876", rawDataID)
	}
	var destinationPath string
	if err := db.Get(&destinationPath, `SELECT destination_path FROM sync_logs WHERE id = 123`); err != nil {
		t.Fatalf("query sync log destination: %v", err)
	}
	if destinationPath != hilbertRawDataIDDestinationPrefix+"9876" {
		t.Fatalf("sync log destination=%q want durable recovery prefix", destinationPath)
	}
}

func TestUploadEpisodeDirectRejectsConflictingHilbertRawDataID(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	if _, err := db.Exec(`UPDATE episodes SET hilbert_raw_data_id = 12345 WHERE id = 4181`); err != nil {
		t.Fatalf("set existing Hilbert raw data ID: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, started_at)
		VALUES (123, 4181, 'in_progress', 1, ?)
	`, time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	hilbert := &fakeHilbertRawDataClient{}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      &fakeSourceObjectReader{data: []byte("mcap bytes")},
		tosUploader: &fakeTOSObjectUploader{},
	}

	_, err := w.uploadEpisodeDirect(context.Background(), 123, syncEpisodeUploadRow{
		ID:                4181,
		EpisodeUUID:       "episode-uuid",
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "existing=12345 incoming=9876") {
		t.Fatalf("uploadEpisodeDirect() error = %v, want ID conflict", err)
	}
	if !isNonRetryableSyncError(err) {
		t.Fatalf("uploadEpisodeDirect() error = %v, want non-retryable", err)
	}
	if hilbert.credentialsRawDataID != 0 {
		t.Fatalf("credentials raw data ID = %d, want no credentials request", hilbert.credentialsRawDataID)
	}

	var rawDataID int64
	if err := db.Get(&rawDataID, `SELECT hilbert_raw_data_id FROM episodes WHERE id = 4181`); err != nil {
		t.Fatalf("query episode Hilbert raw data ID: %v", err)
	}
	if rawDataID != 12345 {
		t.Fatalf("hilbert_raw_data_id=%d want unchanged 12345", rawDataID)
	}
}

func TestUploadEpisodeDirectRecoversHilbertRawDataIDFromHistoricalSyncLog(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 4181, "approved", false)
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status, attempt_count, destination_path, started_at)
		VALUES
			(121, 4181, 'failed', 3, ?, ?),
			(122, 4181, 'failed', 1, NULL, ?),
			(123, 4181, 'in_progress', 1, NULL, ?)
	`,
		hilbertRawDataIDDestinationPrefix+"9876",
		time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 1, 1, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 1, 2, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert sync logs: %v", err)
	}

	source := &fakeSourceObjectReader{data: []byte("mcap bytes")}
	credentials := &auth.HilbertRawDataUploadCredentials{
		Provider: "TOS",
		Endpoint: "tos-cn-beijing.volces.com",
		Region:   "cn-beijing",
		Bucket:   "hilbert-bucket",
		Key:      "raw-data/123/capture.mcap",
	}
	credentials.Credentials.AccessKeyID = "temp-ak"
	credentials.Credentials.SecretAccessKey = "temp-sk"
	hilbert := &fakeHilbertRawDataClient{credentials: credentials}
	w := &SyncWorker{
		db:          db,
		minioBucket: "source-bucket",
		hilbert:     hilbert,
		source:      source,
		tosUploader: &fakeTOSObjectUploader{},
	}

	result, err := w.uploadEpisodeDirect(context.Background(), 123, syncEpisodeUploadRow{
		ID:                4181,
		DCPlanID:          sql.NullInt64{Int64: 1001, Valid: true},
		ProjectedDCPlanID: sql.NullInt64{Int64: 1001, Valid: true},
		WorkspaceID:       sql.NullInt64{Int64: 123, Valid: true},
		McapPath:          "source-bucket/device/capture.mcap",
		Checksum:          sql.NullString{String: strings.Repeat("a", 64), Valid: true},
		CreatedAt:         time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("uploadEpisodeDirect() error = %v", err)
	}
	if hilbert.registerCount != 0 {
		t.Fatalf("RegisterRawData calls = %d, want 0", hilbert.registerCount)
	}
	if hilbert.credentialsRawDataID != 9876 || result.UploadID != "9876" {
		t.Fatalf("recovered raw data IDs = credentials:%d upload:%q, want 9876", hilbert.credentialsRawDataID, result.UploadID)
	}

	var destinationPath string
	if err := db.Get(&destinationPath, "SELECT destination_path FROM sync_logs WHERE id = 123"); err != nil {
		t.Fatalf("query current sync log destination: %v", err)
	}
	if destinationPath != hilbertRawDataIDDestinationPrefix+"9876" {
		t.Fatalf("current sync log destination = %q, want recovered Hilbert raw-data ID", destinationPath)
	}
	var rawDataID sql.NullInt64
	if err := db.Get(&rawDataID, `SELECT hilbert_raw_data_id FROM episodes WHERE id = 4181`); err != nil {
		t.Fatalf("query episode Hilbert raw data ID: %v", err)
	}
	if !rawDataID.Valid || rawDataID.Int64 != 9876 {
		t.Fatalf("hilbert_raw_data_id=%#v want recovered ID 9876", rawDataID)
	}
}

func TestEnqueueEpisode_DeduplicatesPendingEpisode(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 42); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if err := w.EnqueueEpisode(ctx, 42); !errors.Is(err, ErrEpisodeAlreadyEnqueued) {
		t.Fatalf("second enqueue error = %v, want ErrEpisodeAlreadyEnqueued", err)
	}

	select {
	case got := <-w.enqueueCh:
		if got.episodeID != 42 {
			t.Fatalf("unexpected episode id: got %d want 42", got.episodeID)
		}
		if got.manual {
			t.Fatal("unexpected manual mode for EnqueueEpisode")
		}
	default:
		t.Fatal("expected episode to be enqueued")
	}

	select {
	case got := <-w.enqueueCh:
		t.Fatalf("duplicate enqueue detected: got %d", got.episodeID)
	default:
	}
}

func TestEnqueueEpisode_AllowsReenqueueAfterProcessing(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("initial enqueue failed: %v", err)
	}
	w.unmarkEnqueued(7)
	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("reenqueue failed: %v", err)
	}

	count := 0
	for {
		select {
		case <-w.enqueueCh:
			count++
		default:
			if count != 2 {
				t.Fatalf("expected 2 enqueue records after reenqueue, got %d", count)
			}
			return
		}
	}
}

func TestSyncWorkerEpisodeProgressSetGetAndFinish(t *testing.T) {
	w := NewSyncWorker(nil, nil, nil, "", SyncWorkerConfig{}, nil)

	if _, ok := w.GetEpisodeProgress(42); ok {
		t.Fatal("GetEpisodeProgress before set ok = true, want false")
	}

	w.setEpisodeProgress(42, 12, 100)
	progress, ok := w.GetEpisodeProgress(42)
	if !ok {
		t.Fatal("GetEpisodeProgress after set ok = false, want true")
	}
	if progress.UploadedBytes != 12 || progress.TotalBytes != 100 {
		t.Fatalf("progress = %+v, want uploaded=12 total=100", progress)
	}
	if progress.UpdatedAt.IsZero() {
		t.Fatal("progress UpdatedAt is zero")
	}

	w.setEpisodeProgress(42, 150, 200)
	progress, ok = w.GetEpisodeProgress(42)
	if !ok {
		t.Fatal("GetEpisodeProgress after overwrite ok = false, want true")
	}
	if progress.UploadedBytes != 150 || progress.TotalBytes != 200 {
		t.Fatalf("overwritten progress = %+v, want uploaded=150 total=200", progress)
	}

	w.finishEpisodeProgress(42)
	if _, ok := w.GetEpisodeProgress(42); ok {
		t.Fatal("GetEpisodeProgress after finish ok = true, want false")
	}
}

func TestFindPendingEpisodes_ExcludesExhaustedFailuresFromPollingOnly(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db, cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3}}

	insertEpisodeForSyncWorkerTest(t, db, 1, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 2, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 3, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 4, "approved", false)

	insertSyncLogForSyncWorkerTest(t, db, 2, "failed", 3)
	insertSyncLogForSyncWorkerTest(t, db, 3, "failed", 2)
	insertSyncLogForSyncWorkerTest(t, db, 4, "pending", 1)

	apiIDs, err := w.findPendingEpisodes(context.Background(), true)
	if err != nil {
		t.Fatalf("api pending query failed: %v", err)
	}
	assertEpisodeIDs(t, apiIDs, []int64{1, 2, 3})

	pollIDs, err := w.findPendingEpisodes(context.Background(), false)
	if err != nil {
		t.Fatalf("poll pending query failed: %v", err)
	}
	assertEpisodeIDs(t, pollIDs, []int64{1, 3})
}

func TestFindPendingEpisodes_SkipsNonRetryableFailuresFromPollingOnly(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db, cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3}}

	insertEpisodeForSyncWorkerTest(t, db, 5, "approved", false)
	insertEpisodeForSyncWorkerTest(t, db, 6, "approved", false)
	insertNonRetryableSyncLogForSyncWorkerTest(t, db, 6, "failed", 1)

	apiIDs, err := w.findPendingEpisodes(context.Background(), true)
	if err != nil {
		t.Fatalf("api pending query failed: %v", err)
	}
	assertEpisodeIDs(t, apiIDs, []int64{5, 6})

	pollIDs, err := w.findPendingEpisodes(context.Background(), false)
	if err != nil {
		t.Fatalf("poll pending query failed: %v", err)
	}
	assertEpisodeIDs(t, pollIDs, []int64{5})
}

func TestEnqueueEpisodeManual_AllowsExhaustedRetryEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 10, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 10, "failed", 3)

	if err := w.EnqueueEpisodeManual(context.Background(), 10); err != nil {
		t.Fatalf("manual enqueue failed: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 10)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 0 {
		t.Fatalf("latest attempt_count = %d, want 0 for fresh manual chain", latest.AttemptCount)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 10); count != 2 {
		t.Fatalf("sync log count = %d, want failed history plus fresh pending", count)
	}

	select {
	case got := <-w.enqueueCh:
		if got.episodeID != 10 {
			t.Fatalf("unexpected episode id: got %d want 10", got.episodeID)
		}
		if !got.manual {
			t.Fatal("expected manual mode for EnqueueEpisodeManual")
		}
	default:
		t.Fatal("expected episode to be enqueued")
	}
}

func TestEnqueueEpisodeManualForBulkRunCanCancelPendingWork(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 30, "approved", false)

	w := NewSyncWorker(db, nil, nil, "", SyncWorkerConfig{MaxRetries: 3}, nil)
	w.running.Store(true)

	if err := w.EnqueueEpisodeManualForBulkRun(context.Background(), 30, "bulk_sync_001"); err != nil {
		t.Fatalf("bulk manual enqueue failed: %v", err)
	}

	var queued struct {
		Status    string `db:"status"`
		BulkRunID string `db:"bulk_run_id"`
	}
	if err := db.Get(&queued, `SELECT status, bulk_run_id FROM sync_logs WHERE episode_id = 30`); err != nil {
		t.Fatalf("query queued sync log: %v", err)
	}
	if queued.Status != "pending" || queued.BulkRunID != "bulk_sync_001" {
		t.Fatalf("queued sync log = %+v, want pending bulk_sync_001", queued)
	}

	canceled, err := w.CancelBulkRun(context.Background(), "bulk_sync_001")
	if err != nil {
		t.Fatalf("cancel bulk run: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled count = %d, want 1", canceled)
	}
	if got := latestSyncLogForSyncWorkerTest(t, db, 30); got.Status != "canceled" {
		t.Fatalf("latest status = %q, want canceled", got.Status)
	}
	if _, _, err := w.acquireSyncLogWithMode(context.Background(), 30, "local/episode.mcap", true); !errors.Is(err, errSyncCanceled) {
		t.Fatalf("claim canceled bulk work error = %v, want %v", err, errSyncCanceled)
	}

	retryWorker := NewSyncWorker(db, nil, nil, "", SyncWorkerConfig{BatchSize: 10, MaxRetries: 3}, nil)
	retryWorker.running.Store(true)
	autoQueued, err := retryWorker.EnqueuePendingEpisodes(context.Background())
	if err != nil {
		t.Fatalf("scan canceled episode: %v", err)
	}
	if autoQueued != 0 {
		t.Fatalf("auto queued canceled episode count = %d, want 0", autoQueued)
	}
	if err := retryWorker.EnqueueEpisodeManual(context.Background(), 30); err != nil {
		t.Fatalf("manual retry after cancellation failed: %v", err)
	}
	if got := latestSyncLogForSyncWorkerTest(t, db, 30); got.Status != "pending" {
		t.Fatalf("manual retry latest status = %q, want pending", got.Status)
	}
}

func TestCancelBulkRunPreservesExhaustedFailure(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	insertEpisodeForSyncWorkerTest(t, db, 31, "approved", false)
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, bulk_run_id, status, attempt_count, next_retry_at)
		VALUES (?, ?, 'failed', 3, ?)
	`, 31, "bulk_sync_exhausted", time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatalf("insert exhausted bulk sync failure: %v", err)
	}

	w := NewSyncWorker(db, nil, nil, "", SyncWorkerConfig{MaxRetries: 3}, nil)
	canceled, err := w.CancelBulkRun(context.Background(), "bulk_sync_exhausted")
	if err != nil {
		t.Fatalf("cancel bulk run: %v", err)
	}
	if canceled != 0 {
		t.Fatalf("canceled count = %d, want 0", canceled)
	}
	got := latestSyncLogForSyncWorkerTest(t, db, 31)
	if got.Status != "failed" || !got.NextRetry.Valid {
		t.Fatalf("exhausted bulk sync log = %+v, want failed with next_retry_at preserved", got)
	}
}

func TestEnqueueEpisodeManual_PromotesDueFailureToPending(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 13, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 13, "failed", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 13); err != nil {
		t.Fatalf("manual enqueue failed: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 13)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want completed attempt count 1", latest.AttemptCount)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 13); count != 1 {
		t.Fatalf("sync log count = %d, want reused failed row", count)
	}
}

func TestEnqueueEpisodeResyncRejectsAlreadySyncedEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 27, "approved", true)
	insertSyncLogForSyncWorkerTest(t, db, 27, "completed", 1)

	if err := w.EnqueueEpisodeResync(context.Background(), 27); !errors.Is(err, ErrEpisodeAlreadySynced) {
		t.Fatalf("resync enqueue error = %v, want ErrEpisodeAlreadySynced", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 27)
	if latest.Status != "completed" {
		t.Fatalf("latest status = %q, want completed", latest.Status)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 27); count != 1 {
		t.Fatalf("sync log count = %d, want unchanged completed history", count)
	}

	select {
	case got := <-w.enqueueCh:
		t.Fatalf("unexpected resync enqueue: %+v", got)
	default:
	}
}

func TestDispatchPendingSyncLogsSkipsAlreadySyncedEpisodes(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 28, "approved", true)
	insertSyncLogForSyncWorkerTest(t, db, 28, "pending", 0)

	w.dispatchPendingSyncLogs(context.Background())

	select {
	case got := <-w.jobCh:
		t.Fatalf("unexpected synced episode dispatch: %+v", got)
	default:
	}
}

func TestEnqueueEpisode_RejectsInProgressEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 11, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 11, "in_progress", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 11); !errors.Is(err, ErrSyncAlreadyInProgress) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncAlreadyInProgress", err)
	}
}

func TestEnqueueEpisodeManual_PersistsPendingWhenMemoryQueueFull(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 14, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 14, "failed", 3)

	if err := w.EnqueueEpisodeManual(context.Background(), 14); err != nil {
		t.Fatalf("manual enqueue failed despite durable pending: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 14)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if !w.tryMarkEnqueued(14) {
		t.Fatal("episode marker remained set after enqueue channel was full")
	}
	w.unmarkEnqueued(14)
}

func TestEnqueueEpisodeManual_RejectsPendingEpisode(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 12, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 12, "pending", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 12); !errors.Is(err, ErrSyncAlreadyInProgress) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncAlreadyInProgress", err)
	}
}

func TestEnqueueEpisodeManual_AllowsNonRetryableFailure(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 24, "approved", false)
	insertNonRetryableSyncLogForSyncWorkerTest(t, db, 24, "failed", 1)

	if err := w.EnqueueEpisodeManual(context.Background(), 24); err != nil {
		t.Fatalf("manual enqueue failed: %v", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 24)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 0 {
		t.Fatalf("latest attempt_count = %d, want fresh pending attempt count 0", latest.AttemptCount)
	}
	if count := countSyncLogsForSyncWorkerTest(t, db, 24); count != 2 {
		t.Fatalf("sync log count = %d, want failed history plus fresh pending", count)
	}
}

func TestEnqueuePendingEpisodes_PersistsPendingWhenMemoryQueueFull(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	insertEpisodeForSyncWorkerTest(t, db, 15, "approved", false)

	count, err := w.EnqueuePendingEpisodes(context.Background())
	if err != nil {
		t.Fatalf("enqueue pending episodes failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("enqueued count = %d, want 1", count)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 15)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
}

func TestDispatchPendingSyncLogs_DispatchesPersistedRows(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 16, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 16, "pending", 0)

	w.dispatchPendingSyncLogs(context.Background())

	select {
	case got := <-w.jobCh:
		if got.episodeID != 16 {
			t.Fatalf("unexpected episode id: got %d want 16", got.episodeID)
		}
		if got.manual {
			t.Fatal("unexpected manual mode for recovered pending row")
		}
	default:
		t.Fatal("expected persisted pending row to be dispatched")
	}
}

func TestPollAndProcess_SkipsAutoDiscoveryWhenDisabled(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3, AutoScanEnabled: false},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 20, "approved", false)

	w.pollAndProcess(context.Background())

	if count := countSyncLogsForSyncWorkerTest(t, db, 20); count != 0 {
		t.Fatalf("sync log count = %d, want 0 when auto scan is disabled", count)
	}
	select {
	case got := <-w.jobCh:
		t.Fatalf("unexpected job dispatched with auto scan disabled: %+v", got)
	default:
	}
}

func TestPollAndProcess_DispatchesPendingRowsWhenAutoDiscoveryDisabled(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3, AutoScanEnabled: false},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 21, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 21, "pending", 0)

	w.pollAndProcess(context.Background())

	select {
	case got := <-w.jobCh:
		if got.episodeID != 21 {
			t.Fatalf("unexpected episode id: got %d want 21", got.episodeID)
		}
	default:
		t.Fatal("expected pending row to be dispatched with auto scan disabled")
	}
}

func TestPollAndProcess_RetriesDueFailuresWhenAutoDiscoveryDisabled(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3, AutoScanEnabled: false},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 22, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 22, "failed", 1)

	w.pollAndProcess(context.Background())

	latest := latestSyncLogForSyncWorkerTest(t, db, 22)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	select {
	case got := <-w.jobCh:
		if got.episodeID != 22 {
			t.Fatalf("unexpected episode id: got %d want 22", got.episodeID)
		}
	default:
		t.Fatal("expected retryable failure to be dispatched with auto scan disabled")
	}
}

func TestPollAndProcess_DiscoversPendingEpisodesWhenAutoScanEnabled(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3, AutoScanEnabled: true},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 23, "approved", false)

	w.pollAndProcess(context.Background())

	latest := latestSyncLogForSyncWorkerTest(t, db, 23)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	select {
	case got := <-w.jobCh:
		if got.episodeID != 23 {
			t.Fatalf("unexpected episode id: got %d want 23", got.episodeID)
		}
	default:
		t.Fatal("expected auto-discovered episode to be dispatched")
	}
}

func TestRetryFailedEpisodes_PromotesDueFailureToPendingBeforeDispatch(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertEpisodeForSyncWorkerTest(t, db, 17, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 17, "failed", 1)

	w.retryFailedEpisodes(context.Background())

	latest := latestSyncLogForSyncWorkerTest(t, db, 17)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want completed attempt count 1", latest.AttemptCount)
	}
	select {
	case got := <-w.jobCh:
		if got.episodeID != 17 {
			t.Fatalf("unexpected episode id: got %d want 17", got.episodeID)
		}
	default:
		t.Fatal("expected retryable failure to be dispatched")
	}
}

func TestRetryFailedEpisodesIgnoresMissingDeletedAndAlreadySyncedEpisodes(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:              db,
		cfg:             SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
		jobCh:           make(chan syncEnqueueRequest, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	insertSyncLogForSyncWorkerTest(t, db, 2, "failed", 1)
	insertEpisodeForSyncWorkerTest(t, db, 3, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 3, "failed", 1)
	if _, err := db.Exec(`UPDATE episodes SET deleted_at = ? WHERE id = 3`, time.Now().UTC()); err != nil {
		t.Fatalf("mark episode deleted: %v", err)
	}
	insertEpisodeForSyncWorkerTest(t, db, 4, "approved", true)
	insertSyncLogForSyncWorkerTest(t, db, 4, "failed", 1)
	insertEpisodeForSyncWorkerTest(t, db, 5, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 5, "failed", 1)

	var logs bytes.Buffer
	previousLogger := logger.Get()
	logger.Set(log.New(&logs, "", 0))
	t.Cleanup(func() { logger.Set(previousLogger) })

	w.retryFailedEpisodes(context.Background())

	if strings.Contains(logs.String(), "Failed to queue retry") {
		t.Fatalf("unexpected retry queue failure log: %s", logs.String())
	}

	if latest := latestSyncLogForSyncWorkerTest(t, db, 4); latest.Status != "failed" {
		t.Fatalf("synced episode latest status = %q, want failed unchanged", latest.Status)
	}
	if latest := latestSyncLogForSyncWorkerTest(t, db, 5); latest.Status != "pending" {
		t.Fatalf("unsynced episode latest status = %q, want pending", latest.Status)
	}

	gotUnsynced := <-w.jobCh
	if gotUnsynced.episodeID != 5 {
		t.Fatalf("unexpected unsynced retry dispatch: got %+v want episode 5", gotUnsynced)
	}
	select {
	case got := <-w.jobCh:
		t.Fatalf("unexpected additional retry dispatch: %+v", got)
	default:
	}
}

func TestAcquireSyncLogWithMode_ClaimsFreshPendingRow(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:  db,
		cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
	}

	insertEpisodeForSyncWorkerTest(t, db, 18, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 18, "pending", 0)

	syncLogID, attemptCount, err := w.acquireSyncLogWithMode(context.Background(), 18, "local/episode.mcap", false)
	if err != nil {
		t.Fatalf("claim pending sync log failed: %v", err)
	}
	if syncLogID <= 0 {
		t.Fatalf("syncLogID = %d, want positive id", syncLogID)
	}
	if attemptCount != 1 {
		t.Fatalf("attemptCount = %d, want 1", attemptCount)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 18)
	if latest.Status != "in_progress" {
		t.Fatalf("latest status = %q, want in_progress", latest.Status)
	}
	if latest.AttemptCount != 1 {
		t.Fatalf("latest attempt_count = %d, want 1", latest.AttemptCount)
	}
}

func TestAcquireSyncLogWithMode_ClaimsRetryPendingRow(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:  db,
		cfg: SyncWorkerConfig{BatchSize: 10, MaxRetries: 3},
	}

	insertEpisodeForSyncWorkerTest(t, db, 19, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 19, "pending", 1)

	_, attemptCount, err := w.acquireSyncLogWithMode(context.Background(), 19, "local/episode.mcap", false)
	if err != nil {
		t.Fatalf("claim retry pending sync log failed: %v", err)
	}
	if attemptCount != 2 {
		t.Fatalf("attemptCount = %d, want retry attempt 2", attemptCount)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 19)
	if latest.Status != "in_progress" {
		t.Fatalf("latest status = %q, want in_progress", latest.Status)
	}
	if latest.AttemptCount != 2 {
		t.Fatalf("latest attempt_count = %d, want 2", latest.AttemptCount)
	}
}

func TestProcessEnqueuedEpisode_HoldsMarkerUntilProcessingReturns(t *testing.T) {
	w := &SyncWorker{
		enqueuedEpisode: map[int64]struct{}{77: {}},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		w.processEnqueuedEpisodeWith(
			context.Background(),
			syncEnqueueRequest{episodeID: 77, manual: true},
			func(context.Context, int64, bool) {
				close(started)
				<-release
			},
		)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("processing did not start")
	}

	if w.tryMarkEnqueued(77) {
		t.Fatal("episode marker was released while processing was still active")
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processing did not finish")
	}

	if !w.tryMarkEnqueued(77) {
		t.Fatal("episode marker was not released after processing finished")
	}
}

func TestEnqueueEpisode_ReturnsQueueFull(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest),
		enqueuedEpisode: make(map[int64]struct{}),
	}
	w.running.Store(true)

	if err := w.EnqueueEpisode(context.Background(), 99); !errors.Is(err, ErrSyncQueueFull) {
		t.Fatalf("enqueue error = %v, want ErrSyncQueueFull", err)
	}
}

func TestEnqueueEpisodeManual_ReturnsNotRunningWhenWorkerNotStarted(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan syncEnqueueRequest, 1),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	if err := w.EnqueueEpisodeManual(context.Background(), 123); !errors.Is(err, ErrSyncWorkerNotRunning) {
		t.Fatalf("manual enqueue error = %v, want ErrSyncWorkerNotRunning", err)
	}
}

func TestNextRetryDelay_UsesMinuteScaleBackoff(t *testing.T) {
	w := &SyncWorker{
		cfg: SyncWorkerConfig{
			RetryBaseSec:   30,
			RetryMaxSec:    1800,
			RetryJitterSec: 0,
		},
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 30 * time.Second},
		{attempt: 2, want: 60 * time.Second},
		{attempt: 3, want: 120 * time.Second},
		{attempt: 4, want: 240 * time.Second},
		{attempt: 10, want: 1800 * time.Second},
	}

	for _, tt := range tests {
		got := w.nextRetryDelay(tt.attempt)
		if got != tt.want {
			t.Fatalf("nextRetryDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestNextRetryDelay_IncludesBoundedJitter(t *testing.T) {
	w := &SyncWorker{
		cfg: SyncWorkerConfig{
			RetryBaseSec:   30,
			RetryMaxSec:    1800,
			RetryJitterSec: 30,
		},
	}

	got := w.nextRetryDelay(3)
	min := 120 * time.Second
	max := 150 * time.Second
	if got < min || got > max {
		t.Fatalf("nextRetryDelay with jitter = %v, want [%v, %v]", got, min, max)
	}
}

func TestMarkSyncFailed_NonRetryableClearsNextRetry(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{
		db:  db,
		cfg: SyncWorkerConfig{RetryBaseSec: 30, RetryMaxSec: 1800},
	}

	insertEpisodeForSyncWorkerTest(t, db, 25, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 25, "in_progress", 1)
	var syncLogID int64
	if err := db.Get(&syncLogID, "SELECT id FROM sync_logs WHERE episode_id = ?", 25); err != nil {
		t.Fatalf("query sync log id: %v", err)
	}

	w.markSyncFailed(context.Background(), syncLogID, 25, 0, newNonRetryableSyncError("asset_id missing"), 1)

	latest := latestSyncLogForSyncWorkerTest(t, db, 25)
	if latest.Status != "failed" {
		t.Fatalf("latest status = %q, want failed", latest.Status)
	}
	if latest.NextRetry.Valid {
		t.Fatalf("next_retry_at valid = true, want NULL")
	}
}

func TestMarkSyncCompleted_WritesExistingCloudFields(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db}

	insertEpisodeForSyncWorkerTest(t, db, 26, "approved", false)
	insertSyncLogForSyncWorkerTest(t, db, 26, "in_progress", 1)
	var syncLogID int64
	if err := db.Get(&syncLogID, "SELECT id FROM sync_logs WHERE episode_id = ?", 26); err != nil {
		t.Fatalf("query sync log id: %v", err)
	}

	w.markSyncCompleted(context.Background(), syncLogID, 26, &cloud.UploadResult{
		LogicalUploadID: "logical-26",
		UploadID:        "upload-26",
		ObjectKey:       "cloud/object.mcap",
		FileSize:        12345,
	}, 3)

	var ep struct {
		CloudSynced    bool   `db:"cloud_synced"`
		CloudMcapPath  string `db:"cloud_mcap_path"`
		CloudProcessed bool   `db:"cloud_processed"`
	}
	if err := db.Get(&ep, "SELECT cloud_synced, cloud_mcap_path, cloud_processed FROM episodes WHERE id = ?", 26); err != nil {
		t.Fatalf("query episode cloud fields: %v", err)
	}
	if !ep.CloudSynced || ep.CloudMcapPath != "cloud/object.mcap" || ep.CloudProcessed {
		t.Fatalf("episode cloud fields = %+v", ep)
	}

	var logRow struct {
		Status           string `db:"status"`
		DestinationPath  string `db:"destination_path"`
		BytesTransferred int64  `db:"bytes_transferred"`
	}
	if err := db.Get(&logRow, "SELECT status, destination_path, bytes_transferred FROM sync_logs WHERE id = ?", syncLogID); err != nil {
		t.Fatalf("query sync log completion fields: %v", err)
	}
	if logRow.Status != "completed" || logRow.DestinationPath != "cloud/object.mcap" || logRow.BytesTransferred != 12345 {
		t.Fatalf("sync log completion fields = %+v", logRow)
	}
}

func TestAcquireSyncLogRejectsEpisodeThatIsNoLongerApproved(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db}

	insertEpisodeForSyncWorkerTest(t, db, 27, "manual_review_failed", false)
	insertSyncLogForSyncWorkerTest(t, db, 27, "pending", 0)

	_, _, err := w.acquireSyncLogWithMode(context.Background(), 27, "bucket/path.mcap", false)
	if err == nil || !strings.Contains(err.Error(), "must be approved") {
		t.Fatalf("acquireSyncLogWithMode() error = %v, want QA approval error", err)
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 27)
	if latest.Status != "pending" {
		t.Fatalf("latest status = %q, want pending", latest.Status)
	}
}

func TestMarkSyncCompletedRejectsEpisodeThatIsNoLongerApproved(t *testing.T) {
	db := newTestSyncWorkerDB(t)
	w := &SyncWorker{db: db}

	insertEpisodeForSyncWorkerTest(t, db, 28, "failed", false)
	insertSyncLogForSyncWorkerTest(t, db, 28, "in_progress", 1)
	var syncLogID int64
	if err := db.Get(&syncLogID, "SELECT id FROM sync_logs WHERE episode_id = ?", 28); err != nil {
		t.Fatalf("query sync log id: %v", err)
	}

	w.markSyncCompleted(context.Background(), syncLogID, 28, &cloud.UploadResult{
		LogicalUploadID: "logical-28",
		UploadID:        "upload-28",
		ObjectKey:       "cloud/object.mcap",
		FileSize:        12345,
	}, 3)

	var cloudSynced bool
	if err := db.Get(&cloudSynced, "SELECT cloud_synced FROM episodes WHERE id = ?", 28); err != nil {
		t.Fatalf("query episode cloud_synced: %v", err)
	}
	if cloudSynced {
		t.Fatal("cloud_synced = true, want false")
	}

	latest := latestSyncLogForSyncWorkerTest(t, db, 28)
	if latest.Status != "failed" {
		t.Fatalf("latest status = %q, want failed", latest.Status)
	}
	if latest.NextRetry.Valid {
		t.Fatal("next_retry_at valid = true, want NULL")
	}
}

func newTestSyncWorkerDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}

	schema := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			storage_backend TEXT NOT NULL DEFAULT 'minio',
			mcap_path TEXT NOT NULL DEFAULT 'test-bucket/source.mcap',
			file_size_bytes INTEGER,
			metadata TEXT,
			qa_status TEXT NOT NULL,
			checksum TEXT,
			hilbert_raw_data_id INTEGER,
			cloud_publish_source TEXT,
			cloud_publish_claimed_at TIMESTAMP NULL,
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			cloud_synced_at TIMESTAMP NULL,
			cloud_mcap_path TEXT,
			cloud_processed BOOLEAN NOT NULL DEFAULT 0,
			duration_sec REAL,
			deleted_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE sync_logs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				episode_id INTEGER NOT NULL,
				bulk_run_id TEXT,
				source_path TEXT,
				source_snapshot TEXT,
				status TEXT NOT NULL,
				destination_path TEXT,
				bytes_transferred INTEGER,
				duration_sec INTEGER,
				error_message TEXT,
				attempt_count INTEGER NOT NULL DEFAULT 0,
				next_retry_at TIMESTAMP NULL,
				started_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL
		)`,
		`CREATE TABLE episode_derivatives (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			generation INTEGER NOT NULL,
			processing_status TEXT NOT NULL,
			qa_status TEXT NOT NULL,
			mcap_path TEXT,
			checksum TEXT,
			file_size_bytes INTEGER
		)`,
	}

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("create schema: %v", err)
		}
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func insertEpisodeForSyncWorkerTest(t *testing.T, db *sqlx.DB, id int64, qaStatus string, cloudSynced bool) {
	t.Helper()

	createdAt := time.Date(2026, 1, int(id), 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO episodes (
			id, episode_id, storage_backend, mcap_path, file_size_bytes,
			checksum, metadata, qa_status, cloud_synced, deleted_at, created_at
		) VALUES (?, ?, 'minio', 'test-bucket/source.mcap', 100,
		          ?, '{}', ?, ?, NULL, ?)
	`, id, fmt.Sprintf("episode-%d", id), strings.Repeat("a", 64), qaStatus, cloudSynced, createdAt); err != nil {
		t.Fatalf("insert episode %d: %v", id, err)
	}
}

func insertSyncLogForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64, status string, attemptCount int) {
	t.Helper()

	startedAt := time.Date(2026, 2, int(episodeID), 0, 0, 0, 0, time.UTC)
	nextRetry := sql.NullTime{}
	if status == "failed" {
		nextRetry = sql.NullTime{Time: startedAt.Add(time.Second), Valid: true}
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status, attempt_count, started_at, next_retry_at)
		VALUES (?, ?, ?, ?, ?)
	`, episodeID, status, attemptCount, startedAt, nextRetry); err != nil {
		t.Fatalf("insert sync log for episode %d: %v", episodeID, err)
	}
}

func insertNonRetryableSyncLogForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64, status string, attemptCount int) {
	t.Helper()

	startedAt := time.Date(2026, 2, int(episodeID), 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status, attempt_count, started_at, next_retry_at)
		VALUES (?, ?, ?, ?, NULL)
	`, episodeID, status, attemptCount, startedAt); err != nil {
		t.Fatalf("insert sync log for episode %d: %v", episodeID, err)
	}
}

type syncLogForSyncWorkerTest struct {
	Status       string       `db:"status"`
	AttemptCount int          `db:"attempt_count"`
	NextRetry    sql.NullTime `db:"next_retry_at"`
}

func latestSyncLogForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64) syncLogForSyncWorkerTest {
	t.Helper()

	var row syncLogForSyncWorkerTest
	if err := db.Get(&row, `
		SELECT status, attempt_count, next_retry_at
		FROM sync_logs
		WHERE episode_id = ?
		ORDER BY id DESC
		LIMIT 1
	`, episodeID); err != nil {
		t.Fatalf("query latest sync log for episode %d: %v", episodeID, err)
	}
	return row
}

func countSyncLogsForSyncWorkerTest(t *testing.T, db *sqlx.DB, episodeID int64) int {
	t.Helper()

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil {
		t.Fatalf("count sync logs for episode %d: %v", episodeID, err)
	}
	return count
}

func assertEpisodeIDs(t *testing.T, got, want []int64) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("unexpected id count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected ids: got %v want %v", got, want)
		}
	}
}
