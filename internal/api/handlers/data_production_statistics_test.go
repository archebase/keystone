// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseDataProductionStatsQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := httptest.NewRequest(http.MethodGet, "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&granularity=hour&source_id=12&data_type=episode&status=success&task_id=task_1", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got, err := parseDataProductionStatsQuery(c, true)
	if err != nil {
		t.Fatalf("parseDataProductionStatsQuery returned error: %v", err)
	}
	if got.Granularity != "hour" || got.SourceID != "12" || got.DataType != "episode" || got.Status != "success" || got.TaskID != "task_1" {
		t.Fatalf("unexpected query: %+v", got)
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
			name: "bad status",
			path: "/stats?start_time=2026-05-01T00:00:00Z&end_time=2026-05-02T00:00:00Z&status=done",
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
