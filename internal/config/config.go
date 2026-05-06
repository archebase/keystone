// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package config provides configuration loading and validation
package config

import (
	"encoding/base64"
	"fmt"
	"os"
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
	Mode            string
	BindAddr        string
	ReadTimeout     int // seconds
	WriteTimeout    int // seconds
	ShutdownTimeout int // seconds
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
	Enabled    bool
	BatchSize  int
	MaxRetries int

	// Cloud upload settings (data-platform integration)
	AuthEndpoint       string // gRPC endpoint for AuthService
	GatewayEndpoint    string // gRPC endpoint for DataGatewayService
	CloudUseTLS        bool   // enable TLS for cloud gRPC connections
	CloudTLSCAFile     string // optional CA bundle path for TLS verification
	CloudTLSServerName string // optional TLS server name override (SNI / verification)
	APIKey             string `json:"-"` // base64url-encoded API key (decoded into SiteID + APISecret at load time; never JSON-marshaled)
	SiteID             int64  // site identifier decoded from APIKey
	APISecret          string `json:"-"` // site secret decoded from APIKey (never JSON-marshaled)
	MaxConcurrent      int    // max concurrent uploads
	WorkerIntervalSec  int    // sync worker poll interval in seconds
	RequestTimeoutSec  int    // per-RPC timeout in seconds
	OSSTimeoutSec      int    // per-part OSS upload timeout in seconds
	RetryBaseSec       int    // base retry backoff in seconds
	RetryMaxSec        int    // max retry backoff in seconds
	RetryJitterSec     int    // max additive jitter in seconds
	PersistRootDir     string // root directory for persisting upload state across restarts; empty disables persistence
	MaxRestartCount    int    // max number of upload restarts before permanent failure; 0 uses uploader default (3)
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
	WSPort      int
	MaxEvents   int
	ReadTimeout int // seconds
	FactoryID   string
}

// RecorderConfig Axon Recorder RPC gateway configuration
type RecorderConfig struct {
	WSPort          int
	PingInterval    int // seconds
	ResponseTimeout int // seconds
}

// AuthConfig JWT authentication configuration (collector login).
type AuthConfig struct {
	JWTSecret      string // #nosec G117 -- signing secret loaded from env; must exist in config struct
	Issuer         string
	JWTExpiryHours int
	AdminUsername  string // #nosec G101 -- admin account name loaded from env
	AdminPassword  string // #nosec G101 -- admin password loaded from env; never logged
}

// Load loads configuration from environment variables and defaults
func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Mode:            getEnv("KEYSTONE_MODE", "edge"),
			BindAddr:        getEnv("KEYSTONE_BIND_ADDR", ":8080"),
			ReadTimeout:     getEnvInt("KEYSTONE_READ_TIMEOUT", 30),
			WriteTimeout:    getEnvInt("KEYSTONE_WRITE_TIMEOUT", 30),
			ShutdownTimeout: getEnvInt("KEYSTONE_SHUTDOWN_TIMEOUT", 10),
		},
		Database: DatabaseConfig{
			Driver: "mysql",
			DSN: fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=UTC&charset=utf8mb4&multiStatements=true",
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
		},
		Auth: AuthConfig{
			JWTSecret:      getEnv("KEYSTONE_JWT_SECRET", ""),
			Issuer:         getEnv("KEYSTONE_JWT_ISSUER", "keystone-edge"),
			JWTExpiryHours: getEnvInt("KEYSTONE_JWT_EXPIRY_HOURS", 24),
			AdminUsername:  getEnv("KEYSTONE_ADMIN_USERNAME", ""),
			AdminPassword:  getEnv("KEYSTONE_ADMIN_PASSWORD", ""),
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
			WSPort:      getEnvInt("KEYSTONE_AXON_TRANSFER_WS_PORT", 8090),
			MaxEvents:   getEnvInt("KEYSTONE_AXON_TRANSFER_MAX_EVENTS", 10000),
			ReadTimeout: getEnvInt("KEYSTONE_AXON_TRANSFER_READ_TIMEOUT", 30),
			FactoryID:   getEnv("KEYSTONE_FACTORY_ID", "factory-default"),
		},
		AxonRecorder: RecorderConfig{
			WSPort:          getEnvInt("KEYSTONE_AXON_RECORDER_WS_PORT", 8091),
			PingInterval:    getEnvInt("KEYSTONE_AXON_RECORDER_PING_INTERVAL", 30),
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
		if strings.TrimSpace(c.Sync.AuthEndpoint) == "" {
			return fmt.Errorf("sync auth endpoint is required when sync is enabled")
		}
		if strings.TrimSpace(c.Sync.GatewayEndpoint) == "" {
			return fmt.Errorf("sync gateway endpoint is required when sync is enabled")
		}
		if strings.TrimSpace(c.Sync.APIKey) == "" {
			return fmt.Errorf("KEYSTONE_CLOUD_API_KEY is required when sync is enabled")
		}
		siteID, apiSecret, err := decodeAPIKey(c.Sync.APIKey)
		if err != nil {
			return fmt.Errorf("KEYSTONE_CLOUD_API_KEY is invalid: %w", err)
		}
		c.Sync.SiteID = siteID
		c.Sync.APISecret = apiSecret
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

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
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

// decodeAPIKey decodes a base64url-no-pad API key into its component parts.
//
// The wire format (produced by the data-platform credential issuer) is:
//
//	base64url_no_pad( i64_big_endian(site_id) + "." + site_secret_utf8 )
//
// Returns the site_id (signed int64) and site_secret string, or an error if
// the key is malformed.
func decodeAPIKey(apiKey string) (siteID int64, apiSecret string, err error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return 0, "", fmt.Errorf("api key must not be empty")
	}

	// Restore standard base64 padding (URL-safe, no-pad variant).
	padLen := (-len(apiKey)) % 4
	if padLen < 0 {
		padLen += 4
	}
	padded := apiKey + strings.Repeat("=", padLen)

	decoded, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return 0, "", fmt.Errorf("base64 decode failed: %w", err)
	}

	// Minimum: 8 bytes site_id + 1 byte '.' + at least 1 byte secret.
	if len(decoded) <= 9 || decoded[8] != '.' {
		return 0, "", fmt.Errorf("invalid format: expected i64_be + '.' + secret")
	}

	// First 8 bytes: signed int64 big-endian.
	siteID = int64(decoded[0])<<56 | int64(decoded[1])<<48 | int64(decoded[2])<<40 |
		int64(decoded[3])<<32 | int64(decoded[4])<<24 | int64(decoded[5])<<16 |
		int64(decoded[6])<<8 | int64(decoded[7])
	if siteID <= 0 {
		return 0, "", fmt.Errorf("site_id decoded from api key must be greater than 0, got %d", siteID)
	}

	// Remaining bytes after the '.' separator: UTF-8 secret.
	secretBytes := decoded[9:]
	apiSecret = string(secretBytes)
	if strings.TrimSpace(apiSecret) == "" {
		return 0, "", fmt.Errorf("site_secret decoded from api key must not be empty")
	}

	return siteID, apiSecret, nil
}
