// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestParseDataOpsEpisodeQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/data-ops/episodes?limit=20&offset=40&created_at_from=2026-06-01T00:00:00Z&created_at_to=2026-06-06T00:00:00Z&q=ep&qa_status=failed,pending_qa&sync_status=not_started,failed&scene_id=1,2&sop_id=9,10&robot_type_id=3&robot_device_id=robot-001,robot-002&collector_operator_id=op001&label=recalled_batch", nil)

	got, err := parseDataOpsEpisodeQuery(c)
	if err != nil {
		t.Fatalf("parseDataOpsEpisodeQuery returned error: %v", err)
	}
	if got.Pagination.Limit != 20 || got.Pagination.Offset != 40 {
		t.Fatalf("unexpected pagination: %+v", got.Pagination)
	}
	if !got.HasCreatedAtFrom || !got.HasCreatedAtTo || got.Keyword != "ep" || got.Label != "recalled_batch" {
		t.Fatalf("unexpected scalar filters: %+v", got)
	}
	if strings.Join(got.QAStatuses, ",") != "failed,pending_qa" {
		t.Fatalf("unexpected qa statuses: %#v", got.QAStatuses)
	}
	if strings.Join(got.SyncStatuses, ",") != "not_started,failed" {
		t.Fatalf("unexpected sync statuses: %#v", got.SyncStatuses)
	}
	if len(got.SceneIDs) != 2 || got.SceneIDs[0] != 1 || got.SceneIDs[1] != 2 {
		t.Fatalf("unexpected scene ids: %#v", got.SceneIDs)
	}
	if len(got.SOPIDs) != 2 || got.SOPIDs[0] != 9 || got.SOPIDs[1] != 10 {
		t.Fatalf("unexpected sop ids: %#v", got.SOPIDs)
	}
	if len(got.RobotTypeIDs) != 1 || got.RobotTypeIDs[0] != 3 {
		t.Fatalf("unexpected robot type ids: %#v", got.RobotTypeIDs)
	}
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
}

func TestDataOpsEpisodeWhereIncludesSOPFilter(t *testing.T) {
	sql, args := buildDataOpsEpisodeWhere(dataOpsEpisodeQuery{SOPIDs: []int64{9, 10}})
	if !strings.Contains(sql, "COALESCE(e.sop_id, t.sop_id) IN (?,?)") {
		t.Fatalf("SOP filter SQL should use episode/task SOP fallback: %s", sql)
	}
	if len(args) != 2 || args[0] != int64(9) || args[1] != int64(10) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsEpisodeListSQLIncludesSOPColumns(t *testing.T) {
	sql := dataOpsEpisodeListSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{
		"COALESCE(e.sop_id, t.sop_id) AS sop_id",
		"LEFT JOIN sops s ON s.id = COALESCE(e.sop_id, t.sop_id)",
		"CONCAT('SOP #', CAST(COALESCE(e.sop_id, t.sop_id) AS CHAR))",
		"ELSE CONCAT(s.slug, ' @ ', s.version)",
		"COALESCE(NULLIF(dc.name, ''), NULLIF(ws.collector_name, '')) AS collector_name",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("data ops SQL should include %q: %s", want, sql)
		}
	}
}

