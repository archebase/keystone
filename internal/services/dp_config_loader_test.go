// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDPConfigFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dp-config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write DP config fixture: %v", err)
	}
	return path
}

func validDPConfigJSON(extra string) string {
	version := `"version":3,`
	if extra == "missing-version" {
		version = ""
	}
	return `{
		` + version + `
		"endpoints": {
			"auth": "https://auth.example.com",
			"gateway": "gateway.example.com:7443"
		},
		"devices": [{
			"deviceId": " asset-1 ",
			"apiKey": " api-key-1 ",
			"tags": {"line": "A", "empty_value": ""}
		}]
	}`
}

func TestLoadDPDeviceUploadConfig_SelectsDeviceAndEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "version 3", body: validDPConfigJSON("")},
		{name: "missing version", body: validDPConfigJSON("missing-version")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadDPDeviceUploadConfig(writeDPConfigFixture(t, tt.body), "asset-1")
			if err != nil {
				t.Fatalf("loadDPDeviceUploadConfig() error = %v", err)
			}
			if cfg.Profile.DeviceID != "asset-1" {
				t.Fatalf("Profile.DeviceID=%q want asset-1", cfg.Profile.DeviceID)
			}
			if cfg.Profile.APIKey != "api-key-1" {
				t.Fatalf("Profile.APIKey was not trimmed")
			}
			if cfg.Auth.Target != "auth.example.com:443" || !cfg.Auth.UseTLS || cfg.Auth.ServerName != "auth.example.com" {
				t.Fatalf("auth endpoint=%+v", cfg.Auth)
			}
			if cfg.Gateway.Target != "gateway.example.com:7443" || cfg.Gateway.UseTLS {
				t.Fatalf("gateway endpoint=%+v", cfg.Gateway)
			}
			if cfg.Profile.Tags["empty_value"] != "" {
				t.Fatalf("empty tag values must be preserved: %+v", cfg.Profile.Tags)
			}
		})
	}
}

func TestParseDPResolvedEndpoint(t *testing.T) {
	tests := []struct {
		raw        string
		target     string
		useTLS     bool
		serverName string
	}{
		{raw: "https://dp.example.com", target: "dp.example.com:443", useTLS: true, serverName: "dp.example.com"},
		{raw: "https://dp.example.com:9443", target: "dp.example.com:9443", useTLS: true, serverName: "dp.example.com"},
		{raw: "http://dp.example.com", target: "dp.example.com:80", useTLS: false},
		{raw: "dp.example.com:7443", target: "dp.example.com:7443", useTLS: false},
		{raw: "dp.example.com", target: "dp.example.com", useTLS: false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := parseDPResolvedEndpoint(tt.raw)
			if err != nil {
				t.Fatalf("parseDPResolvedEndpoint() error = %v", err)
			}
			if got.Target != tt.target || got.UseTLS != tt.useTLS || got.ServerName != tt.serverName {
				t.Fatalf("parseDPResolvedEndpoint()=%+v want target=%q tls=%t server=%q", got, tt.target, tt.useTLS, tt.serverName)
			}
		})
	}
}

func TestParseDPResolvedEndpointRejectsUnsupportedForms(t *testing.T) {
	for _, raw := range []string{
		"",
		"https://dp.example.com/path",
		"https://dp.example.com?x=1",
		"https://dp.example.com#frag",
		"ftp://dp.example.com",
		"dp.example.com/path",
		"dp.example.com?x=1",
		"dp.example.com#frag",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseDPResolvedEndpoint(raw); err == nil {
				t.Fatalf("parseDPResolvedEndpoint(%q) expected error", raw)
			}
		})
	}
}

func TestLoadDPDeviceUploadConfigRejectsContractErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		deviceID string
		want     string
	}{
		{
			name: "unsupported version",
			body: `{"version":2,"endpoints":{"auth":"auth:1","gateway":"gateway:2"},"devices":[{"deviceId":"asset-1","apiKey":"key","tags":{"k":"v"}}]}`,
			want: "unsupported version",
		},
		{
			name:     "missing device",
			body:     validDPConfigJSON(""),
			deviceID: "CLOUD-device-1",
			want:     "no device profile",
		},
		{
			name: "empty api key",
			body: `{"version":3,"endpoints":{"auth":"auth:1","gateway":"gateway:2"},"devices":[{"deviceId":"asset-1","apiKey":"  ","tags":{"k":"v"}}]}`,
			want: "apiKey is empty",
		},
		{
			name: "empty tags",
			body: `{"version":3,"endpoints":{"auth":"auth:1","gateway":"gateway:2"},"devices":[{"deviceId":"asset-1","apiKey":"key","tags":{}}]}`,
			want: "tags must be non-empty",
		},
		{
			name: "empty tag key",
			body: `{"version":3,"endpoints":{"auth":"auth:1","gateway":"gateway:2"},"devices":[{"deviceId":"asset-1","apiKey":"key","tags":{"":"v"}}]}`,
			want: "empty tag key",
		},
		{
			name: "duplicate device",
			body: `{"version":3,"endpoints":{"auth":"auth:1","gateway":"gateway:2"},"devices":[{"deviceId":" asset-1 ","apiKey":"key","tags":{"k":"v"}},{"deviceId":"asset-1","apiKey":"key2","tags":{"k":"v"}}]}`,
			want: "duplicate deviceId",
		},
		{
			name: "missing endpoint",
			body: `{"version":3,"endpoints":{"auth":"","gateway":"gateway:2"},"devices":[{"deviceId":"asset-1","apiKey":"key","tags":{"k":"v"}}]}`,
			want: "endpoints.auth",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deviceID := tt.deviceID
			if deviceID == "" {
				deviceID = "asset-1"
			}
			_, err := loadDPDeviceUploadConfig(writeDPConfigFixture(t, tt.body), deviceID)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want contains %q", err, tt.want)
			}
		})
	}
}
