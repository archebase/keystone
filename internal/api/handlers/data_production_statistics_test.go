// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestParseDataProductionStatsQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequest(http.MethodGet, "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&granularity=hour&source_id=12&scene_id=34,35&robot_type_id=7,8&sop_id=9,10&qa_status=approved,rejected&cloud_synced=false&data_type=episode&task_id=task_1", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got, err := parseDataProductionStatsQuery(c, true)
	if err != nil {
		t.Fatalf("parseDataProductionStatsQuery returned error: %v", err)
	}
	if got.Granularity != "hour" || got.SourceID != "12" || got.DataType != "episode" || got.TaskID != "task_1" {
		t.Fatalf("unexpected query: %+v", got)
	}
	if len(got.SceneIDs) != 2 || got.SceneIDs[0] != 34 || got.SceneIDs[1] != 35 {
		t.Fatalf("unexpected scene ids: %#v", got.SceneIDs)
	}
	if strings.Join(got.RobotTypeIDs, ",") != "7,8" || strings.Join(got.SOPIDs, ",") != "9,10" || strings.Join(got.QAStatuses, ",") != "approved,rejected" {
		t.Fatalf("unexpected list filters: %+v", got)
	}
	if len(got.CloudSyncedValues) != 1 || got.CloudSyncedValues[0] {
		t.Fatalf("unexpected cloud synced values: %#v", got.CloudSyncedValues)
	}
}

func TestProductionRecordsSQLUsesEpisodesOnly(t *testing.T) {
	sql := productionRecordsSQL()
	if !strings.Contains(sql, "FROM episodes e") {
		t.Fatalf("production records SQL should be based on episodes: %s", sql)
	}
	if strings.Contains(sql, "UNION ALL") {
		t.Fatalf("production records SQL should not include task-only unions: %s", sql)
	}
	if strings.Contains(sql, "t.status IN ('failed', 'cancelled')") || strings.Contains(sql, "t.status IN ('ready', 'in_progress')") {
		t.Fatalf("production records SQL should not include task status fallback records: %s", sql)
	}
	for _, want := range []string{"e.scene_id AS scene_id", "COALESCE(e.qa_status, '') AS qa_status", "e.cloud_synced AS cloud_synced"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("production records SQL should include %q: %s", want, sql)
		}
	}
	for _, want := range []string{
		"ws.id = COALESCE(e.workstation_id, t.workstation_id)",
		"COALESCE(CAST(e.sop_id AS CHAR), CAST(t.sop_id AS CHAR), '') AS sop_id",
		"s.id = COALESCE(e.sop_id, t.sop_id)",
		"CONCAT('SOP #', CAST(COALESCE(e.sop_id, t.sop_id) AS CHAR))",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("production records SQL should prefer episode SOP with task fallback %q: %s", want, sql)
		}
	}
}

func TestDataProductionDetailsSQLSelectsSOPFields(t *testing.T) {
	querySQL := dataProductionDetailsSQL("SELECT 1", "time", "DESC")
	for _, want := range []string{
		"COALESCE(CONVERT_TZ(event_time, @@session.time_zone, '+00:00'), event_time)",
		"collector_name",
		"sop_id",
		"sop,",
		"qa_status",
		"cloud_synced",
	} {
		if !strings.Contains(querySQL, want) {
			t.Fatalf("detail SQL should select %q: %s", want, querySQL)
		}
	}
}

func TestAggregateRowToSummaryIncludesQAAndCloudSync(t *testing.T) {
	resp := aggregateRowToSummary(aggregateStatsRow{
		TotalCount:      sql.NullInt64{Int64: 10, Valid: true},
		SuccessCount:    sql.NullInt64{Int64: 10, Valid: true},
		ApprovedQACount: sql.NullInt64{Int64: 7, Valid: true},
		CloudSynced:     sql.NullInt64{Int64: 8, Valid: true},
		CloudUnsynced:   sql.NullInt64{Int64: 2, Valid: true},
	}, 0)

	if resp.QA.Approved != 7 || resp.QA.PassRate != 0.7 {
		t.Fatalf("unexpected QA summary: %+v", resp.QA)
	}
	if resp.Cloud.Synced != 8 || resp.Cloud.Unsynced != 2 || resp.Cloud.SyncRate != 0.8 {
		t.Fatalf("unexpected cloud sync summary: %+v", resp.Cloud)
	}
}

func TestFilteredProductionRecordsSQLIncludesEpisodeFilters(t *testing.T) {
	cloudSynced := true
	handler := &DataProductionStatisticsHandler{}
	query, args := handler.filteredProductionRecordsSQL(dataProductionStatsQuery{
		StartTime:         mustParseStatsTimeForTest(t, "2026-05-01T00:00:00Z"),
		EndTime:           mustParseStatsTimeForTest(t, "2026-05-02T00:00:00Z"),
		SceneIDs:          []int64{34},
		QAStatuses:        []string{"rejected"},
		CloudSyncedValues: []bool{cloudSynced},
	})
	for _, want := range []string{"scene_id IN (?)", "qa_status IN (?)", "cloud_synced = ?"} {
		if !strings.Contains(query, want) {
			t.Fatalf("filtered SQL should include %q: %s", want, query)
		}
	}
	if len(args) != 5 {
		t.Fatalf("arg count = %d, want 5: %#v", len(args), args)
	}
}

