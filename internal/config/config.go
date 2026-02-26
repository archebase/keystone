// config/config.go - Configuration loading and validation
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config represents the complete configuration for Keystone Edge
type Config struct {
	Server     ServerConfig
	Database   DatabaseConfig
	Storage    StorageConfig
	QA         QAConfig
	Sync       SyncConfig
	Features   FeaturesConfig
	Monitoring MonitoringConfig
	Resources  ResourceLimitsConfig
}

// ServerConfig server configuration
type ServerConfig struct {
	Mode           string
	BindAddr       string
	ReadTimeout    int // seconds
	WriteTimeout   int // seconds
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
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// QAConfig QA engine configuration
type QAConfig struct {
	Enabled               bool
	AutoApproveThreshold  float64
	MaxWorkers            int
	TimeoutPerEpisode     int // seconds
	Checks                []string
}

// SyncConfig synchronization configuration
type SyncConfig struct {
	Enabled         bool
	Endpoint        string
	BatchSize       int
	MaxBytes        int64
	MaxRetries      int
	CheckpointPath  string
}

// FeaturesConfig feature flags configuration
type FeaturesConfig struct {
	StrataEnabled   bool
	SlateEnabled    bool
	DagsterEnabled  bool
	RayEnabled      bool
	LanceDBEnabled  bool
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
	MaxMemoryMB      int
	MaxCPUPercent    int
	DiskWatermarkLow  int
	DiskWatermarkHigh int
}

// Load loads configuration from environment variables and defaults
func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Mode:           getEnv("KEYSTONE_MODE", "edge"),
			BindAddr:       getEnv("KEYSTONE_BIND_ADDR", ":8080"),
			ReadTimeout:    getEnvInt("KEYSTONE_READ_TIMEOUT", 30),
			WriteTimeout:   getEnvInt("KEYSTONE_WRITE_TIMEOUT", 30),
			ShutdownTimeout: getEnvInt("KEYSTONE_SHUTDOWN_TIMEOUT", 10),
		},
		Database: DatabaseConfig{
			Driver:          "mysql",
			DSN:             fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=Local&charset=utf8mb4",
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
			Enabled:        getEnvBool("KEYSTONE_SYNC_ENABLED", true),
			Endpoint:       getEnv("KEYSTONE_CLOUD_ENDPOINT", ""),
			BatchSize:      getEnvInt("KEYSTONE_SYNC_BATCH_SIZE", 100),
			MaxBytes:       int64(getEnvInt("KEYSTONE_SYNC_MAX_BYTES", 10737418240)), // 10GB
			MaxRetries:     getEnvInt("KEYSTONE_SYNC_MAX_RETRIES", 5),
			CheckpointPath: getEnv("KEYSTONE_SYNC_CHECKPOINT_PATH", "/var/lib/keystone/.checkpoint"),
		},
		Features: FeaturesConfig{
			StrataEnabled:   false,
			SlateEnabled:    false,
			DagsterEnabled:  false,
			RayEnabled:      false,
			LanceDBEnabled:  false,
		},
		Monitoring: MonitoringConfig{
			Enabled:             getEnvBool("KEYSTONE_METRICS_ENABLED", true),
			MetricsPort:         getEnvInt("KEYSTONE_METRICS_PORT", 9090),
			HealthCheckInterval: getEnvInt("KEYSTONE_HEALTH_CHECK_INTERVAL", 10),
			LogLevel:            getEnv("KEYSTONE_LOG_LEVEL", "info"),
			LogOutput:           getEnv("KEYSTONE_LOG_OUTPUT", "/var/log/keystone-edge/"),
		},
		Resources: ResourceLimitsConfig{
			MaxMemoryMB:      getEnvInt("KEYSTONE_MAX_MEMORY_MB", 6144),
			MaxCPUPercent:    getEnvInt("KEYSTONE_MAX_CPU_PERCENT", 80),
			DiskWatermarkLow: getEnvInt("KEYSTONE_DISK_WATERMARK_LOW", 20),
			DiskWatermarkHigh: getEnvInt("KEYSTONE_DISK_WATERMARK_HIGH", 10),
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
