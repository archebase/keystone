// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestEvaluateMcapMagicCheck(t *testing.T) {
	valid := append([]byte(nil), mcapMagicBytes...)
	bad := []byte{0x8b, 0xef, 0xb8, 0x75, 0xc6, 0x97, 0x96, 0x61}

	tests := []struct {
		name       string
		head       []byte
		tail       []byte
		wantPassed bool
		wantDetail string
	}{
		{
			name:       "head and tail match",
			head:       valid,
			tail:       valid,
			wantPassed: true,
			wantDetail: "MCAP head and tail magic matched",
		},
		{
			name:       "head mismatch",
			head:       bad,
			tail:       valid,
			wantPassed: false,
			wantDetail: "MCAP integrity check failed: head magic mismatch",
		},
		{
			name:       "tail mismatch",
			head:       valid,
			tail:       bad,
			wantPassed: false,
			wantDetail: "MCAP integrity check failed: tail magic mismatch",
		},
		{
			name:       "both mismatch",
			head:       bad,
			tail:       bad,
			wantPassed: false,
			wantDetail: "MCAP integrity check failed: head and tail magic mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateMcapMagicCheck(1024, tt.head, tt.tail, "")
			if got.Passed != tt.wantPassed {
				t.Fatalf("passed = %v, want %v", got.Passed, tt.wantPassed)
			}
			if got.Details != tt.wantDetail {
				t.Fatalf("details = %q, want %q", got.Details, tt.wantDetail)
			}
			if got.Metadata["expected_magic"] != "89 4d 43 41 50 30 0d 0a" {
				t.Fatalf("expected_magic metadata = %v", got.Metadata["expected_magic"])
			}
		})
	}
}

func TestEpisodeQAHandlerEnqueueEpisodeCapturesAutoSyncBeforeQueueing(t *testing.T) {
	handler := &EpisodeQAHandler{queue: make(chan int64, 1)}
	capturer := &fakeAutoSyncEpisodeCapturer{}
	handler.SetAutoSyncCapturer(capturer)

	handler.EnqueueEpisode(42)

	if capturer.episodeID != 42 {
		t.Fatalf("captured episode = %d, want 42", capturer.episodeID)
	}
	select {
	case episodeID := <-handler.queue:
		if episodeID != 42 {
			t.Fatalf("queued episode = %d, want 42", episodeID)
		}
	default:
		t.Fatal("episode was not queued for automatic QA")
	}
}

type fakeAutoSyncEpisodeCapturer struct {
	episodeID int64
}

func (f *fakeAutoSyncEpisodeCapturer) CaptureEpisode(_ context.Context, episodeID int64) (bool, error) {
	f.episodeID = episodeID
	return true, nil
}

func TestEvaluateRecordingNotEmptyCheck(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantPassed bool
		wantDetail string
	}{
		{
			name: "messages and recorded topics pass",
			body: `{
				"recording": {
					"duration_sec": 6.4,
					"file_size_bytes": 2048,
					"message_count": 12,
					"topics_recorded": ["/camera/image_raw/compressed"]
				},
				"topics_summary": []
			}`,
			wantPassed: true,
			wantDetail: "Recording sidecar reports messages and topics",
		},
		{
			name: "messages and topic summary pass",
			body: `{
				"recording": {
					"duration_sec": 6.4,
					"file_size_bytes": 2048,
					"message_count": 12,
					"topics_recorded": []
				},
				"topics_summary": [{"topic": "/camera/image_raw/compressed"}]
			}`,
			wantPassed: true,
			wantDetail: "Recording sidecar reports messages and topics",
		},
		{
			name: "empty recording fails",
			body: `{
				"recording": {
					"duration_sec": 6.461,
					"file_size_bytes": 1129,
					"message_count": 0,
					"topics_recorded": []
				},
				"topics_summary": []
			}`,
			wantPassed: false,
			wantDetail: "Recording sidecar check failed: message_count is zero and no recorded topics",
		},
		{
			name: "zero messages with topics fails",
			body: `{
				"recording": {
					"message_count": 0,
					"topics_recorded": ["/camera/image_raw/compressed"]
				}
			}`,
			wantPassed: false,
			wantDetail: "Recording sidecar check failed: message_count is zero",
		},
		{
			name: "messages without topics fail",
			body: `{
				"recording": {
					"message_count": 12,
					"topics_recorded": []
				},
				"topics_summary": []
			}`,
			wantPassed: false,
			wantDetail: "Recording sidecar check failed: no recorded topics",
		},
		{
			name:       "invalid json fails",
			body:       `{`,
			wantPassed: false,
			wantDetail: "Recording sidecar check failed: invalid sidecar JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evaluateRecordingNotEmptyCheck([]byte(tt.body), nil)
			if err != nil {
				t.Fatalf("evaluate recording check: %v", err)
			}
			if got.Passed != tt.wantPassed {
				t.Fatalf("passed = %v, want %v", got.Passed, tt.wantPassed)
			}
			if got.Details != tt.wantDetail {
				t.Fatalf("details = %q, want %q", got.Details, tt.wantDetail)
			}
			if got.CheckName != episodeQACheckRecordingNotEmpty {
				t.Fatalf("check name = %q, want %q", got.CheckName, episodeQACheckRecordingNotEmpty)
			}
		})
	}
}

