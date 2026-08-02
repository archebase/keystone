// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

func TestHilbertRawDataClientUsesRESTContract(t *testing.T) {
	var sawRegister bool
	var sawLookup bool
	var sawCredentials bool
	var sawFinish bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case hilbertRawDataRegisterPath:
			sawRegister = true
			if r.Method != http.MethodPost {
				t.Fatalf("register method = %s, want POST", r.Method)
			}
			var body HilbertRawDataRegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode register body: %v", err)
			}
			if body.WorkspaceID != 2 || body.DCPlanID != 8 || body.BagName != "capture.mcap" || body.BagDigest != "9dd4e461268c8034f5c8564e155c67a6" {
				t.Fatalf("unexpected register body: %+v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":42}`))
		case hilbertRawDataQueryPath:
			sawLookup = true
			if r.Method != http.MethodGet {
				t.Fatalf("lookup method = %s, want GET", r.Method)
			}
			if r.URL.Query().Get("workspaceId") != "2" || r.URL.Query().Get("bagName") != "capture.mcap" ||
				r.URL.Query().Get("pageNum") != "1" || r.URL.Query().Get("pageSize") != "200" {
				t.Fatalf("lookup query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"records":[{"id":41,"workspaceId":2,"dcPlanId":8,"bagName":"capture-old.mcap","bagStartTime":"2026-07-15T02:00:00Z","bagEndTime":"2026-07-15T02:00:01Z","bagSize":1,"bagDigest":"9dd4e461268c8034f5c8564e155c67a6"},{"id":42,"workspaceId":2,"dcPlanId":8,"bagName":"capture.mcap","bagStartTime":"2026-07-15T02:00:00Z","bagEndTime":"2026-07-15T02:00:01Z","bagSize":1,"bagDigest":"9dd4e461268c8034f5c8564e155c67a6"}],"total":2,"pageNum":1,"pageSize":200}}`))
		case hilbertRawDataGetUploadCredentialsPath:
			sawCredentials = true
			if r.Method != http.MethodGet {
				t.Fatalf("credentials method = %s, want GET", r.Method)
			}
			if r.URL.Query().Get("workspaceId") != "2" || r.URL.Query().Get("id") != "42" {
				t.Fatalf("credentials query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"provider":"TOS","endpoint":"tos-s3-cn-beijing.ivolces.com","region":"cn-beijing","bucket":"bucket-a","key":"object-a","credentials":{"access_key_id":"ak","secret_access_key":"sk","session_token":"token","expire_time":"2026-07-15T04:30:10Z"}}}`))
		case hilbertRawDataFinishUploadPath:
			sawFinish = true
			if r.Method != http.MethodPost {
				t.Fatalf("finish method = %s, want POST", r.Method)
			}
			var body map[string]int64
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode finish body: %v", err)
			}
			if body["workspaceId"] != 2 || body["rawDataId"] != 42 {
				t.Fatalf("unexpected finish body: %+v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{
		BaseURL:   server.URL,
		AccessKey: "ak",
		SecretKey: "sk",
	})
	rawID, err := client.RegisterRawData(
		t.Context(),
		HilbertRawDataRegisterRequest{
			WorkspaceID:  2,
			DCPlanID:     8,
			BagName:      "capture.mcap",
			BagStartTime: time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC),
			BagEndTime:   time.Date(2026, 7, 15, 2, 0, 1, 0, time.UTC),
			BagSize:      1,
			BagDigest:    "9dd4e461268c8034f5c8564e155c67a6",
		},
	)
	if err != nil {
		t.Fatalf("RegisterRawData() error = %v", err)
	}
	found, err := client.FindRawDataByBagName(t.Context(), 2, "capture.mcap")
	if err != nil {
		t.Fatalf("FindRawDataByBagName() error = %v", err)
	}
	if found == nil || found.ID != rawID || found.BagName != "capture.mcap" {
		t.Fatalf("FindRawDataByBagName() = %+v, want raw data %d", found, rawID)
	}
	credentials, err := client.GetRawDataUploadCredentials(t.Context(), 2, rawID)
	if err != nil {
		t.Fatalf("GetRawDataUploadCredentials() error = %v", err)
	}
	if credentials.Provider != "TOS" || credentials.Bucket != "bucket-a" || credentials.Key != "object-a" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if err := client.FinishRawDataUpload(t.Context(), 2, rawID); err != nil {
		t.Fatalf("FinishRawDataUpload() error = %v", err)
	}
	if !sawRegister || !sawLookup || !sawCredentials || !sawFinish {
		t.Fatalf("saw register=%t lookup=%t credentials=%t finish=%t", sawRegister, sawLookup, sawCredentials, sawFinish)
	}
}

