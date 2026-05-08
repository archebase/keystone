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
			if !strings.Contains(expr, "CONVERT_TZ(event_time, @@session.time_zone, ?)") {
				t.Fatalf("bucket expression should convert event_time into local timezone: %s", expr)
			}
			if !strings.Contains(expr, "CONVERT_TZ(") || !strings.Contains(expr, "'+00:00'") {
				t.Fatalf("bucket expression should convert local bucket back to UTC: %s", expr)
			}
			if len(args) != tt.argCount {
				t.Fatalf("arg count = %d, want %d", len(args), tt.argCount)
			}
			for _, arg := range args {
				if arg != "+08:00" {
					t.Fatalf("bucket arg = %v, want +08:00", arg)
				}
			}
		})
	}
}