func TestDefaultEpisodeQASuiteUsesTarExtensionForEgoPortalE2(t *testing.T) {
	got := defaultEpisodeQASuite(episodeQACheckRow{
		DeviceType: egoPortalE2DeviceType,
		McapPath:   "device-uploads/e2/capture.tar",
	})
	want := []string{episodeQACheckTarExtension}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("suite = %v, want %v", got, want)
	}
}

func TestEvaluateTarExtensionCheck(t *testing.T) {
	for _, test := range []struct {
		path string
		pass bool
	}{
		{path: "device-uploads/e2/capture.tar", pass: true},
		{path: "device-uploads/e2/CAPTURE.TAR", pass: true},
		{path: "device-uploads/e2/capture.tar.gz", pass: false},
		{path: "device-uploads/e2/capture.mcap", pass: false},
		{path: "device-uploads/e2/capture", pass: false},
	} {
		got := evaluateTarExtensionCheck(test.path)
		if got.Passed != test.pass || got.CheckName != episodeQACheckTarExtension {
			t.Fatalf("evaluateTarExtensionCheck(%q) = %#v, want passed=%v", test.path, got, test.pass)
		}
	}
}
func TestDefaultEpisodeQASuiteIncludesRecordingNotEmptyWhenSidecarExists(t *testing.T) {
	got := defaultEpisodeQASuite(episodeQACheckRow{SidecarPath: "device-uploads/capture.json"})
	want := []string{episodeQACheckMcapMagic, episodeQACheckRecordingNotEmpty}
	if len(got) != len(want) {
		t.Fatalf("suite length = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suite[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefaultEpisodeQASuiteSkipsRecordingNotEmptyWithoutSidecar(t *testing.T) {
	got := defaultEpisodeQASuite(episodeQACheckRow{})
	want := []string{episodeQACheckMcapMagic}
	if len(got) != len(want) {
		t.Fatalf("suite length = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suite[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefaultEpisodeQASuiteSkipsRecordingNotEmptyForTOSOnlyEpisode(t *testing.T) {
	got := defaultEpisodeQASuite(episodeQACheckRow{
		SidecarPath: "device-uploads/capture.mcap.metadata.json",
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"source":"dgwcompat","bucket":"tos-bucket","object_key":"device-uploads/capture.mcap"}`,
		},
	})
	want := []string{episodeQACheckMcapMagic}
	if len(got) != len(want) {
		t.Fatalf("suite length = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suite[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSizeFromTOSRangeResponse(t *testing.T) {
	got := sizeFromTOSRangeResponse(episodeQATOSObject{
		Data:          mcapMagicBytes,
		ContentRange:  "bytes 0-7/1024",
		ContentLength: int64(len(mcapMagicBytes)),
	})
	if got != 1024 {
		t.Fatalf("size = %d, want 1024", got)
	}
}

func TestRunMcapMagicQACheckUsesTOSWithoutS3(t *testing.T) {
	handler := NewEpisodeQAHandler(nil, nil, "edge-mercury", nil, &config.StorageConfig{
		Type:           "tos",
		Endpoint:       "tos-cn-beijing.volces.com",
		Bucket:         "tos-bucket",
		Region:         "cn-beijing",
		AccessKey:      "test-ak",
		SecretKey:      "test-sk",
		UseSSL:         true,
		StorageRoleTRN: "trn:iam::123:role/storage",
	})
	handler.tos.stsClient = fakeEpisodeQASTSClient{}
	handler.tos.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Host; got != "tos-bucket.tos-cn-beijing.volces.com" {
			t.Fatalf("host = %q, want tos-bucket.tos-cn-beijing.volces.com", got)
		}
		var contentRange string
		switch got := req.Header.Get("Range"); got {
		case "bytes=0-7":
			contentRange = "bytes 0-7/16"
		case "bytes=8-15":
			contentRange = "bytes 8-15/16"
		default:
			t.Fatalf("Range = %q, want MCAP head or tail range", got)
		}
		resp := &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(string(mcapMagicBytes))),
			ContentLength: int64(len(mcapMagicBytes)),
		}
		resp.Header.Set("Content-Range", contentRange)
		return resp, nil
	})}

	outcome, err := handler.runMcapMagicQACheck(context.Background(), episodeQACheckRow{
		McapPath: "device-uploads/capture.mcap",
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"source":"dgwcompat","object_store_backend":"volcengine_tos","bucket":"tos-bucket","object_key":"device-uploads/capture.mcap"}`,
		},
	})
	if err != nil {
		t.Fatalf("run MCAP QA check: %v", err)
	}
	if !outcome.Passed {
		t.Fatalf("outcome passed = false, details=%s metadata=%#v", outcome.Details, outcome.Metadata)
	}
}

func TestRunRecordingNotEmptyQACheckUsesTOSWithoutS3(t *testing.T) {
	handler := NewEpisodeQAHandler(nil, nil, "edge-mercury", nil, &config.StorageConfig{
		Type:           "tos",
		Endpoint:       "tos-cn-beijing.volces.com",
		Bucket:         "tos-bucket",
		Region:         "cn-beijing",
		AccessKey:      "test-ak",
		SecretKey:      "test-sk",
		UseSSL:         true,
		StorageRoleTRN: "trn:iam::123:role/storage",
	})
	handler.tos.stsClient = fakeEpisodeQASTSClient{}
	body := `{"recording":{"message_count":1,"topics_recorded":["/camera"]}}`
	handler.tos.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Host; got != "tos-bucket.tos-cn-beijing.volces.com" {
			t.Fatalf("host = %q, want tos-bucket.tos-cn-beijing.volces.com", got)
		}
		if got := req.Header.Get("Range"); got == "" {
			t.Fatal("Range header is empty")
		}
		resp := &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}
		resp.Header.Set("Content-Range", "bytes 0-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
		return resp, nil
	})}

	outcome, err := handler.runRecordingNotEmptyQACheck(context.Background(), episodeQACheckRow{
		SidecarPath: "device-uploads/capture.json",
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"source":"dgwcompat","object_store_backend":"volcengine_tos","bucket":"tos-bucket","object_key":"device-uploads/capture.mcap"}`,
		},
	})
	if err != nil {
		t.Fatalf("run recording QA check: %v", err)
	}
	if !outcome.Passed {
		t.Fatalf("outcome passed = false, details=%s metadata=%#v", outcome.Details, outcome.Metadata)
	}
}

func TestResolveEpisodeObjectLocationUsesMetadataBucket(t *testing.T) {
	metadata := sql.NullString{
		Valid:  true,
		String: `{"source":"dgwcompat","bucket":"tos-bucket","object_key":"device-uploads/capture.mcap"}`,
	}

	bucket, objectName, ok := resolveEpisodeObjectLocation("edge-factory-test", "device-uploads/capture.mcap", metadata)
	if !ok {
		t.Fatal("resolveEpisodeObjectLocation() ok = false, want true")
	}
	if bucket != "tos-bucket" {
		t.Fatalf("bucket = %q, want tos-bucket", bucket)
	}
	if objectName != "device-uploads/capture.mcap" {
		t.Fatalf("objectName = %q, want device-uploads/capture.mcap", objectName)
	}
}

func TestResolveEpisodeObjectLocationFallsBackToConfiguredBucket(t *testing.T) {
	bucket, objectName, ok := resolveEpisodeObjectLocation("edge-factory-test", "device-uploads/capture.mcap", sql.NullString{})
	if !ok {
		t.Fatal("resolveEpisodeObjectLocation() ok = false, want true")
	}
	if bucket != "edge-factory-test" {
		t.Fatalf("bucket = %q, want edge-factory-test", bucket)
	}
	if objectName != "device-uploads/capture.mcap" {
		t.Fatalf("objectName = %q, want device-uploads/capture.mcap", objectName)
	}
}

func TestPersistEpisodeQACheckFailureMarksEpisodeFailed(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, deleted_at)
		VALUES (1, 'qa_running', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   "MCAP integrity check failed: tail magic mismatch",
		Metadata: map[string]any{
			"expected_magic":   "89 4d 43 41 50 30 0d 0a",
			"found_tail_magic": "8b ef b8 75 c6 97 96 61",
		},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusApproved,
		MutableStatus:  true,
	}
	result, err := handler.persistEpisodeQASuiteResult(context.Background(), claim, qaRunModeManual, []episodeQACheckOutcome{outcome}, time.Now().UTC())
	if err != nil {
		t.Fatalf("persist qa check: %v", err)
	}
	if result.QAStatus != qaStatusFailed {
		t.Fatalf("result qa_status = %q, want failed", result.QAStatus)
	}

	var episode struct {
		QaStatus    string `db:"qa_status"`
		QualityFlag string `db:"quality_flag"`
	}
	if err := db.Get(&episode, "SELECT qa_status, quality_flag FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QaStatus != "failed" {
		t.Fatalf("qa_status = %q, want failed", episode.QaStatus)
	}
	if episode.QualityFlag != outcome.Details {
		t.Fatalf("quality_flag = %q, want %q", episode.QualityFlag, outcome.Details)
	}

	var count int
	if err := db.Get(&count, "SELECT COUNT(1) FROM qa_checks WHERE episode_id = 1 AND check_name = 'mcap_magic' AND passed = FALSE"); err != nil {
		t.Fatalf("count qa_checks: %v", err)
	}
	if count != 1 {
		t.Fatalf("failed qa_check count = %d, want 1", count)
	}
}

func TestPersistEpisodeQACheckManualFailureRejectsCloudSyncedEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, cloud_synced, quality_flag, deleted_at)
		VALUES (1, 'qa_running', TRUE, NULL, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   "MCAP integrity check failed",
		Metadata:  map[string]any{},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusApproved,
		Mode:           qaRunModeManual,
		MutableStatus:  true,
	}
	if _, err := handler.persistEpisodeQASuiteResult(
		context.Background(),
		claim,
		qaRunModeManual,
		[]episodeQACheckOutcome{outcome},
		time.Now().UTC(),
	); !errors.Is(err, errEpisodeQACloudSynced) {
		t.Fatalf("persistEpisodeQASuiteResult() error = %v, want errEpisodeQACloudSynced", err)
	}

	var checkCount int
	if err := db.Get(&checkCount, `SELECT COUNT(*) FROM qa_checks WHERE episode_id = 1`); err != nil {
		t.Fatalf("count qa checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("qa check count = %d, want 0", checkCount)
	}
}

func TestPersistEpisodeQACheckManualFailureRejectsActiveCloudSync(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, cloud_synced, quality_flag, deleted_at)
		VALUES (1, 'qa_running', FALSE, NULL, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status)
		VALUES (1, 'in_progress')
	`); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   "MCAP integrity check failed",
		Metadata:  map[string]any{},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusApproved,
		Mode:           qaRunModeManual,
		MutableStatus:  true,
	}
	if _, err := handler.persistEpisodeQASuiteResult(
		context.Background(),
		claim,
		qaRunModeManual,
		[]episodeQACheckOutcome{outcome},
		time.Now().UTC(),
	); !errors.Is(err, errEpisodeQASyncActive) {
		t.Fatalf("persistEpisodeQASuiteResult() error = %v, want errEpisodeQASyncActive", err)
	}

	var checkCount int
	if err := db.Get(&checkCount, `SELECT COUNT(*) FROM qa_checks WHERE episode_id = 1`); err != nil {
		t.Fatalf("count qa checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("qa check count = %d, want 0", checkCount)
	}
}

