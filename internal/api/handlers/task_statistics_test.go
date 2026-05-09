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

func TestParseTaskBreakdownQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequest(http.MethodGet, "/tasks/statistics/breakdown?dimension=sop&workstation_id=12&status=completed&start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got, err := parseTaskBreakdownQuery(c)
	if err != nil {
		t.Fatalf("parseTaskBreakdownQuery returned error: %v", err)
	}
	if got.Dimension != "sop" || got.WorkstationID != "12" || got.Status != "completed" || got.StartTime == nil || got.EndTime == nil {
		t.Fatalf("unexpected query: %+v", got)
	}
}

func TestTaskBreakdownExpressionsSupportsDashboardDimensions(t *testing.T) {
	tests := []struct {
		dimension string
		wantID    string
		wantName  string
	}{
		{dimension: "scene", wantID: "tasks.scene_id", wantName: "场景 #"},
		{dimension: "sop", wantID: "tasks.sop_id", wantName: "SOP #"},
	}

	for _, tt := range tests {
		t.Run(tt.dimension, func(t *testing.T) {
			idExpr, nameExpr, err := taskBreakdownExpressions(tt.dimension)
			if err != nil {
				t.Fatalf("taskBreakdownExpressions(%q) returned error: %v", tt.dimension, err)
			}
			if !strings.Contains(idExpr, tt.wantID) {
				t.Fatalf("idExpr = %q, should contain %q", idExpr, tt.wantID)
			}
			if !strings.Contains(nameExpr, tt.wantName) {
				t.Fatalf("nameExpr = %q, should contain %q", nameExpr, tt.wantName)
			}
		})
	}
}

func TestTaskBreakdownSQLGroupsByDimensionExpression(t *testing.T) {
	baseSQL := taskBreakdownBaseSQL("tasks.deleted_at IS NULL")
	countSQL := taskBreakdownCountSQL("tasks.scene_id", baseSQL)
	querySQL := taskBreakdownSQL("tasks.scene_id", "tasks.scene_name", baseSQL)

	for _, sql := range []string{countSQL, querySQL} {
		if !strings.Contains(sql, "GROUP BY tasks.scene_id") {
			t.Fatalf("breakdown SQL should group by dimension expression: %s", sql)
		}
	}
	if !strings.Contains(querySQL, "ORDER BY task_count DESC") {
		t.Fatalf("breakdown SQL should order by task count: %s", querySQL)
	}
}

func TestTaskStatisticsRouteCanCoexistWithTaskDetailRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	group := router.Group("")
	stats := group.Group("/tasks/statistics")
	handler := &TaskHandler{}

	stats.GET("/breakdown", handler.GetTaskBreakdown)
	handler.RegisterRoutes(group)
}