func TestHilbertRawDataClientFindsExactBagNameOnLaterPage(t *testing.T) {
	var pageNumbers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hilbertRawDataQueryPath {
			http.NotFound(w, r)
			return
		}
		pageNum := r.URL.Query().Get("pageNum")
		pageNumbers = append(pageNumbers, pageNum)
		switch pageNum {
		case "1":
			_, _ = w.Write([]byte(`{"code":0,"data":{"records":[{"id":41,"workspaceId":2,"dcPlanId":8,"bagName":"capture.mcap.backup","bagStartTime":"2026-07-15T02:00:00Z","bagEndTime":"2026-07-15T02:00:01Z","bagSize":1,"bagDigest":"9dd4e461268c8034f5c8564e155c67a6"}],"total":201,"pageNum":1,"pageSize":200}}`))
		case "2":
			_, _ = w.Write([]byte(`{"code":0,"data":{"records":[{"id":42,"workspaceId":2,"dcPlanId":8,"bagName":"capture.mcap","bagStartTime":"2026-07-15T02:00:00Z","bagEndTime":"2026-07-15T02:00:01Z","bagSize":1,"bagDigest":"9dd4e461268c8034f5c8564e155c67a6"}],"total":201,"pageNum":2,"pageSize":200}}`))
		default:
			t.Fatalf("unexpected pageNum %q", pageNum)
		}
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{
		BaseURL:   server.URL,
		AccessKey: "ak",
		SecretKey: "sk",
	})
	found, err := client.FindRawDataByBagName(t.Context(), 2, "capture.mcap")
	if err != nil {
		t.Fatalf("FindRawDataByBagName() error = %v", err)
	}
	if found == nil || found.ID != 42 || found.BagName != "capture.mcap" {
		t.Fatalf("FindRawDataByBagName() = %+v, want exact second-page match", found)
	}
	if len(pageNumbers) != 2 || pageNumbers[0] != "1" || pageNumbers[1] != "2" {
		t.Fatalf("lookup pages = %v, want [1 2]", pageNumbers)
	}
}

func TestHilbertRawDataClientHandlesLocalizedErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hilbertRawDataRegisterPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":-1,"message":{"en_US":"already exists","zh_CN":"原始数据已存在"},"data":null}`))
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{
		BaseURL:   server.URL,
		AccessKey: "ak",
		SecretKey: "sk",
	})
	_, err := client.RegisterRawData(t.Context(), HilbertRawDataRegisterRequest{
		WorkspaceID:  2,
		DCPlanID:     8,
		BagName:      "capture.mcap",
		BagStartTime: time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC),
		BagEndTime:   time.Date(2026, 7, 15, 2, 0, 1, 0, time.UTC),
		BagSize:      1,
		BagDigest:    "9dd4e461268c8034f5c8564e155c67a6",
	})
	if err == nil {
		t.Fatal("RegisterRawData() error = nil")
	}
	if !strings.Contains(err.Error(), "原始数据已存在") {
		t.Fatalf("RegisterRawData() error = %v, want localized message", err)
	}
}