func TestPersistEpisodeQACheckManualSuccessRestoresFailedEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, deleted_at)
		VALUES (1, 'qa_running', 'previous failure', NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    true,
		Score:     1,
		Details:   "MCAP head and tail magic matched",
		Metadata: map[string]any{
			"expected_magic": "89 4d 43 41 50 30 0d 0a",
		},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusFailed,
		MutableStatus:  true,
	}
	result, err := handler.persistEpisodeQASuiteResult(context.Background(), claim, qaRunModeManual, []episodeQACheckOutcome{outcome}, time.Now().UTC())
	if err != nil {
		t.Fatalf("persist qa check: %v", err)
	}
	if result.QAStatus != qaStatusApproved {
		t.Fatalf("result qa_status = %q, want approved", result.QAStatus)
	}

	var episode struct {
		QaStatus    string         `db:"qa_status"`
		QualityFlag sql.NullString `db:"quality_flag"`
	}
	if err := db.Get(&episode, "SELECT qa_status, quality_flag FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QaStatus != "approved" {
		t.Fatalf("qa_status = %q, want approved", episode.QaStatus)
	}
	if episode.QualityFlag.Valid {
		t.Fatalf("quality_flag = %q, want NULL", episode.QualityFlag.String)
	}
}

func TestPersistEpisodeQACheckAutoSuccessAutoApprovesEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, auto_approved, deleted_at)
		VALUES (1, 'qa_running', NULL, 0, NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    true,
		Score:     1,
		Details:   "MCAP head and tail magic matched",
		Metadata: map[string]any{
			"expected_magic": "89 4d 43 41 50 30 0d 0a",
		},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusPendingQA,
		MutableStatus:  true,
	}
	result, err := handler.persistEpisodeQASuiteResult(context.Background(), claim, qaRunModeAuto, []episodeQACheckOutcome{outcome}, time.Now().UTC())
	if err != nil {
		t.Fatalf("persist qa check: %v", err)
	}
	if result.QAStatus != qaStatusApproved || !result.Passed {
		t.Fatalf("unexpected result: %+v", result)
	}

	var episode struct {
		QaStatus     string         `db:"qa_status"`
		QualityFlag  sql.NullString `db:"quality_flag"`
		AutoApproved bool           `db:"auto_approved"`
	}
	if err := db.Get(&episode, "SELECT qa_status, quality_flag, auto_approved FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QaStatus != qaStatusApproved {
		t.Fatalf("qa_status = %q, want approved", episode.QaStatus)
	}
	if !episode.AutoApproved {
		t.Fatalf("auto_approved = false, want true")
	}
	if episode.QualityFlag.Valid {
		t.Fatalf("quality_flag = %q, want NULL", episode.QualityFlag.String)
	}
}

