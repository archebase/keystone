// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// config/config_test.go - Configuration tests
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var storageConfigEnvKeys = []string{
	"KEYSTONE_DGW_COMPAT_ENABLED",
	"KEYSTONE_DGW_TOS_ENDPOINT",
	"KEYSTONE_DGW_TOS_BUCKET",
	"KEYSTONE_DGW_TOS_REGION",
	"KEYSTONE_DGW_VOLCENGINE_STS_ROLE_TRN",
	"KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN",
	"KEYSTONE_DGW_VOLCENGINE_STS_ENDPOINT",
	"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID",
	"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET",
	"KEYSTONE_MINIO_ENDPOINT",
	"KEYSTONE_MINIO_USE_SSL",
	"KEYSTONE_MINIO_ACCESS_KEY",
	"KEYSTONE_MINIO_SECRET_KEY",
	"KEYSTONE_MINIO_BUCKET",
	"KEYSTONE_MINIO_REGION",
	"KEYSTONE_FACTORY_ID",
}

func cleanStorageConfigEnv(t *testing.T) {
	t.Helper()

	originalEnv := make(map[string]string, len(storageConfigEnvKeys))
	originalSet := make(map[string]bool, len(storageConfigEnvKeys))
	for _, key := range storageConfigEnvKeys {
		value, ok := os.LookupEnv(key)
		originalEnv[key] = value
		originalSet[key] = ok
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}

	t.Cleanup(func() {
		for _, key := range storageConfigEnvKeys {
			if !originalSet[key] {
				_ = os.Unsetenv(key)
				continue
			}
			_ = os.Setenv(key, originalEnv[key])
		}
	})
}

