// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
)

func TestHilbertNonceEncryptionRoundTrip(t *testing.T) {
	material := base64.StdEncoding.EncodeToString(make([]byte, hilbertNonceLengthBytes))
	cipherText, err := EncryptHilbertNonceValue("device-api-key", material)
	if err != nil {
		t.Fatalf("EncryptHilbertNonceValue() error = %v", err)
	}
	if cipherText == "device-api-key" {
		t.Fatal("cipher text contains plaintext")
	}
	plainText, err := DecryptHilbertNonceValue(cipherText, material)
	if err != nil {
		t.Fatalf("DecryptHilbertNonceValue() error = %v", err)
	}
	if plainText != "device-api-key" {
		t.Fatalf("plain text = %q", plainText)
	}
}

func TestHilbertDCPlanUnmarshalUsesNestedProjectAndTaskNames(t *testing.T) {
	var plan HilbertDCPlan
	if err := json.Unmarshal([]byte(`{
		"id": 1001,
		"workspaceId": 123,
		"name": "Plan A",
		"dcProjectId": 13,
		"dcProject": {"id": 13, "name": "Project A", "description": "Kitchen collection project"},
		"dcTaskId": 14,
		"dcTask": {"id": 14, "name": "Task A", "description": "Fold the blanket twice"},
		"dcDeviceId": 15,
		"dcDevice": {"id": 15, "name": "Device A"}
	}`), &plan); err != nil {
		t.Fatalf("unmarshal dc plan: %v", err)
	}
	if plan.DCProjectName != "Project A" || plan.DCProjectDescription != "Kitchen collection project" || plan.DCTaskName != "Task A" || plan.DCTaskDescription != "Fold the blanket twice" || plan.DCDeviceName != "Device A" {
		t.Fatalf("project/task descriptions/device = %q/%q/%q/%q/%q, want nested values", plan.DCProjectName, plan.DCProjectDescription, plan.DCTaskName, plan.DCTaskDescription, plan.DCDeviceName)
	}
}

func TestHilbertDCPlanUnmarshalKeepsFlatNamesOverNestedNames(t *testing.T) {
	var plan HilbertDCPlan
	if err := json.Unmarshal([]byte(`{
		"id": 1001,
		"workspaceId": 123,
		"name": "Plan A",
		"dcProjectId": 13,
		"dcProjectName": "Flat Project",
		"dcProject": {"id": 13, "name": "Nested Project"},
		"dcTaskId": 14,
		"dcTaskName": "Flat Task",
		"dcTask": {"id": 14, "name": "Nested Task"},
		"dcDeviceId": 15,
		"dcDeviceName": "Flat Device",
		"dcDevice": {"id": 15, "name": "Nested Device"}
	}`), &plan); err != nil {
		t.Fatalf("unmarshal dc plan: %v", err)
	}
	if plan.DCProjectName != "Flat Project" || plan.DCTaskName != "Flat Task" || plan.DCDeviceName != "Flat Device" {
		t.Fatalf("project/task/device names = %q/%q/%q, want flat names", plan.DCProjectName, plan.DCTaskName, plan.DCDeviceName)
	}
}

func TestValidateDCDeviceAPIKeyUsesNonceContract(t *testing.T) {
	material := base64.StdEncoding.EncodeToString(make([]byte, hilbertNonceLengthBytes))
	now := time.Unix(1700000000, 123000000)
	millis := "1700000000123"
	sum := sha256.Sum256([]byte("hilbert-sk," + millis))
	wantAuth := "Digest hilbert-ak;" + millis + ";" + hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case hilbertNoncePath:
			writeHilbertTestJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"id": 77, "randomKey": material}})
		case hilbertDCDeviceValidatePath:
			if got := r.Header.Get("Authorization"); got != wantAuth {
				t.Fatalf("authorization = %q", got)
			}
			var body struct {
				WorkspaceID  int64  `json:"workspaceId"`
				DeviceID     int64  `json:"id"`
				NonceID      int64  `json:"nonceId"`
				CipherAPIKey string `json:"cipherApiKey"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode validate body: %v", err)
			}
			if body.WorkspaceID != 10 || body.DeviceID != 101 || body.NonceID != 77 {
				t.Fatalf("unexpected validate body: %#v", body)
			}
			plainText, err := DecryptHilbertNonceValue(body.CipherAPIKey, material)
			if err != nil || plainText != "device-api-key" {
				t.Fatalf("decrypt validate credential: plain=%q err=%v", plainText, err)
			}
			writeHilbertTestJSON(t, w, map[string]any{"code": 0, "data": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{BaseURL: server.URL, TimeoutSeconds: 2, AccessKey: "hilbert-ak", SecretKey: "hilbert-sk"})
	client.now = func() time.Time { return now }
	valid, err := client.ValidateDCDeviceAPIKey(context.Background(), 10, 101, "device-api-key")
	if err != nil {
		t.Fatalf("ValidateDCDeviceAPIKey() error = %v", err)
	}
	if !valid {
		t.Fatal("ValidateDCDeviceAPIKey() = false")
	}
}

func TestGetCurrentAccountUsesDigestAuth(t *testing.T) {
	now := time.Unix(1700000000, 123000000)
	millis := "1700000000123"
	sum := sha256.Sum256([]byte("hilbert-sk," + millis))
	wantAuth := "Digest hilbert-ak;" + millis + ";" + hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hilbertAccountGetCurPath {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("authorization = %q", got)
		}
		writeHilbertTestJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"id":          1,
				"code":        "shanghai-keystone",
				"displayName": "上海一楼实训场Keystone",
				"role":        "admin",
				"status":      "enabled",
			},
		})
	}))
	defer server.Close()

	client := NewHilbertClient(&config.HilbertConfig{BaseURL: server.URL, TimeoutSeconds: 2, AccessKey: "hilbert-ak", SecretKey: "hilbert-sk"})
	client.now = func() time.Time { return now }
	account, err := client.GetCurrentAccount(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentAccount() error = %v", err)
	}
	if account == nil || account.Code != "shanghai-keystone" {
		t.Fatalf("account=%#v want shanghai-keystone", account)
	}
}

func writeHilbertTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
