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

func TestParseProductionDashboardQueryDefaultsAndBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/snapshot?workstation_id=12&factory_id=34&organization_id=56&trend_days=99&distribution_limit=999&active_limit=999&recent_limit=999&preview_limit=999&timezone_offset=%2B08:00", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got, err := parseProductionDashboardQuery(c)
	if err != nil {
		t.Fatalf("parseProductionDashboardQuery returned error: %v", err)
	}
	if got.WorkstationID != "12" || got.FactoryID != "34" || got.OrganizationID != "56" {
		t.Fatalf("unexpected scope filters: %+v", got)
	}
	if got.TrendDays != maxDashboardTrendDays {
		t.Fatalf("trend days = %d, want %d", got.TrendDays, maxDashboardTrendDays)
	}
	if got.DistributionLimit != maxDashboardDistributionLimit {
		t.Fatalf("distribution limit = %d, want %d", got.DistributionLimit, maxDashboardDistributionLimit)
	}
	if got.ActiveLimit != maxDashboardActiveLimit {
		t.Fatalf("active limit = %d, want %d", got.ActiveLimit, maxDashboardActiveLimit)
	}
	if got.RecentLimit != maxDashboardRecentLimit {
		t.Fatalf("recent limit = %d, want %d", got.RecentLimit, maxDashboardRecentLimit)
	}
	if got.PreviewLimit != maxDashboardPreviewLimit {
		t.Fatalf("preview limit = %d, want %d", got.PreviewLimit, maxDashboardPreviewLimit)
	}
	if got.TimezoneOffset != "+08:00" {
		t.Fatalf("timezone offset = %q, want +08:00", got.TimezoneOffset)
	}
}

func TestParseProductionDashboardQueryValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []string{
		"/dashboard/snapshot?workstation_id=0",
		"/dashboard/snapshot?trend_days=0",
		"/dashboard/snapshot?recent_limit=0",
		"/dashboard/snapshot?preview_limit=0",
		"/dashboard/snapshot?timezone_offset=08:00",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, target, nil)
			if _, err := parseProductionDashboardQuery(c); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestDashboardSOPLabelSQL(t *testing.T) {
	got := dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version")
	for _, want := range []string{
		"WHEN t.sop_id IS NULL THEN '未分类'",
		"CONCAT('SOP #', CAST(t.sop_id AS CHAR))",
		"ELSE CONCAT(s.slug, '@', s.version)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("SOP label SQL should contain %q: %s", want, got)
		}
	}
}

func TestDashboardTaskScopeUsesCollectorAssignment(t *testing.T) {
	conditions, args := appendDashboardTaskScope(
		[]string{"t.deleted_at IS NULL"},
		nil,
		productionDashboardScope{
			Role:           "data_collector",
			WorkstationID:  "99",
			FactoryID:      "88",
			OrganizationID: "77",
			collectorID:    42,
		},
	)

	joined := strings.Join(conditions, " AND ")
	if !strings.Contains(joined, "ws_scope.data_collector_id = ?") {
		t.Fatalf("collector task scope should filter by collector assignment: %s", joined)
	}
	if strings.Contains(joined, "CAST(t.workstation_id AS CHAR)") || strings.Contains(joined, "CAST(t.factory_id AS CHAR)") {
		t.Fatalf("collector task scope should ignore caller-supplied admin filters: %s", joined)
	}
	if len(args) != 1 || args[0] != int64(42) {
		t.Fatalf("args = %#v, want collector id 42", args)
	}
}

func TestDashboardActiveBatchesQueryLimitsBeforeTaskAggregation(t *testing.T) {
	query, args := buildDashboardActiveBatchesQuery(
		productionDashboardScope{
			Role:        "data_collector",
			collectorID: 42,
		},
		20,
	)
	compact := strings.Join(strings.Fields(query), " ")

	for _, want := range []string{
		"WITH active_batches AS",
		"WHERE b.deleted_at IS NULL AND b.status = 'active' AND ws.data_collector_id = ?",
		"ORDER BY COALESCE(b.started_at, b.updated_at, b.created_at) DESC, b.id DESC LIMIT ?",
		"FROM active_batches ab LEFT JOIN tasks t ON t.batch_id = ab.id AND t.deleted_at IS NULL GROUP BY ab.id",
	} {
		if !strings.Contains(compact, want) {
			t.Fatalf("active batch SQL should contain %q: %s", want, compact)
		}
	}
	if strings.Contains(compact, "FROM tasks WHERE deleted_at IS NULL GROUP BY batch_id") {
		t.Fatalf("active batch SQL should not globally aggregate tasks first: %s", compact)
	}
	if len(args) != 2 || args[0] != int64(42) || args[1] != 20 {
		t.Fatalf("args = %#v, want collector id then limit", args)
	}
}

