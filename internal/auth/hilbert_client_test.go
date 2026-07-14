// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestValidateDCDeviceAPIKeyUsesNonceContract(t *testing.T) {
	material := base64.StdEncoding.EncodeToString(make([]byte, hilbertNonceLengthBytes))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case hilbertNoncePath:
			writeHilbertTestJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"id": 77, "randomKey": material}})
		case hilbertDCDeviceValidatePath:
			if got := r.Header.Get("Authorization"); got != "Bearer service-session" {
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

	client := NewHilbertClient(&config.HilbertConfig{BaseURL: server.URL, TimeoutSeconds: 2})
	valid, err := client.ValidateDCDeviceAPIKey(context.Background(), "service-session", 10, 101, "device-api-key")
	if err != nil {
		t.Fatalf("ValidateDCDeviceAPIKey() error = %v", err)
	}
	if !valid {
		t.Fatal("ValidateDCDeviceAPIKey() = false")
	}
}

func writeHilbertTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