func TestDataOpsSyncStatusWhereSupportsNotStartedAndLatestStatus(t *testing.T) {
	sql, args := dataOpsSyncStatusWhere([]string{"not_started", "failed"})
	if !strings.Contains(sql, "NOT EXISTS") {
		t.Fatalf("sync status SQL should include not_started branch: %s", sql)
	}
	if !strings.Contains(sql, "MAX(sl2.id)") || !strings.Contains(sql, "sl_latest.status IN (?)") {
		t.Fatalf("sync status SQL should filter latest sync log status: %s", sql)
	}
	if len(args) != 1 || args[0] != "failed" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsLatestQueriesOnlyUsePageEpisodeIDs(t *testing.T) {
	qaSQL, qaArgs := dataOpsLatestQAChecksSQL([]int64{10, 20})
	if !strings.Contains(qaSQL, "WHERE episode_id IN (?,?)") {
		t.Fatalf("latest QA SQL should constrain page episode IDs: %s", qaSQL)
	}
	if len(qaArgs) != 2 {
		t.Fatalf("latest QA args = %#v", qaArgs)
	}

	syncSQL, syncArgs := dataOpsLatestSyncLogsSQL([]int64{10, 20})
	if !strings.Contains(syncSQL, "WHERE episode_id IN (?,?)") {
		t.Fatalf("latest sync SQL should constrain page episode IDs: %s", syncSQL)
	}
	if len(syncArgs) != 2 {
		t.Fatalf("latest sync args = %#v", syncArgs)
	}
}

func TestParseDataOpsBulkEpisodeFilters(t *testing.T) {
	got, err := parseDataOpsBulkEpisodeFilters(DataOpsBulkEpisodeFilters{
		CreatedAtFrom:       "2026-06-01T00:00:00Z",
		CreatedAtTo:         "2026-06-06T00:00:00Z",
		Keyword:             "ep",
		QAStatus:            "failed,pending_qa",
		SyncStatus:          "not_started,failed",
		SceneID:             "1,2",
		SOPID:               "9,10",
		RobotTypeID:         "3",
		RobotDeviceID:       "robot-001,robot-002",
		CollectorOperatorID: "op001",
		Label:               "recalled_batch",
		Limit:               "20",
		Offset:              "40",
	})
	if err != nil {
		t.Fatalf("parseDataOpsBulkEpisodeFilters returned error: %v", err)
	}
	if got.Pagination.Limit != 0 || got.Pagination.Offset != 0 {
		t.Fatalf("bulk filters should ignore pagination: %+v", got.Pagination)
	}
	if !got.HasCreatedAtFrom || !got.HasCreatedAtTo || got.Keyword != "ep" || got.Label != "recalled_batch" {
		t.Fatalf("unexpected scalar filters: %+v", got)
	}
	if strings.Join(got.QAStatuses, ",") != "failed,pending_qa" {
		t.Fatalf("unexpected qa statuses: %#v", got.QAStatuses)
	}
	if strings.Join(got.SyncStatuses, ",") != "not_started,failed" {
		t.Fatalf("unexpected sync statuses: %#v", got.SyncStatuses)
	}
	if len(got.SceneIDs) != 2 || got.SceneIDs[0] != 1 || got.SceneIDs[1] != 2 {
		t.Fatalf("unexpected scene ids: %#v", got.SceneIDs)
	}
	if len(got.SOPIDs) != 2 || got.SOPIDs[0] != 9 || got.SOPIDs[1] != 10 {
		t.Fatalf("unexpected sop ids: %#v", got.SOPIDs)
	}
	if len(got.RobotTypeIDs) != 1 || got.RobotTypeIDs[0] != 3 {
		t.Fatalf("unexpected robot type ids: %#v", got.RobotTypeIDs)
	}
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
}

func TestParseDataOpsBulkEpisodeFiltersDoesNotCapMultiValueCount(t *testing.T) {
	got, err := parseDataOpsBulkEpisodeFilters(DataOpsBulkEpisodeFilters{
		SceneID:       joinedNumberList(maxMultiValueFilterItems + 1),
		RobotDeviceID: joinedStringList("robot-", maxMultiValueFilterItems+1),
	})
	if err != nil {
		t.Fatalf("parseDataOpsBulkEpisodeFilters returned error: %v", err)
	}
	if len(got.SceneIDs) != maxMultiValueFilterItems+1 {
		t.Fatalf("scene id count = %d, want %d", len(got.SceneIDs), maxMultiValueFilterItems+1)
	}
	if len(got.RobotDeviceIDs) != maxMultiValueFilterItems+1 {
		t.Fatalf("robot device id count = %d, want %d", len(got.RobotDeviceIDs), maxMultiValueFilterItems+1)
	}
}

func TestParseDataOpsBulkEpisodeRequestConfirmGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"filters":{}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &DataOpsHandler{}
	if _, _, ok := h.parseBulkEpisodeActionRequest(c, true); ok {
		t.Fatal("bulk execute request without confirm should fail")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestParseDataOpsBulkEpisodeRequestPreviewDoesNotRequireConfirm(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/data-ops/episodes/bulk-qa/preview", bytes.NewBufferString(`{"filters":{"qa_status":"failed"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &DataOpsHandler{}
	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		t.Fatal("bulk preview request should not require confirm")
	}
	if strings.Join(q.QAStatuses, ",") != "failed" {
		t.Fatalf("unexpected qa statuses: %#v", q.QAStatuses)
	}
}

func TestDataOpsEpisodeIDSnapshotSQLUsesDataOpsOrdering(t *testing.T) {
	sql := dataOpsEpisodeIDSnapshotSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{
		"SELECT e.id",
		"FROM episodes e",
		"ORDER BY e.created_at DESC, e.id DESC",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("ID snapshot SQL should include %q: %s", want, sql)
		}
	}
}

