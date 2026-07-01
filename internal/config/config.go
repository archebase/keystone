// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package config provides configuration loading and validation
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config represents the complete configuration for Keystone Edge
type Config struct {
	Server       ServerConfig
	Database     DatabaseConfig
	Storage      StorageConfig
	QA           QAConfig
	Sync         SyncConfig
	Auth         AuthConfig
	Features     FeaturesConfig
	Monitoring   MonitoringConfig
	Resources    ResourceLimitsConfig
	AxonTransfer TransferConfig
	AxonRecorder RecorderConfig
}

// ServerConfig server configuration
type ServerConfig struct {
	Mode                  string
	BindAddr              string
	CallbackPublicBaseURL string
	ReadTimeout           int // seconds
	WriteTimeout          int // seconds
	ShutdownTimeout       int // seconds
}

// DatabaseConfig database configuration
type DatabaseConfig struct {
	Driver          string
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime int // seconds
}

// StorageConfig storage configuration
type StorageConfig struct {
	Type      string // "s3"
	Endpoint  string
	AccessKey string `json:"-"`
	SecretKey string `json:"-"`
	Bucket    string
	UseSSL    bool
}

// QAConfig QA engine configuration
type QAConfig struct {
	Enabled              bool
	AutoApproveThreshold float64
	MaxWorkers           int
	TimeoutPerEpisode    int // seconds
	Checks               []string
}

// SyncConfig synchronization configuration
type SyncConfig struct {
	Enabled         bool
	AutoScanEnabled bool
	BatchSize       int
	MaxRetries      int

	// Cloud upload settings (data-platform integration)
	AuthEndpoint       string // gRPC endpoint for AuthService
	GatewayEndpoint    string // gRPC endpoint for DataGatewayService
	CloudUseTLS        bool   // enable TLS for cloud gRPC connections
	CloudTLSCAFile     string // optional CA bundle path for TLS verification
	CloudTLSServerName string // optional TLS server name override (SNI / verification)
	APIKey             string `json:"-"` // opaque cloud-issued credential; never JSON-marshaled
	MaxConcurrent      int    // max concurrent uploads
	WorkerIntervalSec  int    // sync worker poll interval in seconds
	RequestTimeoutSec  int    // per-RPC timeout in seconds
	OSSTimeoutSec      int    // per-part OSS upload timeout in seconds
	RetryBaseSec       int    // base retry backoff in seconds
	RetryMaxSec        int    // max retry backoff in seconds
	RetryJitterSec     int    // max additive jitter in seconds
	PersistRootDir     string // root directory for persisting upload state across restarts; empty disables persistence
	MaxRestartCount    int    // max number of upload restarts before permanent failure; 0 uses uploader default (3)
	DPConfigPath       string // data-platform config path for direct device-profile uploads
}

// FeaturesConfig feature flags configuration
type FeaturesConfig struct {
	StrataEnabled  bool
	SlateEnabled   bool
	DagsterEnabled bool
	RayEnabled     bool
	LanceDBEnabled bool
}

// MonitoringConfig monitoring configuration
type MonitoringConfig struct {
	Enabled             bool
	MetricsPort         int
	HealthCheckInterval int // seconds
	LogLevel            string
	LogOutput           string
}

// ResourceLimitsConfig resource limits configuration
type ResourceLimitsConfig struct {
	MaxMemoryMB       int
	MaxCPUPercent     int
	DiskWatermarkLow  int
	DiskWatermarkHigh int
}

// TransferConfig Transfer service configuration
type TransferConfig struct {
	WSPort         int
	MaxEvents      int
	ReadTimeout    int // seconds
	WriteTimeout   int // seconds
	PingInterval   int // seconds
	PingTimeout    int // seconds
	StaleThreshold int // seconds
	FactoryID      string
}

// RecorderConfig Axon Recorder RPC gateway configuration
type RecorderConfig struct {
	WSPort          int
	AuthEnabled     bool
	PingInterval    int // seconds
	PingTimeout     int // seconds
	StaleThreshold  int // seconds
	ResponseTimeout int // seconds
}

// AuthConfig JWT authentication configuration (collector login).
type AuthConfig struct {
	JWTSecret             string // #nosec G117 -- signing secret loaded from env; must exist in config struct
	Issuer                string
	JWTExpiryHours        int
	AdminUsername         string // #nosec G101 -- admin account name loaded from env
	AdminPassword         string // #nosec G101 -- admin password loaded from env; never logged
	DashboardDisplayToken string // #nosec G101 -- optional long-lived dashboard display token loaded from env
	HilbertBaseURL        string
	HilbertTimeoutSeconds int
}

