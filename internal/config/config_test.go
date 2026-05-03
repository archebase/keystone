// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// config/config_test.go - Configuration tests
package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	// Save original environment variables
	originalEnv := map[string]string{
		"KEYSTONE_MODE":             os.Getenv("KEYSTONE_MODE"),
		"KEYSTONE_MYSQL_HOST":       os.Getenv("KEYSTONE_MYSQL_HOST"),
		"KEYSTONE_MYSQL_PASSWORD":   os.Getenv("KEYSTONE_MYSQL_PASSWORD"),
		"KEYSTONE_MINIO_ACCESS_KEY": os.Getenv("KEYSTONE_MINIO_ACCESS_KEY"),
		"KEYSTONE_MINIO_SECRET_KEY": os.Getenv("KEYSTONE_MINIO_SECRET_KEY"),
		"KEYSTONE_FACTORY_ID":       os.Getenv("KEYSTONE_FACTORY_ID"),
	}
	defer func() {
		// Restore original environment variables
		for k, v := range originalEnv {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}()

	// Set test environment variables
	os.Setenv("KEYSTONE_MYSQL_PASSWORD", "test-password")
	os.Setenv("KEYSTONE_MINIO_ACCESS_KEY", "test-access-key")
	os.Setenv("KEYSTONE_MINIO_SECRET_KEY", "test-secret-key")
	os.Setenv("KEYSTONE_FACTORY_ID", "factory-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify default values
	if cfg.Server.Mode != "edge" {
		t.Errorf("Load().Server.Mode = %v, want edge", cfg.Server.Mode)
	}

	if cfg.Server.BindAddr != ":8080" {
		t.Errorf("Load().Server.BindAddr = %v, want :8080", cfg.Server.BindAddr)
	}

	// Verify reading from environment variables
	if cfg.Database.DSN == "" {
		t.Error("Load().Database.DSN is empty")
	}

	if cfg.Storage.Bucket != "edge-factory-test" {
		t.Errorf("Load().Storage.Bucket = %v, want edge-factory-test", cfg.Storage.Bucket)
	}

	// Verify QA configuration
	if !cfg.QA.Enabled {
		t.Error("Load().QA.Enabled should default to true")
	}

	if cfg.QA.AutoApproveThreshold != 0.90 {
		t.Errorf("Load().QA.AutoApproveThreshold = %v, want 0.90", cfg.QA.AutoApproveThreshold)
	}

	if cfg.QA.MaxWorkers != 4 {
		t.Errorf("Load().QA.MaxWorkers = %v, want 4", cfg.QA.MaxWorkers)
	}

	// Verify feature flags (edge version should have these disabled)
	if cfg.Features.StrataEnabled {
		t.Error("Load().Features.StrataEnabled should be false")
	}

	if cfg.Features.DagsterEnabled {
		t.Error("Load().Features.DagsterEnabled should be false")
	}
}

func TestLoadWithCustomEnv(t *testing.T) {
	// Save original environment variables
	originalEnv := map[string]string{
		"KEYSTONE_MODE":             os.Getenv("KEYSTONE_MODE"),
		"KEYSTONE_BIND_ADDR":        os.Getenv("KEYSTONE_BIND_ADDR"),
		"KEYSTONE_MYSQL_PASSWORD":   os.Getenv("KEYSTONE_MYSQL_PASSWORD"),
		"KEYSTONE_MINIO_ACCESS_KEY": os.Getenv("KEYSTONE_MINIO_ACCESS_KEY"),
		"KEYSTONE_MINIO_SECRET_KEY": os.Getenv("KEYSTONE_MINIO_SECRET_KEY"),
		"KEYSTONE_QA_MAX_WORKERS":   os.Getenv("KEYSTONE_QA_MAX_WORKERS"),
		"KEYSTONE_MAX_MEMORY_MB":    os.Getenv("KEYSTONE_MAX_MEMORY_MB"),
	}
	defer func() {
		for k, v := range originalEnv {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}()

	// Set custom environment variables
	os.Setenv("KEYSTONE_MODE", "edge")
	os.Setenv("KEYSTONE_BIND_ADDR", ":9090")
	os.Setenv("KEYSTONE_MYSQL_PASSWORD", "custom-password")
	os.Setenv("KEYSTONE_MINIO_ACCESS_KEY", "custom-access")
	os.Setenv("KEYSTONE_MINIO_SECRET_KEY", "custom-secret")
	os.Setenv("KEYSTONE_QA_MAX_WORKERS", "8")
	os.Setenv("KEYSTONE_MAX_MEMORY_MB", "8192")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.BindAddr != ":9090" {
		t.Errorf("Load().Server.BindAddr = %v, want :9090", cfg.Server.BindAddr)
	}

	if cfg.QA.MaxWorkers != 8 {
		t.Errorf("Load().QA.MaxWorkers = %v, want 8", cfg.QA.MaxWorkers)
	}

	if cfg.Resources.MaxMemoryMB != 8192 {
		t.Errorf("Load().Resources.MaxMemoryMB = %v, want 8192", cfg.Resources.MaxMemoryMB)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "Valid configuration",
			cfg: &Config{
				Server: ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{
					DSN: "user:pass@tcp(localhost:3306)/db",
				},
				Storage: StorageConfig{
					AccessKey: "key",
					SecretKey: "secret",
				},
				Auth: AuthConfig{
					JWTSecret: "test-secret",
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid mode",
			cfg: &Config{
				Server: ServerConfig{Mode: "cloud"},
				Database: DatabaseConfig{
					DSN: "user:pass@tcp(localhost:3306)/db",
				},
				Storage: StorageConfig{
					AccessKey: "key",
					SecretKey: "secret",
				},
			},
			wantErr: true,
		},
		{
			name: "Empty DSN",
			cfg: &Config{
				Server: ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{
					DSN: "",
				},
				Storage: StorageConfig{
					AccessKey: "key",
					SecretKey: "secret",
				},
			},
			wantErr: true,
		},
		{
			name: "Empty storage keys",
			cfg: &Config{
				Server: ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{
					DSN: "user:pass@tcp(localhost:3306)/db",
				},
				Storage: StorageConfig{
					AccessKey: "",
					SecretKey: "",
				},
			},
			wantErr: true,
		},
		{
			name: "Missing JWTSecret",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: ""},
			},
			wantErr: true,
		},
		{
			name: "Only admin username set (no password)",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "secret", AdminUsername: "admin", AdminPassword: ""},
			},
			wantErr: true,
		},
		{
			name: "Only admin password set (no username)",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "secret", AdminUsername: "", AdminPassword: "pass"},
			},
			wantErr: true,
		},
		{
			name: "Valid admin credentials",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "secret", AdminUsername: "admin", AdminPassword: "pass"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeAPIKey(t *testing.T) {
	tests := []struct {
		name           string
		apiKey         string
		wantSiteID     int64
		wantAPISecret  string
		wantErr        bool
		wantErrContain string
	}{
		{
			// site_id=42, secret="test-secret-value"
			// Generated: base64url_no_pad( i64_be(42) + "." + "test-secret-value" )
			name:          "valid key",
			apiKey:        "AAAAAAAAACoudGVzdC1zZWNyZXQtdmFsdWU",
			wantSiteID:    42,
			wantAPISecret: "test-secret-value",
			wantErr:       false,
		},
		{
			name:           "empty key",
			apiKey:         "",
			wantErr:        true,
			wantErrContain: "must not be empty",
		},
		{
			name:           "whitespace only",
			apiKey:         "   ",
			wantErr:        true,
			wantErrContain: "must not be empty",
		},
		{
			name:           "invalid base64",
			apiKey:         "!!!notbase64!!!",
			wantErr:        true,
			wantErrContain: "base64 decode failed",
		},
		{
			name:           "too short (no separator)",
			apiKey:         "AAAAAAAA",
			wantErr:        true,
			wantErrContain: "invalid format",
		},
		{
			name:           "wrong separator byte",
			apiKey:         "AAAAAAAAAX9zZWNyZXQ", // 8 zero bytes + 'A' (0x41) instead of '.' (0x2E) + "secret"
			wantErr:        true,
			wantErrContain: "invalid format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			siteID, apiSecret, err := decodeAPIKey(tt.apiKey)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeAPIKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrContain != "" && err != nil {
					if !contains(err.Error(), tt.wantErrContain) {
						t.Errorf("decodeAPIKey() error = %q, want to contain %q", err.Error(), tt.wantErrContain)
					}
				}
				return
			}
			if siteID != tt.wantSiteID {
				t.Errorf("decodeAPIKey() siteID = %d, want %d", siteID, tt.wantSiteID)
			}
			if apiSecret != tt.wantAPISecret {
				t.Errorf("decodeAPIKey() apiSecret = %q, want %q", apiSecret, tt.wantAPISecret)
			}
		})
	}
}