func setStorageConfigEnv(t *testing.T, key string, value string) {
	t.Helper()
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

func tosStorageEnvDebug() string {
	return "compat=" + os.Getenv("KEYSTONE_DGW_COMPAT_ENABLED") +
		" endpoint=" + os.Getenv("KEYSTONE_DGW_TOS_ENDPOINT") +
		" bucket=" + os.Getenv("KEYSTONE_DGW_TOS_BUCKET")
}

func TestLoad(t *testing.T) {
	// Save original environment variables
	originalEnv := map[string]string{
		"KEYSTONE_MODE":                                os.Getenv("KEYSTONE_MODE"),
		"KEYSTONE_MYSQL_HOST":                          os.Getenv("KEYSTONE_MYSQL_HOST"),
		"KEYSTONE_MYSQL_PASSWORD":                      os.Getenv("KEYSTONE_MYSQL_PASSWORD"),
		"KEYSTONE_MINIO_ACCESS_KEY":                    os.Getenv("KEYSTONE_MINIO_ACCESS_KEY"),
		"KEYSTONE_MINIO_SECRET_KEY":                    os.Getenv("KEYSTONE_MINIO_SECRET_KEY"),
		"KEYSTONE_MINIO_BUCKET":                        os.Getenv("KEYSTONE_MINIO_BUCKET"),
		"KEYSTONE_FACTORY_ID":                          os.Getenv("KEYSTONE_FACTORY_ID"),
		"KEYSTONE_SYNC_DP_CONFIG":                      os.Getenv("KEYSTONE_SYNC_DP_CONFIG"),
		"KEYSTONE_CALLBACK_PUBLIC_BASE_URL":            os.Getenv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL"),
		"KEYSTONE_AXON_RECORDER_AUTH_ENABLED":          os.Getenv("KEYSTONE_AXON_RECORDER_AUTH_ENABLED"),
		"KEYSTONE_HILBERT_BASE_URL":                    os.Getenv("KEYSTONE_HILBERT_BASE_URL"),
		"KEYSTONE_HILBERT_TIMEOUT_SECONDS":             os.Getenv("KEYSTONE_HILBERT_TIMEOUT_SECONDS"),
		"KEYSTONE_HILBERT_ACCESS_KEY":                  os.Getenv("KEYSTONE_HILBERT_ACCESS_KEY"),
		"KEYSTONE_HILBERT_SECRET_KEY":                  os.Getenv("KEYSTONE_HILBERT_SECRET_KEY"),
		"KEYSTONE_LOG_OUTPUT":                          os.Getenv("KEYSTONE_LOG_OUTPUT"),
		"KEYSTONE_DGW_COMPAT_ENABLED":                  os.Getenv("KEYSTONE_DGW_COMPAT_ENABLED"),
		"KEYSTONE_DGW_TOS_ENDPOINT":                    os.Getenv("KEYSTONE_DGW_TOS_ENDPOINT"),
		"KEYSTONE_DGW_TOS_BUCKET":                      os.Getenv("KEYSTONE_DGW_TOS_BUCKET"),
		"KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN": os.Getenv("KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN"),
		"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID":        os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID"),
		"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET":    os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET"),
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
	os.Unsetenv("KEYSTONE_SYNC_DP_CONFIG")
	os.Unsetenv("KEYSTONE_AXON_RECORDER_AUTH_ENABLED")
	os.Unsetenv("KEYSTONE_HILBERT_BASE_URL")
	os.Unsetenv("KEYSTONE_HILBERT_TIMEOUT_SECONDS")
	os.Unsetenv("KEYSTONE_HILBERT_ACCESS_KEY")
	os.Unsetenv("KEYSTONE_HILBERT_SECRET_KEY")
	os.Unsetenv("KEYSTONE_LOG_OUTPUT")
	os.Unsetenv("KEYSTONE_MINIO_BUCKET")
	os.Unsetenv("KEYSTONE_DGW_COMPAT_ENABLED")
	os.Unsetenv("KEYSTONE_DGW_TOS_ENDPOINT")
	os.Unsetenv("KEYSTONE_DGW_TOS_BUCKET")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET")
	os.Setenv("KEYSTONE_MYSQL_PASSWORD", "test-password")
	os.Setenv("KEYSTONE_MINIO_ACCESS_KEY", "test-access-key")
	os.Setenv("KEYSTONE_MINIO_SECRET_KEY", "test-secret-key")
	os.Setenv("KEYSTONE_FACTORY_ID", "factory-test")
	os.Setenv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL", "http://127.0.0.1:9999")

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
	if cfg.Server.CallbackPublicBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("Load().Server.CallbackPublicBaseURL = %q, want http://127.0.0.1:9999", cfg.Server.CallbackPublicBaseURL)
	}

	// Verify reading from environment variables
	if cfg.Database.DSN == "" {
		t.Error("Load().Database.DSN is empty")
	}
	if !strings.Contains(cfg.Database.DSN, "time_zone=%27%2B00%3A00%27") {
		t.Errorf("Load().Database.DSN should set session time_zone to UTC: %s", cfg.Database.DSN)
	}

	if cfg.Storage.Bucket != "edge-factory-test" {
		t.Errorf("Load().Storage.Bucket = %v, want edge-factory-test", cfg.Storage.Bucket)
	}
	if cfg.Storage.Type != "s3" {
		t.Errorf("Load().Storage.Type = %v, want s3", cfg.Storage.Type)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}
	if cfg.Sync.DPConfigPath != filepath.Join(home, ".archebase", "config.json") {
		t.Errorf("Load().Sync.DPConfigPath = %q, want default ~/.archebase/config.json", cfg.Sync.DPConfigPath)
	}
	if cfg.AxonRecorder.AuthEnabled {
		t.Error("Load().AxonRecorder.AuthEnabled should default to false")
	}
	if cfg.Hilbert.BaseURL != "" || cfg.Hilbert.TimeoutSeconds != 5 || cfg.Hilbert.AccessKey != "" || cfg.Hilbert.SecretKey != "" {
		t.Errorf("Load().Hilbert default = %+v, want empty endpoint/AK/SK and timeout 5", cfg.Hilbert)
	}
	if cfg.Monitoring.LogOutput != "stdout" {
		t.Errorf("Load().Monitoring.LogOutput = %q, want stdout", cfg.Monitoring.LogOutput)
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

func TestLoadStereoSplitConfig(t *testing.T) {
	t.Setenv("KEYSTONE_ORBIT_BASE_URL", "http://orbit.archebase-system.svc.cluster.local:8080")
	t.Setenv("KEYSTONE_DERIVATIVE_TOS_PREFIX", "derived/local-test")
	t.Setenv("KEYSTONE_DERIVATIVE_POLL_INTERVAL_SECONDS", "17")
	t.Setenv("KEYSTONE_DGW_COMPAT_ENABLED", "true")
	t.Setenv("KEYSTONE_DGW_TOS_ENDPOINT", "https://tos-cn-beijing.volces.com")
	t.Setenv("KEYSTONE_DGW_TOS_BUCKET", "keystone-managed-bucket")
	t.Setenv("KEYSTONE_DGW_TOS_REGION", "cn-beijing")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Derivatives.Enabled || cfg.Derivatives.OrbitBaseURL != "http://orbit.archebase-system.svc.cluster.local:8080" {
		t.Fatalf("Load().Derivatives = %+v", cfg.Derivatives)
	}
	if cfg.Derivatives.OutputBucket != "keystone-managed-bucket" || cfg.Derivatives.OutputPrefix != "derived/local-test" {
		t.Fatalf("Load().Derivatives output = %+v", cfg.Derivatives)
	}
	if cfg.Derivatives.OrbitTimeoutSec != 30 ||
		cfg.Derivatives.PollIntervalSec != 17 ||
		cfg.Derivatives.Resources.Requests["cpu"] != "2" ||
		cfg.Derivatives.Resources.Limits["memory"] != "8Gi" ||
		cfg.Derivatives.Resources.Limits["ephemeral-storage"] != "100Gi" {
		t.Fatalf("Load().Derivatives fixed defaults = %+v", cfg.Derivatives)
	}
}

func TestLoadStereoSplitDisabledWithoutOrbitURL(t *testing.T) {
	t.Setenv("KEYSTONE_ORBIT_BASE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Derivatives.Enabled {
		t.Fatalf("Load().Derivatives.Enabled = true without KEYSTONE_ORBIT_BASE_URL")
	}
}

func TestLoadCalibrationUsesFixedProcessingConfig(t *testing.T) {
	t.Setenv("KEYSTONE_ORBIT_BASE_URL", "http://orbit.archebase-system.svc.cluster.local:8080")

	cfg := loadCalibrationConfig()
	if cfg.OrbitBaseURL != "http://orbit.archebase-system.svc.cluster.local:8080" {
		t.Fatalf("calibration config = %+v", cfg)
	}
	if cfg.OrbitTimeoutSec != 30 || cfg.ActiveDeadlineSec != 3600 || cfg.TTLSecondsAfterDone != 86400 ||
		cfg.PollIntervalSec != 5 || cfg.MaxResultBytes != 16*1024*1024 || cfg.OrbitLogTailBytes != 1024*1024 ||
		cfg.Resources.Requests["cpu"] != "2" || cfg.Resources.Requests["memory"] != "4Gi" ||
		cfg.Resources.Requests["ephemeral-storage"] != "8Gi" ||
		cfg.Resources.Limits["cpu"] != "4" || cfg.Resources.Limits["memory"] != "8Gi" ||
		cfg.Resources.Limits["ephemeral-storage"] != "16Gi" {
		t.Fatalf("calibration fixed defaults = %+v", cfg)
	}
}

func TestValidateCalibrationDoesNotRequireEnvironmentImage(t *testing.T) {
	cfg := &Config{
		Server:     ServerConfig{Mode: ModeEdge, CallbackPublicBaseURL: "http://127.0.0.1:9999"},
		Database:   DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
		TOSStorage: StorageConfig{Type: "tos", Endpoint: "tos.example", Bucket: "bucket", Region: "region"},
		Auth:       AuthConfig{JWTSecret: "secret"},
		Calibration: CalibrationConfig{
			OrbitBaseURL:    "http://orbit.example:8080",
			OrbitTimeoutSec: 10,
			Resources: KubernetesResourcesConfig{
				Requests: map[string]string{"cpu": "1"},
				Limits:   map[string]string{"cpu": "2"},
			},
			ActiveDeadlineSec:   600,
			TTLSecondsAfterDone: 86400,
			PollIntervalSec:     5,
			MaxResultBytes:      1024,
			OrbitLogTailBytes:   1024,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestLoadWithCustomEnv(t *testing.T) {
	// Save original environment variables
	originalEnv := map[string]string{
		"KEYSTONE_MODE":                                os.Getenv("KEYSTONE_MODE"),
		"KEYSTONE_BIND_ADDR":                           os.Getenv("KEYSTONE_BIND_ADDR"),
		"KEYSTONE_MYSQL_PASSWORD":                      os.Getenv("KEYSTONE_MYSQL_PASSWORD"),
		"KEYSTONE_MINIO_ACCESS_KEY":                    os.Getenv("KEYSTONE_MINIO_ACCESS_KEY"),
		"KEYSTONE_MINIO_SECRET_KEY":                    os.Getenv("KEYSTONE_MINIO_SECRET_KEY"),
		"KEYSTONE_MINIO_BUCKET":                        os.Getenv("KEYSTONE_MINIO_BUCKET"),
		"KEYSTONE_QA_MAX_WORKERS":                      os.Getenv("KEYSTONE_QA_MAX_WORKERS"),
		"KEYSTONE_MAX_MEMORY_MB":                       os.Getenv("KEYSTONE_MAX_MEMORY_MB"),
		"KEYSTONE_DASHBOARD_DISPLAY_TOKEN":             os.Getenv("KEYSTONE_DASHBOARD_DISPLAY_TOKEN"),
		"KEYSTONE_CALLBACK_PUBLIC_BASE_URL":            os.Getenv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL"),
		"KEYSTONE_AXON_RECORDER_AUTH_ENABLED":          os.Getenv("KEYSTONE_AXON_RECORDER_AUTH_ENABLED"),
		"KEYSTONE_HILBERT_BASE_URL":                    os.Getenv("KEYSTONE_HILBERT_BASE_URL"),
		"KEYSTONE_HILBERT_TIMEOUT_SECONDS":             os.Getenv("KEYSTONE_HILBERT_TIMEOUT_SECONDS"),
		"KEYSTONE_HILBERT_ACCESS_KEY":                  os.Getenv("KEYSTONE_HILBERT_ACCESS_KEY"),
		"KEYSTONE_HILBERT_SECRET_KEY":                  os.Getenv("KEYSTONE_HILBERT_SECRET_KEY"),
		"KEYSTONE_LOG_OUTPUT":                          os.Getenv("KEYSTONE_LOG_OUTPUT"),
		"KEYSTONE_DGW_COMPAT_ENABLED":                  os.Getenv("KEYSTONE_DGW_COMPAT_ENABLED"),
		"KEYSTONE_DGW_TOS_ENDPOINT":                    os.Getenv("KEYSTONE_DGW_TOS_ENDPOINT"),
		"KEYSTONE_DGW_TOS_BUCKET":                      os.Getenv("KEYSTONE_DGW_TOS_BUCKET"),
		"KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN": os.Getenv("KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN"),
		"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID":        os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID"),
		"KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET":    os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET"),
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
	os.Setenv("KEYSTONE_MINIO_BUCKET", "custom-bucket")
	os.Setenv("KEYSTONE_QA_MAX_WORKERS", "8")
	os.Setenv("KEYSTONE_MAX_MEMORY_MB", "8192")
	os.Setenv("KEYSTONE_DASHBOARD_DISPLAY_TOKEN", "display-secret")
	os.Setenv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL", "https://keystone.factory.internal")
	os.Setenv("KEYSTONE_AXON_RECORDER_AUTH_ENABLED", "true")
	os.Setenv("KEYSTONE_HILBERT_BASE_URL", "https://hilbert.example.test")
	os.Setenv("KEYSTONE_HILBERT_TIMEOUT_SECONDS", "9")
	os.Setenv("KEYSTONE_HILBERT_ACCESS_KEY", "hilbert-ak")
	os.Setenv("KEYSTONE_HILBERT_SECRET_KEY", "hilbert-sk")
	os.Setenv("KEYSTONE_LOG_OUTPUT", "stderr")
	os.Unsetenv("KEYSTONE_DGW_COMPAT_ENABLED")
	os.Unsetenv("KEYSTONE_DGW_TOS_ENDPOINT")
	os.Unsetenv("KEYSTONE_DGW_TOS_BUCKET")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID")
	os.Unsetenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET")

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
	if cfg.Storage.Bucket != "custom-bucket" {
		t.Errorf("Load().Storage.Bucket = %v, want custom-bucket", cfg.Storage.Bucket)
	}

	if cfg.Resources.MaxMemoryMB != 8192 {
		t.Errorf("Load().Resources.MaxMemoryMB = %v, want 8192", cfg.Resources.MaxMemoryMB)
	}
	if cfg.Monitoring.LogOutput != "stderr" {
		t.Errorf("Load().Monitoring.LogOutput = %q, want stderr", cfg.Monitoring.LogOutput)
	}

	if cfg.Auth.DashboardDisplayToken != "display-secret" {
		t.Errorf("Load().Auth.DashboardDisplayToken = %q, want display-secret", cfg.Auth.DashboardDisplayToken)
	}

	if !cfg.AxonRecorder.AuthEnabled {
		t.Error("Load().AxonRecorder.AuthEnabled = false, want true")
	}
	if cfg.Server.CallbackPublicBaseURL != "https://keystone.factory.internal" {
		t.Errorf("Load().Server.CallbackPublicBaseURL = %q, want https://keystone.factory.internal", cfg.Server.CallbackPublicBaseURL)
	}
	if cfg.Hilbert.BaseURL != "https://hilbert.example.test" ||
		cfg.Hilbert.TimeoutSeconds != 9 ||
		cfg.Hilbert.AccessKey != "hilbert-ak" ||
		cfg.Hilbert.SecretKey != "hilbert-sk" {
		t.Errorf("Load().Hilbert = %+v, want custom Hilbert config", cfg.Hilbert)
	}
}

func TestLoadStorageConfigKeepsAxonOnMinIOWhenDGWCompatEnabled(t *testing.T) {
	cleanStorageConfigEnv(t)
	setStorageConfigEnv(t, "KEYSTONE_DGW_COMPAT_ENABLED", "true")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_ENDPOINT", "https://tos-cn-beijing.volces.com")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_BUCKET", "tos-bucket")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_REGION", "cn-beijing")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STS_ROLE_TRN", "trn:iam::123:role/upload")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN", "trn:iam::123:role/storage")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STS_ENDPOINT", "https://sts.volcengineapi.com")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID", "tos-ak")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET", "tos-sk")
	setStorageConfigEnv(t, "KEYSTONE_MINIO_ENDPOINT", "192.168.119.4:9000")
	setStorageConfigEnv(t, "KEYSTONE_MINIO_ACCESS_KEY", "minio-ak")
	setStorageConfigEnv(t, "KEYSTONE_MINIO_SECRET_KEY", "minio-sk")
	setStorageConfigEnv(t, "KEYSTONE_MINIO_BUCKET", "minio-bucket")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Storage.Type != "s3" {
		t.Fatalf("Storage.Type = %q, want s3", loaded.Storage.Type)
	}
	if loaded.Storage.Endpoint != "192.168.119.4:9000" {
		t.Fatalf("Storage.Endpoint = %q, want 192.168.119.4:9000", loaded.Storage.Endpoint)
	}
	if loaded.Storage.Bucket != "minio-bucket" {
		t.Fatalf("Storage.Bucket = %q, want minio-bucket", loaded.Storage.Bucket)
	}
	if loaded.Storage.AccessKey != "minio-ak" || loaded.Storage.SecretKey != "minio-sk" {
		t.Fatalf("unexpected MinIO credentials selected")
	}
	if loaded.Storage.UseSSL {
		t.Fatalf("Storage.UseSSL = true, want false")
	}
	if !loaded.Storage.EnsureBucket {
		t.Fatalf("Storage.EnsureBucket = false, want true for MinIO")
	}

	tosCfg := loaded.TOSStorage
	if tosCfg.Type != "tos" || tosCfg.Bucket != "tos-bucket" {
		t.Fatalf("TOS storage = type %q bucket %q, want tos/tos-bucket", tosCfg.Type, tosCfg.Bucket)
	}
	if tosCfg.StorageRoleTRN != "trn:iam::123:role/storage" {
		t.Fatalf("TOS StorageRoleTRN = %q, want storage role", tosCfg.StorageRoleTRN)
	}
}

func TestLoadStorageConfigDoesNotFallbackToUploadSTSRoleForTOSQA(t *testing.T) {
	cleanStorageConfigEnv(t)
	setStorageConfigEnv(t, "KEYSTONE_DGW_COMPAT_ENABLED", "true")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_ENDPOINT", "https://tos-cn-beijing.volces.com")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_BUCKET", "tos-bucket")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_REGION", "cn-beijing")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STS_ROLE_TRN", "trn:iam::123:role/upload")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN", "")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID", "tos-ak")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET", "tos-sk")

	cfg := loadTOSStorageConfig()
	if cfg.Type != "tos" {
		t.Fatalf("Type = %q, want tos (%s)", cfg.Type, tosStorageEnvDebug())
	}
	if cfg.StorageRoleTRN != "" {
		t.Fatalf("StorageRoleTRN = %q, want empty when storage role is not configured", cfg.StorageRoleTRN)
	}
}

func TestLoadStorageConfigAllowsTOSWithoutStaticCredentials(t *testing.T) {
	cleanStorageConfigEnv(t)
	setStorageConfigEnv(t, "KEYSTONE_DGW_COMPAT_ENABLED", "true")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_ENDPOINT", "https://tos-cn-beijing.volces.com")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_BUCKET", "tos-bucket")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_REGION", "cn-beijing")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN", "trn:iam::123:role/storage")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID", "")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET", "")

	cfg := loadTOSStorageConfig()
	if cfg.Type != "tos" {
		t.Fatalf("Type = %q, want tos (%s)", cfg.Type, tosStorageEnvDebug())
	}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		t.Fatalf("TOS static credentials = %q/%q, want empty for default SDK credential chain", cfg.AccessKey, cfg.SecretKey)
	}
}

func TestLoadValidateTOSIRSAWithoutMinIOCredentials(t *testing.T) {
	cleanStorageConfigEnv(t)
	setStorageConfigEnv(t, "KEYSTONE_DGW_COMPAT_ENABLED", "true")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_ENDPOINT", "https://tos-cn-beijing.volces.com")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_BUCKET", "tos-bucket")
	setStorageConfigEnv(t, "KEYSTONE_DGW_TOS_REGION", "cn-beijing")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_STORAGE_STS_ROLE_TRN", "trn:iam::123:role/storage")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID", "")
	setStorageConfigEnv(t, "KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET", "")
	t.Setenv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL", "https://keystone.example.test")
	t.Setenv("KEYSTONE_MYSQL_PASSWORD", "mysql-password")
	t.Setenv("KEYSTONE_JWT_SECRET", "jwt-secret")
	t.Setenv("KEYSTONE_SYNC_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Storage.Type != "s3" {
		t.Fatalf("Load().Storage.Type = %q, want s3", cfg.Storage.Type)
	}
	if cfg.Storage.AccessKey != "" || cfg.Storage.SecretKey != "" {
		t.Fatalf("Load().Storage static credentials = %q/%q, want empty", cfg.Storage.AccessKey, cfg.Storage.SecretKey)
	}
	if cfg.TOSStorage.Type != "tos" {
		t.Fatalf("Load().TOSStorage.Type = %q, want tos", cfg.TOSStorage.Type)
	}
	if cfg.TOSStorage.AccessKey != "" || cfg.TOSStorage.SecretKey != "" {
		t.Fatalf("Load().TOSStorage static credentials = %q/%q, want empty", cfg.TOSStorage.AccessKey, cfg.TOSStorage.SecretKey)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
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
				Server: ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
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
			name: "Valid cloud mode",
			cfg: &Config{
				Server: ServerConfig{Mode: "cloud", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database: DatabaseConfig{
					DSN: "user:pass@tcp(localhost:3306)/db",
				},
				Storage: StorageConfig{
					AccessKey: "key",
					SecretKey: "secret",
				},
				Auth: AuthConfig{JWTSecret: "test-secret"},
			},
			wantErr: false,
		},
		{
			name: "Invalid mode",
			cfg: &Config{
				Server:   ServerConfig{Mode: "hybrid", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "test-secret"},
			},
			wantErr: true,
		},
		{
			name: "Empty DSN",
			cfg: &Config{
				Server: ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
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
				Server: ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
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
			name: "TOS storage allows default credential chain",
			cfg: &Config{
				Server:     ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database:   DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				TOSStorage: StorageConfig{Type: "tos", Endpoint: "tos-cn-beijing.volces.com", Bucket: "tos-bucket", Region: "cn-beijing"},
				Auth:       AuthConfig{JWTSecret: "secret"},
			},
			wantErr: false,
		},
		{
			name: "TOS storage rejects partial static credentials",
			cfg: &Config{
				Server:     ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database:   DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:    StorageConfig{AccessKey: "minio-ak", SecretKey: "minio-sk"},
				TOSStorage: StorageConfig{Type: "tos", Endpoint: "tos-cn-beijing.volces.com", Bucket: "tos-bucket", Region: "cn-beijing", AccessKey: "ak"},
				Auth:       AuthConfig{JWTSecret: "secret"},
			},
			wantErr: true,
		},
		{
			name: "MinIO storage rejects partial credentials even with TOS",
			cfg: &Config{
				Server:     ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database:   DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:    StorageConfig{AccessKey: "minio-ak"},
				TOSStorage: StorageConfig{Type: "tos", Endpoint: "tos-cn-beijing.volces.com", Bucket: "tos-bucket", Region: "cn-beijing"},
				Auth:       AuthConfig{JWTSecret: "secret"},
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
				Server:   ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "secret", AdminUsername: "admin", AdminPassword: ""},
			},
			wantErr: true,
		},
		{
			name: "Only admin password set (no username)",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
				Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
				Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
				Auth:     AuthConfig{JWTSecret: "secret", AdminUsername: "", AdminPassword: "pass"},
			},
			wantErr: true,
		},
		{
			name: "Valid admin credentials",
			cfg: &Config{
				Server:   ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
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

func TestValidateCallbackPublicBaseURL(t *testing.T) {
	validBase := Config{
		Server:   ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
		Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
		Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
		Auth:     AuthConfig{JWTSecret: "jwt-secret"},
		Hilbert:  HilbertConfig{BaseURL: "https://hilbert.example", AccessKey: "ak", SecretKey: "sk"},
	}

	t.Run("required", func(t *testing.T) {
		cfg := validBase
		cfg.Server.CallbackPublicBaseURL = ""
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "KEYSTONE_CALLBACK_PUBLIC_BASE_URL") {
			t.Fatalf("Validate() error = %v, want callback public base URL error", err)
		}
	})

	for _, raw := range []string{
		"192.168.1.20:9999",
		"ftp://192.168.1.20:9999",
		"http:///api",
		"http://gateway.local/keystone",
		"http://gateway.local?x=1",
		"http://gateway.local#abc",
	} {
		t.Run("rejects "+raw, func(t *testing.T) {
			cfg := validBase
			cfg.Server.CallbackPublicBaseURL = raw
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "KEYSTONE_CALLBACK_PUBLIC_BASE_URL") {
				t.Fatalf("Validate() error = %v, want callback public base URL error", err)
			}
		})
	}

	t.Run("normalizes trailing slash", func(t *testing.T) {
		cfg := validBase
		cfg.Server.CallbackPublicBaseURL = "https://keystone.factory.internal/"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error = %v", err)
		}
		if cfg.Server.CallbackPublicBaseURL != "https://keystone.factory.internal" {
			t.Fatalf("CallbackPublicBaseURL = %q, want normalized URL", cfg.Server.CallbackPublicBaseURL)
		}
	})
}

func TestValidateSyncDPConfig(t *testing.T) {
	validBase := Config{
		Server:   ServerConfig{Mode: "edge", CallbackPublicBaseURL: "http://127.0.0.1:9999"},
		Database: DatabaseConfig{DSN: "user:pass@tcp(localhost:3306)/db"},
		Storage:  StorageConfig{AccessKey: "key", SecretKey: "secret"},
		Auth:     AuthConfig{JWTSecret: "jwt-secret"},
		Hilbert:  HilbertConfig{BaseURL: "https://hilbert.example", AccessKey: "ak", SecretKey: "sk"},
	}

	t.Run("sync disabled — no DP config required", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{Enabled: false}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() unexpected error = %v", err)
		}
	})

	t.Run("sync enabled — missing Hilbert config", func(t *testing.T) {
		cfg := validBase
		cfg.Hilbert = HilbertConfig{}
		cfg.Sync = SyncConfig{
			Enabled:           true,
			DPConfigPath:      "",
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: 30,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "KEYSTONE_HILBERT_BASE_URL") {
			t.Fatalf("Validate() error = %v, want Hilbert config error", err)
		}
	})

	t.Run("sync enabled — DP config and old cloud endpoint/API key are not required", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
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
		if cfg.Sync.AuthEndpoint != "" || cfg.Sync.GatewayEndpoint != "" || cfg.Sync.APIKey != "" {
			t.Fatalf("legacy cloud config should remain optional and empty: %+v", cfg.Sync)
		}
	})

	t.Run("sync enabled — direct upload request timeout is not validated", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: -1,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error = %v", err)
		}
	})

	t.Run("sync enabled — direct upload restart count is not validated", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			BatchSize:         10,
			MaxRetries:        5,
			MaxConcurrent:     2,
			WorkerIntervalSec: 60,
			RequestTimeoutSec: 30,
			OSSTimeoutSec:     300,
			RetryBaseSec:      30,
			RetryMaxSec:       1800,
			MaxRestartCount:   -1,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error = %v", err)
		}
	})

	t.Run("sync enabled — trims DP config whitespace", func(t *testing.T) {
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			DPConfigPath:      "  /etc/keystone/dp-config.json  ",
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
		if cfg.Sync.DPConfigPath != "/etc/keystone/dp-config.json" {
			t.Errorf("DPConfigPath = %q, want trimmed path", cfg.Sync.DPConfigPath)
		}
	})

	t.Run("sync enabled — expands DP config home path", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		cfg := validBase
		cfg.Sync = SyncConfig{
			Enabled:           true,
			DPConfigPath:      "~/.archebase/config.json",
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
		if cfg.Sync.DPConfigPath != filepath.Join(home, ".archebase", "config.json") {
			t.Errorf("DPConfigPath = %q, want expanded home path", cfg.Sync.DPConfigPath)
		}
	})
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
