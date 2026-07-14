// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package dgwcompat provides a data-platform compatible upload control plane for POC use.
package dgwcompat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the environment-driven compatibility server settings.
type Config struct {
	Enabled bool

	GRPCAddr string

	TOSBucket      string
	TOSEndpoint    string
	TOSRegion      string
	TOSKeyPrefix   string
	UploadPartSize int64

	STSRoleTRN      string
	STSSessionTTL   time.Duration
	STSEndpoint     string
	AccessKeyID     string
	AccessKeySecret string
	MockSTS         bool

	DeviceJWTSecret string // #nosec G117 -- JWT signing secret is loaded from environment
	DeviceJWTTTL    time.Duration
	HilbertBaseURL  string
	HilbertCode     string
	HilbertPassword string // #nosec G117 -- service password is loaded from environment
}

// LoadConfigFromEnv loads dgw compatibility settings without touching Keystone's global config.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Enabled:         getEnvBool("KEYSTONE_DGW_COMPAT_ENABLED", false),
		GRPCAddr:        getEnv("KEYSTONE_DGW_GRPC_ADDR", getEnv("KEYSTONE_DGW_GATEWAY_ADDR", ":50053")),
		TOSBucket:       strings.TrimSpace(os.Getenv("KEYSTONE_DGW_TOS_BUCKET")),
		TOSEndpoint:     strings.TrimSpace(os.Getenv("KEYSTONE_DGW_TOS_ENDPOINT")),
		TOSRegion:       strings.TrimSpace(os.Getenv("KEYSTONE_DGW_TOS_REGION")),
		TOSKeyPrefix:    getEnv("KEYSTONE_DGW_TOS_KEY_PREFIX", "device-uploads"),
		UploadPartSize:  getEnvInt64("KEYSTONE_DGW_UPLOAD_PART_SIZE_BYTES", 8*1024*1024),
		STSRoleTRN:      strings.TrimSpace(os.Getenv("KEYSTONE_DGW_VOLCENGINE_STS_ROLE_TRN")),
		STSSessionTTL:   time.Duration(getEnvInt64("KEYSTONE_DGW_STS_SESSION_TTL_SECONDS", 3600)) * time.Second,
		STSEndpoint:     strings.TrimSpace(os.Getenv("KEYSTONE_DGW_VOLCENGINE_STS_ENDPOINT")),
		AccessKeyID:     strings.TrimSpace(os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID")),
		AccessKeySecret: strings.TrimSpace(os.Getenv("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET")),
		MockSTS:         getEnvBool("KEYSTONE_DGW_MOCK_STS", false),
		DeviceJWTSecret: strings.TrimSpace(os.Getenv("KEYSTONE_JWT_SECRET")),
		DeviceJWTTTL:    time.Duration(getEnvInt64("KEYSTONE_DGW_DEVICE_JWT_TTL_SECONDS", 900)) * time.Second,
		HilbertBaseURL:  strings.TrimSpace(os.Getenv("KEYSTONE_HILBERT_BASE_URL")),
		HilbertCode:     strings.TrimSpace(os.Getenv("KEYSTONE_HILBERT_SERVICE_ACCOUNT_CODE")),
		HilbertPassword: strings.TrimSpace(os.Getenv("KEYSTONE_HILBERT_SERVICE_ACCOUNT_PASSWORD")),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks only the compatibility server settings.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.GRPCAddr) == "" {
		return fmt.Errorf("KEYSTONE_DGW_GRPC_ADDR must not be empty")
	}
	if c.TOSBucket == "" {
		return fmt.Errorf("KEYSTONE_DGW_TOS_BUCKET is required when compatibility server is enabled")
	}
	if c.TOSEndpoint == "" || c.TOSRegion == "" {
		return fmt.Errorf("KEYSTONE_DGW_TOS_ENDPOINT and KEYSTONE_DGW_TOS_REGION are required when compatibility server is enabled")
	}
	if c.UploadPartSize <= 0 {
		return fmt.Errorf("KEYSTONE_DGW_UPLOAD_PART_SIZE_BYTES must be greater than 0")
	}
	if c.DeviceJWTSecret == "" || c.DeviceJWTTTL <= 0 {
		return fmt.Errorf("KEYSTONE_JWT_SECRET and a positive KEYSTONE_DGW_DEVICE_JWT_TTL_SECONDS are required")
	}
	if c.HilbertBaseURL == "" || c.HilbertCode == "" || c.HilbertPassword == "" {
		return fmt.Errorf("KEYSTONE_HILBERT_BASE_URL and service account credentials are required")
	}
	if c.MockSTS {
		return nil
	}
	if c.STSRoleTRN == "" {
		return fmt.Errorf("KEYSTONE_DGW_VOLCENGINE_STS_ROLE_TRN is required when mock STS is disabled")
	}
	if c.STSSessionTTL <= 0 {
		return fmt.Errorf("KEYSTONE_DGW_STS_SESSION_TTL_SECONDS must be greater than 0")
	}
	if c.AccessKeyID == "" || c.AccessKeySecret == "" {
		return fmt.Errorf("KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_ID and KEYSTONE_DGW_VOLCENGINE_ACCESS_KEY_SECRET are required")
	}
	return nil
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func getEnvInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
