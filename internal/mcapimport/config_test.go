// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package mcapimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigDirectDevice(t *testing.T) {
	t.Setenv("KEYSTONE_IMPORT_DEVICE_API_KEY", "device-secret")
	filePath := writeTestMCAP(t, "mcap payload")
	cfg, err := ParseConfig([]string{
		"--endpoint", "keystone.example.com:50053",
		"--file", filePath,
		"--workspace-id", "3",
		"--dc-plan-id", "41",
		"--task-id", "task-1",
		"--device-id", "17",
	})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if !filepath.IsAbs(cfg.FilePath) {
		t.Fatalf("FilePath = %q, want absolute path", cfg.FilePath)
	}
	if cfg.CaptureID == "" {
		t.Fatal("CaptureID is empty")
	}
	if cfg.Parallel != 4 {
		t.Fatalf("Parallel = %d, want 4", cfg.Parallel)
	}
	if !cfg.UseTLS {
		t.Fatal("UseTLS = false, want true")
	}
}

func TestParseConfigRejectsSecretOnCommandLine(t *testing.T) {
	filePath := writeTestMCAP(t, "mcap payload")
	_, err := ParseConfig([]string{
		"--endpoint", "keystone.example.com:50053",
		"--file", filePath,
		"--workspace-id", "3",
		"--dc-plan-id", "41",
		"--task-id", "task-1",
		"--device-id", "17",
		"--device-api-key", "device-secret",
	})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("ParseConfig() error = %v, want rejected secret flag", err)
	}
}

func TestParseConfigReadsSecretFromEnvironment(t *testing.T) {
	t.Setenv("KEYSTONE_IMPORT_DEVICE_ID", "17")
	t.Setenv("KEYSTONE_IMPORT_DEVICE_API_KEY", "device-secret")
	filePath := writeTestMCAP(t, "mcap payload")
	_, err := ParseConfig([]string{
		"--endpoint", "keystone.example.com:50053",
		"--file", filePath,
		"--workspace-id", "3",
		"--dc-plan-id", "41",
		"--task-id", "task-1",
	})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
}

func TestParseConfigLoadsCredentialFile(t *testing.T) {
	credentialsPath := filepath.Join(t.TempDir(), "device.json")
	if err := saveDeviceCredential(credentialsPath, DeviceCredential{DeviceID: "17", Secret: "device-secret"}, 3); err != nil {
		t.Fatalf("saveDeviceCredential() error = %v", err)
	}
	filePath := writeTestMCAP(t, "mcap payload")
	cfg, err := ParseConfig([]string{
		"--endpoint", "keystone.example.com:50053",
		"--file", filePath,
		"--workspace-id", "3",
		"--dc-plan-id", "41",
		"--task-id", "task-1",
		"--device-credentials-file", credentialsPath,
	})
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.DeviceID != "17" || cfg.DeviceCredential != "device-secret" {
		t.Fatalf("loaded credentials = device %q secret %q", cfg.DeviceID, cfg.DeviceCredential)
	}
}

func TestConfigValidateRejectsInvalidAuthentication(t *testing.T) {
	base := validTestConfig(t)
	tests := []struct {
		name      string
		configure func(*Config)
		want      string
	}{
		{
			name: "no authentication",
			configure: func(cfg *Config) {
				cfg.DeviceID = ""
				cfg.DeviceCredential = ""
			},
			want: "device authentication is required",
		},
		{
			name: "partial direct authentication",
			configure: func(cfg *Config) {
				cfg.DeviceCredential = ""
			},
			want: "must be provided together",
		},
		{
			name: "both authentication modes",
			configure: func(cfg *Config) {
				cfg.DeviceName = "robot"
				cfg.DeviceAuthToken = "one-time-token"
			},
			want: "not both",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.configure(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestConfigValidateRejectsEmptyFile(t *testing.T) {
	cfg := validTestConfig(t)
	emptyPath := filepath.Join(t.TempDir(), "empty.mcap")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg.FilePath = emptyPath
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("Validate() error = %v, want empty-file error", err)
	}
}

func validTestConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Endpoint:         "keystone.example.com:50053",
		UseTLS:           true,
		RPCTimeout:       defaultRPCTimeout,
		FilePath:         writeTestMCAP(t, "mcap payload"),
		WorkspaceID:      3,
		DCPlanID:         41,
		TaskID:           "task-1",
		CaptureID:        "capture-1",
		Parallel:         4,
		DeviceID:         "17",
		DeviceCredential: "device-secret",
	}
}

func writeTestMCAP(t *testing.T, contents string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "capture.mcap")
	if err := os.WriteFile(filePath, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return filePath
}