func TestPersistEpisodeQACheckPublishesTerminalStatusEvent(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	broker := services.NewDeviceStateBroker()
	handler := &EpisodeQAHandler{db: db}
	handler.SetDeviceStateBroker(broker)
	events, unsubscribe := broker.Subscribe(1)
	defer unsubscribe()

	for _, statement := range []string{
		`INSERT INTO robots (id, device_id) VALUES (9, 'robot-1')`,
		`INSERT INTO workstations (id, robot_id) VALUES (3, 9)`,
		`INSERT INTO tasks (id, task_id, workstation_id) VALUES (1, 'task-1', 3)`,
		`INSERT INTO episodes (id, task_id, workstation_id, dc_plan_id, qa_status) VALUES (1, 1, 3, 10, 'qa_running')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed QA event fixture: %v", err)
		}
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   "MCAP integrity check failed",
		Metadata:  map[string]any{},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusPendingQA,
		MutableStatus:  true,
	}
	if _, err := handler.persistEpisodeQASuiteResult(
		context.Background(),
		claim,
		qaRunModeAuto,
		[]episodeQACheckOutcome{outcome},
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("persist qa check: %v", err)
	}

	select {
	case event := <-events:
		if event["type"] != "plan_progress_changed" || event["device_id"] != "robot-1" ||
			event["task_id"] != "task-1" || event["dc_plan_id"] != int64(10) ||
			event["qa_status"] != qaStatusFailed || event["reason"] != "qa_status_changed" {
			t.Fatalf("unexpected QA status event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for QA status event")
	}
}

func TestManualReviewChangesPublishStatusEvents(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	broker := services.NewDeviceStateBroker()
	handler := &EpisodeQAHandler{db: db}
	handler.SetDeviceStateBroker(broker)
	events, unsubscribe := broker.Subscribe(2)
	defer unsubscribe()

	for _, statement := range []string{
		`INSERT INTO robots (id, device_id) VALUES (9, 'robot-1')`,
		`INSERT INTO workstations (id, robot_id) VALUES (3, 9)`,
		`INSERT INTO tasks (id, task_id, workstation_id) VALUES (1, 'task-1', 3)`,
		`INSERT INTO episodes (id, task_id, workstation_id, dc_plan_id, mcap_path, sidecar_path, qa_status)
		 VALUES (1, 1, 3, 10, 'bucket/task-1.mcap', 'bucket/task-1.json', 'approved')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed manual review event fixture: %v", err)
		}
	}

	if _, err := handler.MarkEpisodeManualReviewFailed(context.Background(), 1, "bad data"); err != nil {
		t.Fatalf("mark manual review failed: %v", err)
	}
	assertEpisodeQAStatusEvent(t, events, qaStatusManualReviewFailed)

	if _, err := handler.CancelEpisodeManualReviewFailed(context.Background(), 1, "restore"); err != nil {
		t.Fatalf("cancel manual review failed: %v", err)
	}
	assertEpisodeQAStatusEvent(t, events, qaStatusPendingQA)
}

