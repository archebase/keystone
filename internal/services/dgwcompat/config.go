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

	AuthAddr       string
	GatewayAddr    string
	DeviceInitAddr string

	OSSBucket         string
	OSSPublicEndpoint string
	OSSKeyPrefix      string
	UploadPartSize    int64

	STSRoleARN      string
	STSSessionTTL   time.Duration
	STSRegion       string
	STSEndpoint     string
	AccessKeyID     string
	AccessKeySecret string
	MockSTS         bool

	DeviceAPIKey string
}

// LoadConfigFromEnv loads dgw compatibility settings without touching Keystone's global config.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Enabled:           getEnvBool("KEYSTONE_DGW_COMPAT_ENABLED", false),
		AuthAddr:          getEnv("KEYSTONE_DGW_AUTH_ADDR", ":50051"),
		GatewayAddr:       getEnv("KEYSTONE_DGW_GATEWAY_ADDR", ":50053"),
		DeviceInitAddr:    getEnv("KEYSTONE_DGW_DEVICE_INIT_ADDR", ":50057"),
		OSSBucket:         strings.TrimSpace(os.Getenv("KEYSTONE_DGW_OSS_BUCKET")),
		OSSPublicEndpoint: strings.TrimSpace(os.Getenv("KEYSTONE_DGW_OSS_PUBLIC_ENDPOINT")),
		OSSKeyPrefix:      getEnv("KEYSTONE_DGW_OSS_KEY_PREFIX", "ego-portal-lite"),
		UploadPartSize:    getEnvInt64("KEYSTONE_DGW_UPLOAD_PART_SIZE_BYTES", 8*1024*1024),
		STSRoleARN:        strings.TrimSpace(os.Getenv("KEYSTONE_DGW_STS_ROLE_ARN")),
		STSSessionTTL:     time.Duration(getEnvInt64("KEYSTONE_DGW_STS_SESSION_TTL_SECONDS", 3600)) * time.Second,
		STSRegion:         getEnv("KEYSTONE_DGW_ALIBABA_CLOUD_STS_REGION", "cn-shanghai"),
		STSEndpoint:       getEnv("KEYSTONE_DGW_ALIBABA_CLOUD_STS_ENDPOINT", "https://sts.cn-shanghai.aliyuncs.com"),
		AccessKeyID:       strings.TrimSpace(os.Getenv("KEYSTONE_DGW_ALIBABA_CLOUD_ACCESS_KEY_ID")),
		AccessKeySecret:   strings.TrimSpace(os.Getenv("KEYSTONE_DGW_ALIBABA_CLOUD_ACCESS_KEY_SECRET")),
		MockSTS:           getEnvBool("KEYSTONE_DGW_MOCK_STS", false),
		DeviceAPIKey:      getEnv("KEYSTONE_DGW_DEVICE_API_KEY", "keystone-poc-api-key"),
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
	if strings.TrimSpace(c.AuthAddr) == "" || strings.TrimSpace(c.GatewayAddr) == "" || strings.TrimSpace(c.DeviceInitAddr) == "" {
		return fmt.Errorf("KEYSTONE_DGW auth, gateway and device init addresses must not be empty")
	}
	if c.OSSBucket == "" {
		return fmt.Errorf("KEYSTONE_DGW_OSS_BUCKET is required when compatibility server is enabled")
	}
	if c.OSSPublicEndpoint == "" {
		return fmt.Errorf("KEYSTONE_DGW_OSS_PUBLIC_ENDPOINT is required when compatibility server is enabled")
	}
	if c.UploadPartSize <= 0 {
		return fmt.Errorf("KEYSTONE_DGW_UPLOAD_PART_SIZE_BYTES must be greater than 0")
	}
	if c.DeviceAPIKey == "" {
		return fmt.Errorf("KEYSTONE_DGW_DEVICE_API_KEY must not be empty")
	}
	if c.MockSTS {
		return nil
	}
	if c.STSRoleARN == "" {
		return fmt.Errorf("KEYSTONE_DGW_STS_ROLE_ARN is required when mock STS is disabled")
	}
	if c.STSSessionTTL <= 0 {
		return fmt.Errorf("KEYSTONE_DGW_STS_SESSION_TTL_SECONDS must be greater than 0")
	}
	if c.STSRegion == "" || c.STSEndpoint == "" {
		return fmt.Errorf("KEYSTONE_DGW_ALIBABA_CLOUD_STS_REGION and KEYSTONE_DGW_ALIBABA_CLOUD_STS_ENDPOINT are required")
	}
	if c.AccessKeyID == "" || c.AccessKeySecret == "" {
		return fmt.Errorf("KEYSTONE_DGW_ALIBABA_CLOUD_ACCESS_KEY_ID and KEYSTONE_DGW_ALIBABA_CLOUD_ACCESS_KEY_SECRET are required")
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