func TestValidateSyncAPIKey(t *testing.T) {
	validBase := Config{
		Server:   ServerConfig{Mode: "edge"},
		Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
		Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
		Auth:     AuthConfig{JWTSecret: "jwt-secret"},
	}

	t.Run("sync disabled — no API key required", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{Enabled: false}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() unexpected error = %v", err)
		}
	})

	t.Run("sync enabled — missing API key", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			AuthEndpoint:      "auth:443",
			GatewayEndpoint:   "gateway:443",
			APIKey:            "",
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: 30,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
		}
		if err := cfg.Validate(); err == nil {
			t.Error("Validate() expected error for missing API key, got nil")
		}
	})

	t.Run("sync enabled — invalid API key", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			AuthEndpoint:      "auth:443",
			GatewayEndpoint:   "gateway:443",
			APIKey:            "notvalidbase64!!!",
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: 30,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
		}
		if err := cfg.Validate(); err == nil {
			t.Error("Validate() expected error for invalid API key, got nil")
		}
	})

	t.Run("sync enabled — valid API key populates SiteID and APISecret", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			AuthEndpoint:      "auth:443",
			GatewayEndpoint:   "gateway:443",
			APIKey:            "AAAAAAAAACoudGVzdC1zZWNyZXQtdmFsdWU", // site_id=42, secret="test-secret-value"
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: 30,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error = %v", err)
		}
		if cfg.Sync.SiteID != 42 {
			t.Errorf("SiteID = %d, want 42", cfg.Sync.SiteID)
		}
		if cfg.Sync.APISecret != "test-secret-value" {
			t.Errorf("APISecret = %q, want %q", cfg.Sync.APISecret, "test-secret-value")
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

func TestGetEnv(t *testing.T) {
	// Test non-existent environment variable
	got := getEnv("NONEXISTENT_ENV_VAR_12345", "default")
	if got != "default" {
		t.Errorf("getEnv() = %v, want default", got)
	}

	// Test existing environment variable
	os.Setenv("TEST_GET_ENV", "test-value")
	got = getEnv("TEST_GET_ENV", "default")
	defer os.Unsetenv("TEST_GET_ENV")
	if got != "test-value" {
		t.Errorf("getEnv() = %v, want test-value", got)
	}
}

func TestGetEnvInt(t *testing.T) {
	// Test non-existent environment variable
	got := getEnvInt("NONEXISTENT_ENV_INT_12345", 42)
	if got != 42 {
		t.Errorf("getEnvInt() = %v, want 42", got)
	}

	// Test existing environment variable
	os.Setenv("TEST_GET_ENV_INT", "100")
	got = getEnvInt("TEST_GET_ENV_INT", 42)
	defer os.Unsetenv("TEST_GET_ENV_INT")
	if got != 100 {
		t.Errorf("getEnvInt() = %v, want 100", got)
	}

	// Test invalid value (should return default)
	os.Setenv("TEST_GET_ENV_INT_INVALID", "not-a-number")
	got = getEnvInt("TEST_GET_ENV_INT_INVALID", 42)
	defer os.Unsetenv("TEST_GET_ENV_INT_INVALID")
	if got != 42 {
		t.Errorf("getEnvInt() = %v, want 42", got)
	}
}

func TestGetEnvFloat(t *testing.T) {
	// Test non-existent environment variable
	got := getEnvFloat("NONEXISTENT_ENV_FLOAT_12345", 3.14)
	if got != 3.14 {
		t.Errorf("getEnvFloat() = %v, want 3.14", got)
	}

	// Test existing environment variable
	os.Setenv("TEST_GET_ENV_FLOAT", "2.71")
	got = getEnvFloat("TEST_GET_ENV_FLOAT", 3.14)
	defer os.Unsetenv("TEST_GET_ENV_FLOAT")
	if got != 2.71 {
		t.Errorf("getEnvFloat() = %v, want 2.71", got)
	}
}

func TestGetEnvBool(t *testing.T) {
	// Test non-existent environment variable
	got := getEnvBool("NONEXISTENT_ENV_BOOL_12345", true)
	if got != true {
		t.Errorf("getEnvBool() = %v, want true", got)
	}

	// Test various truth values
	for _, val := range []string{"1", "true", "TRUE", "t", "T"} {
		os.Setenv("TEST_GET_ENV_BOOL", val)
		got = getEnvBool("TEST_GET_ENV_BOOL", false)
		if got != true {
			t.Errorf("getEnvBool(%s) = %v, want true", val, got)
		}
	}

	// Test false value
	os.Setenv("TEST_GET_ENV_BOOL", "false")
	got = getEnvBool("TEST_GET_ENV_BOOL", true)
	defer os.Unsetenv("TEST_GET_ENV_BOOL")
	if got != false {
		t.Errorf("getEnvBool() = %v, want false", got)
	}

	// Test invalid value (should return default)
	os.Setenv("TEST_GET_ENV_BOOL", "not-a-bool")
	got = getEnvBool("TEST_GET_ENV_BOOL", true)
	defer os.Unsetenv("TEST_GET_ENV_BOOL")
	if got != true {
		t.Errorf("getEnvBool() = %v, want true (default)", got)
	}
}