func assertEpisodeQAStatusEvent(t *testing.T, events <-chan services.DeviceStateEvent, wantStatus string) {
	t.Helper()
	select {
	case event := <-events:
		if event["type"] != "plan_progress_changed" || event["device_id"] != "robot-1" ||
			event["task_id"] != "task-1" || event["dc_plan_id"] != int64(10) ||
			event["qa_status"] != wantStatus || event["reason"] != "qa_status_changed" {
			t.Fatalf("unexpected QA status event: %#v", event)
		}
	default:
		t.Fatalf("QA status %q did not publish an event", wantStatus)
	}
}

func TestPersistEpisodeQACheckManualFailureMarksApprovedEpisodeFailed(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, deleted_at)
		VALUES (1, 'qa_running', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	outcome := episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   "MCAP integrity check failed: tail magic mismatch",
		Metadata: map[string]any{
			"expected_magic": "89 4d 43 41 50 30 0d 0a",
		},
	}
	claim := episodeQARunClaim{
		EpisodeID:      1,
		OriginalStatus: qaStatusApproved,
		MutableStatus:  true,
	}
	result, err := handler.persistEpisodeQASuiteResult(context.Background(), claim, qaRunModeManual, []episodeQACheckOutcome{outcome}, time.Now().UTC())
	if err != nil {
		t.Fatalf("persist qa check: %v", err)
	}
	if result.QAStatus != qaStatusFailed {
		t.Fatalf("result qa_status = %q, want failed", result.QAStatus)
	}

	var episode struct {
		QaStatus    string `db:"qa_status"`
		QualityFlag string `db:"quality_flag"`
	}
	if err := db.Get(&episode, "SELECT qa_status, quality_flag FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QaStatus != qaStatusFailed {
		t.Fatalf("qa_status = %q, want failed", episode.QaStatus)
	}
	if episode.QualityFlag != outcome.Details {
		t.Fatalf("quality_flag = %q, want %q", episode.QualityFlag, outcome.Details)
	}
}