func TestDataOpsBulkPreviewSQLs(t *testing.T) {
	qaSQL := dataOpsBulkQAPreviewSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{"matched_count", "qa_running_count", "protected_status_count"} {
		if !strings.Contains(qaSQL, want) {
			t.Fatalf("QA preview SQL should include %q: %s", want, qaSQL)
		}
	}

	syncSQL := dataOpsBulkSyncPreviewSQL(dataOpsEpisodeBaseFromSQL()+dataOpsLatestSyncPreviewJoinSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{"latest_sync", "eligible_count", "qa_not_approved_count", "already_synced_count", "sync_active_count"} {
		if !strings.Contains(syncSQL, want) {
			t.Fatalf("sync preview SQL should include %q: %s", want, syncSQL)
		}
	}
}

func TestPreviewBulkEpisodeSyncTreatsMissingSyncLogAsEligible(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	for id := int64(1); id <= 11; id++ {
		if _, err := db.Exec(`
			INSERT INTO episodes (id, episode_id, task_id, scene_id, qa_status, cloud_synced, deleted_at, created_at)
			VALUES (?, ?, 0, 0, 'approved', 0, NULL, '2026-06-01T00:00:00Z')
		`, id, "episode"); err != nil {
			t.Fatalf("insert episode %d: %v", id, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status)
		VALUES (1, 1, 'failed')
	`); err != nil {
		t.Fatalf("insert failed sync log: %v", err)
	}

	preview, err := h.previewBulkEpisodeSync(context.Background(), dataOpsEpisodeQuery{
		QAStatuses: []string{"approved"},
	})
	if err != nil {
		t.Fatalf("previewBulkEpisodeSync returned error: %v", err)
	}

	if preview.MatchedCount != 11 || preview.EligibleCount != 11 || preview.SkippedCount != 0 {
		t.Fatalf("preview counts = matched %d eligible %d skipped %d, want 11/11/0", preview.MatchedCount, preview.EligibleCount, preview.SkippedCount)
	}
	if len(preview.SkippedBreakdown) != 0 {
		t.Fatalf("unexpected skipped breakdown: %#v", preview.SkippedBreakdown)
	}
}

func setupDataOpsBulkPreviewTestDB(t *testing.T) *sqlx.DB {
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

	schema := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			scene_id INTEGER NOT NULL,
			workstation_id INTEGER,
			sop_id INTEGER,
			qa_status TEXT,
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			deleted_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			sop_id INTEGER,
			workstation_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE scenes (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			data_collector_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE robot_types (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE data_collectors (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE sops (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}