// Load loads configuration from environment variables and defaults
func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Mode:                  getEnv("KEYSTONE_MODE", "edge"),
			BindAddr:              getEnv("KEYSTONE_BIND_ADDR", ":8080"),
			CallbackPublicBaseURL: getEnv("KEYSTONE_CALLBACK_PUBLIC_BASE_URL", ""),
			ReadTimeout:           getEnvInt("KEYSTONE_READ_TIMEOUT", 30),
			WriteTimeout:          getEnvInt("KEYSTONE_WRITE_TIMEOUT", 30),
			ShutdownTimeout:       getEnvInt("KEYSTONE_SHUTDOWN_TIMEOUT", 10),
		},
		Database: DatabaseConfig{
			Driver: "mysql",
			DSN: fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=UTC&charset=utf8mb4&multiStatements=true&time_zone=%%27%%2B00%%3A00%%27",
				getEnv("KEYSTONE_MYSQL_USER", "keystone"),
				getEnv("KEYSTONE_MYSQL_PASSWORD", ""),
				getEnv("KEYSTONE_MYSQL_HOST", "localhost"),
				getEnv("KEYSTONE_MYSQL_PORT", "3306"),
				getEnv("KEYSTONE_MYSQL_DATABASE", "keystone")),
			MaxOpenConns:    getEnvInt("KEYSTONE_DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getEnvInt("KEYSTONE_DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: getEnvInt("KEYSTONE_DB_CONN_MAX_LIFETIME", 300),
		},
		Storage: StorageConfig{
			Type:      "s3",
			Endpoint:  getEnv("KEYSTONE_MINIO_ENDPOINT", "http://localhost:9000"),
			AccessKey: getEnv("KEYSTONE_MINIO_ACCESS_KEY", ""),
			SecretKey: getEnv("KEYSTONE_MINIO_SECRET_KEY", ""),
			Bucket:    "edge-" + getEnv("KEYSTONE_FACTORY_ID", "factory-default"),
			UseSSL:    getEnvBool("KEYSTONE_MINIO_USE_SSL", false),
		},
		QA: QAConfig{
			Enabled:              getEnvBool("KEYSTONE_QA_ENABLED", true),
			AutoApproveThreshold: getEnvFloat("KEYSTONE_QA_AUTO_APPROVE_THRESHOLD", 0.90),
			MaxWorkers:           getEnvInt("KEYSTONE_QA_MAX_WORKERS", 4),
			TimeoutPerEpisode:    getEnvInt("KEYSTONE_QA_TIMEOUT", 300),
			Checks:               []string{"topics", "duration", "gaps", "images"},
		},
		Sync: SyncConfig{
			Enabled:            getEnvBool("KEYSTONE_SYNC_ENABLED", true),
			AutoScanEnabled:    getEnvBool("KEYSTONE_SYNC_AUTO_SCAN_ENABLED", false),
			BatchSize:          getEnvInt("KEYSTONE_SYNC_BATCH_SIZE", 10),
			MaxRetries:         getEnvInt("KEYSTONE_SYNC_MAX_RETRIES", 5),
			AuthEndpoint:       getEnv("KEYSTONE_CLOUD_AUTH_ENDPOINT", ""),
			GatewayEndpoint:    getEnv("KEYSTONE_CLOUD_GATEWAY_ENDPOINT", ""),
			CloudUseTLS:        getEnvBool("KEYSTONE_CLOUD_USE_TLS", true),
			CloudTLSCAFile:     getEnv("KEYSTONE_CLOUD_TLS_CA_FILE", ""),
			CloudTLSServerName: getEnv("KEYSTONE_CLOUD_TLS_SERVER_NAME", ""),
			APIKey:             getEnv("KEYSTONE_CLOUD_API_KEY", ""),
			MaxConcurrent:      getEnvInt("KEYSTONE_SYNC_MAX_CONCURRENT", 2),
			WorkerIntervalSec:  getEnvInt("KEYSTONE_SYNC_WORKER_INTERVAL", 60),
			RequestTimeoutSec:  getEnvInt("KEYSTONE_SYNC_REQUEST_TIMEOUT", 30),
			OSSTimeoutSec:      getEnvInt("KEYSTONE_SYNC_OSS_TIMEOUT", 300),
			RetryBaseSec:       getEnvInt("KEYSTONE_SYNC_RETRY_BASE_SEC", 30),
			RetryMaxSec:        getEnvInt("KEYSTONE_SYNC_RETRY_MAX_SEC", 1800),
			RetryJitterSec:     getEnvInt("KEYSTONE_SYNC_RETRY_JITTER_SEC", 30),
			PersistRootDir:     getEnv("KEYSTONE_SYNC_PERSIST_ROOT_DIR", ""),
			MaxRestartCount:    getEnvInt("KEYSTONE_SYNC_MAX_RESTART_COUNT", 3),
			DPConfigPath:       getEnv("KEYSTONE_SYNC_DP_CONFIG", defaultDPConfigPath()),
		},
		Auth: AuthConfig{
			JWTSecret:             getEnv("KEYSTONE_JWT_SECRET", ""),
			Issuer:                getEnv("KEYSTONE_JWT_ISSUER", "keystone-edge"),
			JWTExpiryHours:        getEnvInt("KEYSTONE_JWT_EXPIRY_HOURS", 24),
			AdminUsername:         getEnv("KEYSTONE_ADMIN_USERNAME", ""),
			AdminPassword:         getEnv("KEYSTONE_ADMIN_PASSWORD", ""),
			DashboardDisplayToken: getEnv("KEYSTONE_DASHBOARD_DISPLAY_TOKEN", ""),
			HilbertBaseURL:        getEnv("KEYSTONE_HILBERT_BASE_URL", ""),
			HilbertTimeoutSeconds: getEnvInt("KEYSTONE_HILBERT_TIMEOUT_SECONDS", 5),
		},
		Features: FeaturesConfig{
			StrataEnabled:  false,
			SlateEnabled:   false,
			DagsterEnabled: false,
			RayEnabled:     false,
			LanceDBEnabled: false,
		},
		Monitoring: MonitoringConfig{
			Enabled:             getEnvBool("KEYSTONE_METRICS_ENABLED", true),
			MetricsPort:         getEnvInt("KEYSTONE_METRICS_PORT", 9090),
			HealthCheckInterval: getEnvInt("KEYSTONE_HEALTH_CHECK_INTERVAL", 10),
			LogLevel:            getEnv("KEYSTONE_LOG_LEVEL", "info"),
			LogOutput:           getEnv("KEYSTONE_LOG_OUTPUT", "/var/log/keystone-edge/"),
		},
		Resources: ResourceLimitsConfig{
			MaxMemoryMB:       getEnvInt("KEYSTONE_MAX_MEMORY_MB", 6144),
			MaxCPUPercent:     getEnvInt("KEYSTONE_MAX_CPU_PERCENT", 80),
			DiskWatermarkLow:  getEnvInt("KEYSTONE_DISK_WATERMARK_LOW", 20),
			DiskWatermarkHigh: getEnvInt("KEYSTONE_DISK_WATERMARK_HIGH", 10),
		},
		AxonTransfer: TransferConfig{
			WSPort:         getEnvInt("KEYSTONE_AXON_TRANSFER_WS_PORT", 8090),
			MaxEvents:      getEnvInt("KEYSTONE_AXON_TRANSFER_MAX_EVENTS", 10000),
			ReadTimeout:    getEnvInt("KEYSTONE_AXON_TRANSFER_READ_TIMEOUT", 30),
			WriteTimeout:   getEnvInt("KEYSTONE_AXON_TRANSFER_WRITE_TIMEOUT", 10),
			PingInterval:   getEnvInt("KEYSTONE_AXON_TRANSFER_PING_INTERVAL", 25),
			PingTimeout:    getEnvInt("KEYSTONE_AXON_TRANSFER_PING_TIMEOUT", 10),
			StaleThreshold: getEnvInt("KEYSTONE_AXON_TRANSFER_STALE_THRESHOLD", 60),
			FactoryID:      getEnv("KEYSTONE_FACTORY_ID", "factory-default"),
		},
		AxonRecorder: RecorderConfig{
			WSPort:          getEnvInt("KEYSTONE_AXON_RECORDER_WS_PORT", 8091),
			AuthEnabled:     getEnvBool("KEYSTONE_AXON_RECORDER_AUTH_ENABLED", false),
			PingInterval:    getEnvInt("KEYSTONE_AXON_RECORDER_PING_INTERVAL", 30),
			PingTimeout:     getEnvInt("KEYSTONE_AXON_RECORDER_PING_TIMEOUT", 10),
			StaleThreshold:  getEnvInt("KEYSTONE_AXON_RECORDER_STALE_THRESHOLD", 60),
			ResponseTimeout: getEnvInt("KEYSTONE_AXON_RECORDER_RESPONSE_TIMEOUT", 15),
		},
	}

	return cfg, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Server.Mode != "edge" {
		return fmt.Errorf("invalid mode: %s, must be 'edge'", c.Server.Mode)
	}
	callbackPublicBaseURL, err := normalizeCallbackPublicBaseURL(c.Server.CallbackPublicBaseURL)
	if err != nil {
		return err
	}
	c.Server.CallbackPublicBaseURL = callbackPublicBaseURL
	if c.Database.DSN == "" {
		return fmt.Errorf("database DSN is required")
	}
	if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
		return fmt.Errorf("storage access key and secret key are required")
	}
	if strings.TrimSpace(c.Auth.JWTSecret) == "" {
		return fmt.Errorf("KEYSTONE_JWT_SECRET is required")
	}
	adminUser := strings.TrimSpace(c.Auth.AdminUsername)
	adminPass := strings.TrimSpace(c.Auth.AdminPassword)
	if (adminUser == "") != (adminPass == "") {
		return fmt.Errorf("KEYSTONE_ADMIN_USERNAME and KEYSTONE_ADMIN_PASSWORD must both be set or both be empty")
	}
	if c.Sync.Enabled {
		c.Sync.DPConfigPath = strings.TrimSpace(c.Sync.DPConfigPath)
		if c.Sync.DPConfigPath == "" {
			return fmt.Errorf("KEYSTONE_SYNC_DP_CONFIG is required when sync is enabled")
		}
		expandedDPConfigPath, err := expandHomePath(c.Sync.DPConfigPath)
		if err != nil {
			return fmt.Errorf("KEYSTONE_SYNC_DP_CONFIG %q is invalid: %w", c.Sync.DPConfigPath, err)
		}
		c.Sync.DPConfigPath = expandedDPConfigPath
		c.Sync.AuthEndpoint = strings.TrimSpace(c.Sync.AuthEndpoint)
		c.Sync.GatewayEndpoint = strings.TrimSpace(c.Sync.GatewayEndpoint)
		c.Sync.APIKey = strings.TrimSpace(c.Sync.APIKey)
		if c.Sync.BatchSize <= 0 {
			return fmt.Errorf("sync batch size must be greater than 0 when sync is enabled")
		}
		if c.Sync.MaxRetries <= 0 {
			return fmt.Errorf("sync max retries must be greater than 0 when sync is enabled")
		}
		if c.Sync.MaxConcurrent <= 0 {
			return fmt.Errorf("sync max concurrent must be greater than 0 when sync is enabled")
		}
		if c.Sync.WorkerIntervalSec <= 0 {
			return fmt.Errorf("sync worker interval must be greater than 0 when sync is enabled")
		}
		if c.Sync.RequestTimeoutSec <= 0 {
			return fmt.Errorf("sync request timeout must be greater than 0 when sync is enabled")
		}
		if c.Sync.OSSTimeoutSec <= 0 {
			return fmt.Errorf("sync oss timeout must be greater than 0 when sync is enabled")
		}
		if c.Sync.RetryBaseSec <= 0 {
			return fmt.Errorf("sync retry base seconds must be greater than 0 when sync is enabled")
		}
		if c.Sync.RetryMaxSec <= 0 {
			return fmt.Errorf("sync retry max seconds must be greater than 0 when sync is enabled")
		}
		if c.Sync.RetryJitterSec < 0 {
			return fmt.Errorf("sync retry jitter seconds must be greater than or equal to 0 when sync is enabled")
		}
		if c.Sync.RetryMaxSec < c.Sync.RetryBaseSec {
			return fmt.Errorf("sync retry max seconds must be greater than or equal to retry base seconds when sync is enabled")
		}
		if c.Sync.MaxRestartCount < 0 {
			return fmt.Errorf("sync max restart count must be greater than or equal to 0 when sync is enabled")
		}
	}
	return nil
}

func normalizeCallbackPublicBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL is required")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL %q is invalid: %w", raw, err)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL must be an absolute URL with host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL scheme must be http or https")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL path must be empty")
	}
	if parsed.RawQuery != "" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL query must be empty")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("KEYSTONE_CALLBACK_PUBLIC_BASE_URL fragment must be empty")
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func defaultDPConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "~/.archebase/config.json"
	}
	return filepath.Join(home, ".archebase", "config.json")
}

func expandHomePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("home directory is not available")
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if val := os.Getenv(key); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return fallback
}