func TestClaimEpisodeQARunReturnsConflictWhenRunning(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, quality_flag, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'qa_running', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	row, err := handler.loadEpisodeForQACheck(context.Background(), 1)
	if err != nil {
		t.Fatalf("load episode: %v", err)
	}
	if _, err := handler.claimEpisodeQARun(context.Background(), row, qaRunModeManual); err != errEpisodeQAAlreadyRunning {
		t.Fatalf("claim error = %v, want errEpisodeQAAlreadyRunning", err)
	}
}

func TestRunEpisodeQASuitePublishesRunningAndReleaseProgressEvents(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	broker := services.NewDeviceStateBroker()
	handler := &EpisodeQAHandler{db: db, stateBroker: broker}
	events, unsubscribe := broker.Subscribe(2)
	defer unsubscribe()

	for _, statement := range []string{
		`INSERT INTO robots (id, device_id) VALUES (1, 'robot-1')`,
		`INSERT INTO workstations (id, robot_id) VALUES (1, 1)`,
		`INSERT INTO tasks (id, task_id, workstation_id) VALUES (1, 'task-1', 1)`,
		`INSERT INTO episodes (id, task_id, workstation_id, dc_plan_id, mcap_path, qa_status)
		 VALUES (1, 1, 1, 10, 'bucket/path.mcap', 'failed')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed QA event fixture: %v", err)
		}
	}

	if _, err := handler.RunEpisodeQASuite(context.Background(), 1, qaRunModeManual); err == nil {
		t.Fatal("RunEpisodeQASuite() error = nil, want storage configuration error")
	}

	assertEpisodeQAStatusEvent(t, events, qaStatusRunning)
	assertEpisodeQAStatusEvent(t, events, qaStatusFailed)
}

func TestRunEpisodeQASuiteRejectsCloudSyncedEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, cloud_synced, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'approved', TRUE, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	if _, err := handler.RunEpisodeQASuite(context.Background(), 1, qaRunModeManual); !errors.Is(err, errEpisodeQACloudSynced) {
		t.Fatalf("RunEpisodeQASuite() error = %v, want errEpisodeQACloudSynced", err)
	}

	var status string
	if err := db.Get(&status, `SELECT qa_status FROM episodes WHERE id = 1`); err != nil {
		t.Fatalf("query episode status: %v", err)
	}
	if status != qaStatusApproved {
		t.Fatalf("qa_status = %q, want approved", status)
	}
}

func TestRunEpisodeQASuiteRejectsEpisodeWithActiveCloudSync(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, cloud_synced, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'approved', FALSE, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status)
		VALUES (1, 'pending')
	`); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	if _, err := handler.RunEpisodeQASuite(context.Background(), 1, qaRunModeManual); !errors.Is(err, errEpisodeQASyncActive) {
		t.Fatalf("RunEpisodeQASuite() error = %v, want errEpisodeQASyncActive", err)
	}

	var status string
	if err := db.Get(&status, `SELECT qa_status FROM episodes WHERE id = 1`); err != nil {
		t.Fatalf("query episode status: %v", err)
	}
	if status != qaStatusApproved {
		t.Fatalf("qa_status = %q, want approved", status)
	}
}

