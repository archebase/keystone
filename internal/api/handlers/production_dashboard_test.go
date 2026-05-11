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

	req := httptest.NewRequest(http.MethodGet, "/dashboard/snapshot?workstation_id=12&factory_id=34&organization_id=56&trend_days=99&distribution_limit=999&active_limit=999&timezone_offset=%2B08:00", nil)
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
	if got.TimezoneOffset != "+08:00" {
		t.Fatalf("timezone offset = %q, want +08:00", got.TimezoneOffset)
	}
}

func TestParseProductionDashboardQueryValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []string{
		"/dashboard/snapshot?workstation_id=0",
		"/dashboard/snapshot?trend_days=0",
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

func TestProductionDashboardRoutesCanRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	group := router.Group("/production/dashboard")
	handler := &ProductionDashboardHandler{}

	handler.RegisterRoutes(group)
}
