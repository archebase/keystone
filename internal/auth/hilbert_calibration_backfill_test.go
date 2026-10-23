// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"archebase.com/keystone-edge/internal/config"
)

func TestHilbertRawDataClientQueriesAndBindsCalibration(t *testing.T) {
	calls := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case hilbertRawDataQueryPath:
			calls = append(calls, "query")
			if r.URL.Query().Get("workspaceId") != "2" || r.URL.Query().Get("id") != "42" ||
				r.URL.Query().Get("pageNum") != "1" || r.URL.Query().Get("pageSize") != "1" {
				t.Fatalf("raw-data query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"records":[{"id":42,"workspaceId":2,"dcPlanId":8,"bagName":"capture.mcap","bagStartTime":"2026-07-15T02:00:00Z","bagEndTime":"2026-07-15T02:00:01Z","bagSize":1,"bagDigest":"9dd4e461268c8034f5c8564e155c67a6","paramFileMotionStoreId":null}],"total":1,"pageNum":1,"pageSize":1}}`))
		case hilbertRawDataUpdateParamFilePath:
			calls = append(calls, "bind")
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode binding request: %v", err)
			}
			if body["workspaceId"] != float64(2) || body["rawDataId"] != float64(42) || body["paramFileMotionStoreId"] != "workspaces/2/calibrationSnapshots/cs_01" {
				t.Fatalf("binding request = %+v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{BaseURL: server.URL, AccessKey: "ak", SecretKey: "sk"})
	rawData, err := client.GetRawDataByID(t.Context(), 2, 42)
	if err != nil {
		t.Fatalf("GetRawDataByID() error = %v", err)
	}
	if rawData == nil || rawData.ID != 42 || rawData.ParamFileMotionStoreID != nil {
		t.Fatalf("GetRawDataByID() = %+v", rawData)
	}
	if err := client.UpdateRawDataParamFile(t.Context(), 2, 42, "workspaces/2/calibrationSnapshots/cs_01"); err != nil {
		t.Fatalf("UpdateRawDataParamFile() error = %v", err)
	}
	if len(calls) != 2 || calls[0] != "query" || calls[1] != "bind" {
		t.Fatalf("calls = %v", calls)
	}
}