func TestMarkEpisodeManualReviewFailedUpdatesStatusAndHistory(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, quality_flag, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'approved', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	result, err := handler.MarkEpisodeManualReviewFailed(context.Background(), 1, "人工复核发现数据无效")
	if err != nil {
		t.Fatalf("mark manual review failed: %v", err)
	}
	if result.QAStatus != qaStatusManualReviewFailed || result.Passed {
		t.Fatalf("result = status %q passed %v, want manual_review_failed false", result.QAStatus, result.Passed)
	}

	var episode struct {
		QAStatus    string  `db:"qa_status"`
		QAScore     float64 `db:"qa_score"`
		QualityFlag string  `db:"quality_flag"`
	}
	if err := db.Get(&episode, "SELECT qa_status, qa_score, quality_flag FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QAStatus != qaStatusManualReviewFailed {
		t.Fatalf("qa_status = %q, want manual_review_failed", episode.QAStatus)
	}
	if episode.QAScore != 0 {
		t.Fatalf("qa_score = %v, want 0", episode.QAScore)
	}
	if episode.QualityFlag != "人工复核发现数据无效" {
		t.Fatalf("quality_flag = %q, want custom details", episode.QualityFlag)
	}

	var check struct {
		CheckName string  `db:"check_name"`
		Passed    bool    `db:"passed"`
		Score     float64 `db:"score"`
		Details   string  `db:"details"`
		Metadata  string  `db:"check_metadata"`
	}
	if err := db.Get(&check, "SELECT check_name, passed, score, details, check_metadata FROM qa_checks WHERE episode_id = 1"); err != nil {
		t.Fatalf("query qa_check: %v", err)
	}
	if check.CheckName != "manual_review" || check.Passed || check.Score != 0 || check.Details != "人工复核发现数据无效" {
		t.Fatalf("qa_check = %+v, want manual review failure", check)
	}
	if check.Metadata == "" {
		t.Fatalf("check_metadata is empty")
	}
}

func TestMarkEpisodeManualReviewFailedRejectsCloudSyncedEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, cloud_synced, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'approved', TRUE, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	if _, err := handler.MarkEpisodeManualReviewFailed(context.Background(), 1, "bad data"); !errors.Is(err, errEpisodeQACloudSynced) {
		t.Fatalf("MarkEpisodeManualReviewFailed() error = %v, want errEpisodeQACloudSynced", err)
	}

	var status string
	if err := db.Get(&status, `SELECT qa_status FROM episodes WHERE id = 1`); err != nil {
		t.Fatalf("query episode status: %v", err)
	}
	if status != qaStatusApproved {
		t.Fatalf("qa_status = %q, want approved", status)
	}
	var checkCount int
	if err := db.Get(&checkCount, `SELECT COUNT(*) FROM qa_checks WHERE episode_id = 1`); err != nil {
		t.Fatalf("count qa checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("qa check count = %d, want 0", checkCount)
	}
}

