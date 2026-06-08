// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
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
