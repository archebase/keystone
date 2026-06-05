// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
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

func TestPersistEpisodeQACheckFailureMarksEpisodeFailed(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, deleted_at)
		VALUES (1, 'approved', NULL, NULL)
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
	if err := handler.persistEpisodeQACheckResult(context.Background(), 1, outcome, time.Now().UTC()); err != nil {
		t.Fatalf("persist qa check: %v", err)
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

func TestPersistEpisodeQACheckSuccessDoesNotRestoreFailedEpisode(t *testing.T) {
	db := setupEpisodeQACheckTestDB(t)
	handler := &EpisodeHandler{db: db}

	_, err := db.Exec(`
		INSERT INTO episodes (id, qa_status, quality_flag, deleted_at)
		VALUES (1, 'failed', 'previous failure', NULL)
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
	if err := handler.persistEpisodeQACheckResult(context.Background(), 1, outcome, time.Now().UTC()); err != nil {
		t.Fatalf("persist qa check: %v", err)
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
	if episode.QualityFlag != "previous failure" {
		t.Fatalf("quality_flag = %q, want previous failure", episode.QualityFlag)
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
			qa_status TEXT,
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