func TestMarkEpisodeManualReviewFailedRejectsEpisodeWithActiveCloudSync(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	if _, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, cloud_synced, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'approved', FALSE, NULL)
	`); err != nil {
		t.Fatalf("insert episode: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (episode_id, status)
		VALUES (1, 'in_progress')
	`); err != nil {
		t.Fatalf("insert sync log: %v", err)
	}

	if _, err := handler.MarkEpisodeManualReviewFailed(context.Background(), 1, "bad data"); !errors.Is(err, errEpisodeQASyncActive) {
		t.Fatalf("MarkEpisodeManualReviewFailed() error = %v, want errEpisodeQASyncActive", err)
	}

	var status string
	if err := db.Get(&status, `SELECT qa_status FROM episodes WHERE id = 1`); err != nil {
		t.Fatalf("query episode status: %v", err)
	}
	if status != qaStatusApproved {
		t.Fatalf("qa_status = %q, want approved", status)
	}
	var checkCount int
	if err := db.Get(&checkCount, `SELECT COUNT(*) FROM qa_checks WHERE episode_id = 1`); err != nil {
		t.Fatalf("count qa checks: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("qa check count = %d, want 0", checkCount)
	}
}

func TestCancelEpisodeManualReviewFailedRestoresPendingQAAndHistory(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeQAHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, mcap_path, qa_status, qa_score, quality_flag, deleted_at)
		VALUES (1, 'bucket/path.mcap', 'manual_review_failed', 0, '人工复核不通过', NULL)
	`)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	result, err := handler.CancelEpisodeManualReviewFailed(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("cancel manual review failed: %v", err)
	}
	if result.QAStatus != qaStatusPendingQA || !result.Passed {
		t.Fatalf("result = status %q passed %v, want pending_qa true", result.QAStatus, result.Passed)
	}

	var episode struct {
		QAStatus    string          `db:"qa_status"`
		QAScore     sql.NullFloat64 `db:"qa_score"`
		QualityFlag sql.NullString  `db:"quality_flag"`
	}
	if err := db.Get(&episode, "SELECT qa_status, qa_score, quality_flag FROM episodes WHERE id = 1"); err != nil {
		t.Fatalf("query episode: %v", err)
	}
	if episode.QAStatus != qaStatusPendingQA {
		t.Fatalf("qa_status = %q, want pending_qa", episode.QAStatus)
	}
	if episode.QAScore.Valid {
		t.Fatalf("qa_score valid = %v, want NULL", episode.QAScore.Valid)
	}
	if episode.QualityFlag.Valid {
		t.Fatalf("quality_flag = %q, want NULL", episode.QualityFlag.String)
	}

	var check struct {
		CheckName string  `db:"check_name"`
		Passed    bool    `db:"passed"`
		Score     float64 `db:"score"`
		Details   string  `db:"details"`
	}
	if err := db.Get(&check, "SELECT check_name, passed, score, details FROM qa_checks WHERE episode_id = 1"); err != nil {
		t.Fatalf("query qa_check: %v", err)
	}
	if check.CheckName != "manual_review_cancel" || !check.Passed || check.Score != 0 || check.Details != "取消人工复核不通过" {
		t.Fatalf("qa_check = %+v, want manual review cancel", check)
	}
}

func setupEpisodeQACheckTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	})

	_, err = db.Exec(`
		CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			task_id INTEGER,
			workstation_id INTEGER,
			dc_plan_id INTEGER,
			mcap_path TEXT,
			sidecar_path TEXT,
			qa_status TEXT,
			cloud_synced BOOLEAN NOT NULL DEFAULT FALSE,
			qa_score REAL,
			auto_approved BOOLEAN,
			quality_flag TEXT,
			metadata TEXT,
			deleted_at TIMESTAMP NULL
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT,
			workstation_id INTEGER,
			deleted_at TIMESTAMP NULL
		);
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			deleted_at TIMESTAMP NULL
		);
		CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT,
			deleted_at TIMESTAMP NULL
		);
		CREATE TABLE qa_checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			check_name TEXT NOT NULL,
			passed BOOLEAN NOT NULL,
			score REAL NOT NULL,
			details TEXT,
			check_metadata TEXT,
			checked_at TIMESTAMP
		);
		CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	return db
}