func TestDataProductionBreakdownSQLGroupsByDimensionExpression(t *testing.T) {
	countSQL := dataProductionBreakdownCountSQL("robot_device_id", "SELECT 1")
	if !strings.Contains(countSQL, "GROUP BY robot_device_id") {
		t.Fatalf("breakdown count SQL should group by the dimension expression: %s", countSQL)
	}
	if strings.Contains(countSQL, "GROUP BY id") {
		t.Fatalf("breakdown count SQL should not group by ambiguous id alias: %s", countSQL)
	}

	querySQL := dataProductionBreakdownSQL("robot_device_id", "robot_device_id", "SELECT 1")
	if !strings.Contains(querySQL, "GROUP BY robot_device_id") {
		t.Fatalf("breakdown SQL should group by the dimension expression: %s", querySQL)
	}
	if strings.Contains(querySQL, "GROUP BY id") {
		t.Fatalf("breakdown SQL should not group by ambiguous id alias: %s", querySQL)
	}
}

func TestStatsBreakdownExpressionsSupportsEpisodeDimensions(t *testing.T) {
	tests := []struct {
		dimension string
		wantID    string
		wantName  string
	}{
		{dimension: "scene", wantID: "scene_id", wantName: "scene_name"},
		{dimension: "sop", wantID: "sop_id", wantName: "sop"},
		{dimension: "qa_status", wantID: "qa_status", wantName: "WHEN 'pending_qa' THEN '待质检'"},
		{dimension: "cloud_synced", wantID: "CASE WHEN cloud_synced THEN 'true' ELSE 'false' END", wantName: "CASE WHEN cloud_synced THEN '已同步' ELSE '未同步' END"},
	}

	for _, tt := range tests {
		t.Run(tt.dimension, func(t *testing.T) {
			idExpr, nameExpr, err := statsBreakdownExpressions(tt.dimension)
			if err != nil {
				t.Fatalf("statsBreakdownExpressions(%q) returned error: %v", tt.dimension, err)
			}
			if idExpr != tt.wantID {
				t.Fatalf("idExpr = %q, want %q", idExpr, tt.wantID)
			}
			if !strings.Contains(nameExpr, tt.wantName) {
				t.Fatalf("nameExpr = %q, should contain %q", nameExpr, tt.wantName)
			}
		})
	}
}

func TestParseDataProductionStatsQueryRejectsInvalidInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		path string
	}{
		{
			name: "missing start",
			path: "/stats?end_time=2026-05-02T00:00:00Z",
		},
		{
			name: "end before start",
			path: "/stats?start_time=2026-05-02T00:00:00Z&end_time=2026-05-01T00:00:00Z",
		},
		{
			name: "bad granularity",
			path: "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&granularity=minute",
		},
		{
			name: "bad scene",
			path: "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&scene_id=bad",
		},
		{
			name: "bad qa status",
			path: "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&qa_status=done",
		},
		{
			name: "bad cloud sync",
			path: "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&cloud_synced=maybe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, tt.path, nil)

			if _, err := parseDataProductionStatsQuery(c, true); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestParseDataProductionStatsQueryRejectsOversizedListFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	target := "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&robot_device_id=" +
		joinedStringList("robot", maxMultiValueFilterItems+1)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)

	if _, err := parseDataProductionStatsQuery(c, true); err == nil {
		t.Fatalf("expected oversized list validation error")
	}
}

func mustParseStatsTimeForTest(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := parseStatsTime(raw)
	if err != nil {
		t.Fatalf("parseStatsTime(%q): %v", raw, err)
	}
	return value
}

func TestParseStatsDetailSort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/stats?sort_by=size_bytes&sort_order=asc", nil)

	sortBy, sortOrder, err := parseStatsDetailSort(c)
	if err != nil {
		t.Fatalf("parseStatsDetailSort returned error: %v", err)
	}
	if sortBy != "size_bytes" || sortOrder != "ASC" {
		t.Fatalf("unexpected sort: %s %s", sortBy, sortOrder)
	}
}

func TestParseStatsDetailSortRejectsInvalidField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/stats?sort_by=deleted_at", nil)

	if _, _, err := parseStatsDetailSort(c); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestParseStatsTimezoneOffset(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "default UTC", raw: "", want: "+00:00"},
		{name: "positive offset", raw: "+08:00", want: "+08:00"},
		{name: "negative offset", raw: "-05:30", want: "-05:30"},
		{name: "bad format", raw: "08:00", wantErr: true},
		{name: "out of range", raw: "+15:00", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatsTimezoneOffset(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatsBucketExpressionUsesLocalTimezoneOffset(t *testing.T) {
	tests := []struct {
		granularity string
		argCount    int
	}{
		{granularity: "hour", argCount: 2},
		{granularity: "day", argCount: 2},
		{granularity: "week", argCount: 3},
		{granularity: "month", argCount: 2},
	}

	for _, tt := range tests {
		t.Run(tt.granularity, func(t *testing.T) {
			expr, args := statsBucketExpression(tt.granularity, "+08:00")
			if !strings.Contains(expr, "COALESCE(CONVERT_TZ(event_time, @@session.time_zone, ?), event_time)") {
				t.Fatalf("bucket expression should convert event_time into local timezone with fallback: %s", expr)
			}
			if !strings.Contains(expr, "TIMESTAMPADD(MINUTE, ?") || !strings.Contains(expr, "'%Y-%m-%dT%H:%i:%sZ'") {
				t.Fatalf("bucket expression should shift local bucket back to UTC: %s", expr)
			}
			if len(args) != tt.argCount {
				t.Fatalf("arg count = %d, want %d", len(args), tt.argCount)
			}
			if args[0] != -480 {
				t.Fatalf("bucket UTC shift arg = %v, want -480", args[0])
			}
			for _, arg := range args[1:] {
				if arg != "+08:00" {
					t.Fatalf("bucket arg = %v, want +08:00", arg)
				}
			}
		})
	}
}