func TestProductionDashboardOverviewEmptyScopeKeepsContract(t *testing.T) {
	scope := productionDashboardScope{
		Role:    "data_collector",
		Warning: "workstation not assigned",
		empty:   true,
	}

	got := emptyProductionDashboardOverview(scope)
	if got.Scope.Warning != "workstation not assigned" {
		t.Fatalf("scope warning = %q", got.Scope.Warning)
	}
	if got.Summary.TotalTasks != 0 || got.Summary.TodayTasks != 0 {
		t.Fatalf("summary should stay zero: %+v", got.Summary)
	}
	if len(got.Trend) != 0 ||
		len(got.TaskStatusDistribution) != 0 ||
		len(got.RecentTasks) != 0 ||
		len(got.Previews) != 0 {
		t.Fatalf("empty overview arrays should be empty: %+v", got)
	}
	if got.Quality.RecentFailures == nil {
		t.Fatalf("quality recent_failures should be an empty array, not nil")
	}
}

func TestProductionDashboardOverviewDistributionAndDevices(t *testing.T) {
	distribution := buildTaskStatusDistribution(dashboardTaskCounts{
		Completed:  3,
		InProgress: 2,
		Pending:    1,
	})
	if len(distribution) != 3 {
		t.Fatalf("distribution len = %d, want 3: %+v", len(distribution), distribution)
	}

	devices := buildOverviewDevices([]dashboardDeviceConnectionRow{
		{DeviceID: "robot-a"},
		{DeviceID: "robot-b"},
		{DeviceID: "robot-c"},
	}, func(deviceID string) bool {
		return deviceID == "robot-a" || deviceID == "robot-b"
	})
	if devices.Summary.Total != 3 || devices.Summary.Online != 2 || devices.Summary.Offline != 1 {
		t.Fatalf("unexpected device summary: %+v", devices.Summary)
	}
	if devices.Summary.OnlineRate != 66.7 {
		t.Fatalf("online rate = %.1f, want 66.7", devices.Summary.OnlineRate)
	}

	stations := buildOverviewStations([]dashboardStationItem{
		{ID: "1", Name: "A", Status: "active"},
		{ID: "2", Name: "B", Status: "inactive"},
		{ID: "3", Name: "C", Status: "break"},
		{ID: "4", Name: "D", Status: "offline"},
	})
	if stations.Summary.Total != 4 || stations.Summary.Active != 1 ||
		stations.Summary.Inactive != 1 || stations.Summary.Break != 1 ||
		stations.Summary.Online != 3 || stations.Summary.Offline != 1 {
		t.Fatalf("unexpected station summary: %+v", stations.Summary)
	}
	if stations.Summary.OnlineRate != 75 {
		t.Fatalf("station online rate = %.1f, want 75", stations.Summary.OnlineRate)
	}
}

func TestDashboardRecentTaskPrioritySQL(t *testing.T) {
	expr := dashboardRecentTaskPrioritySQL("t.status")
	for _, want := range []string{
		"WHEN t.status = 'in_progress' THEN 1",
		"WHEN t.status IN ('failed', 'cancelled') THEN 2",
		"WHEN t.status = 'completed' THEN 3",
		"WHEN t.status = 'ready' THEN 4",
		"WHEN t.status = 'pending' THEN 5",
		"ELSE 6",
	} {
		if !strings.Contains(expr, want) {
			t.Fatalf("priority SQL should contain %q: %s", want, expr)
		}
	}
}

func TestProductionDashboardEpisodePreviewURL(t *testing.T) {
	got := dashboardEpisodePreviewURL("42", "mcap/episode-42.mcap")
	if got != "/api/v1/episodes/42/presign?kind=mcap" {
		t.Fatalf("preview url = %q", got)
	}

	if got := dashboardEpisodePreviewURL("42", ""); got != "" {
		t.Fatalf("preview url without mcap path = %q, want empty", got)
	}
}

func TestProductionDashboardRoutesCanRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	group := router.Group("/production/dashboard")
	handler := &ProductionDashboardHandler{}

	handler.RegisterRoutes(group)
}
