// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"testing"
	"time"

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

func TestDefaultEpisodeQASuiteIncludesRecordingNotEmpty(t *testing.T) {
	got := defaultEpisodeQASuite(episodeQACheckRow{})
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
			mcap_path TEXT,
			sidecar_path TEXT,
			qa_status TEXT,
			qa_score REAL,
			auto_approved BOOLEAN,
			quality_flag TEXT,
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
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	return db
}
