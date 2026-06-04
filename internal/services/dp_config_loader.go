// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// DPConfigFile is the subset of data-platform config consumed by direct sync.
type DPConfigFile struct {
	Version   *int              `json:"version,omitempty"`
	Endpoints DPConfigEndpoints `json:"endpoints"`
	Devices   []DPDeviceProfile `json:"devices"`
}

// DPConfigEndpoints contains the auth and gateway endpoints from a DP config file.
type DPConfigEndpoints struct {
	Auth    string `json:"auth"`
	Gateway string `json:"gateway"`
}

// DPDeviceProfile contains upload credentials and tags for one DP device.
type DPDeviceProfile struct {
	DeviceID string            `json:"deviceId"`
	APIKey   string            `json:"apiKey"`
	Tags     map[string]string `json:"tags"`
}

// DPResolvedEndpoint is a normalized upload service endpoint.
type DPResolvedEndpoint struct {
	Target     string
	UseTLS     bool
	ServerName string
}

// DPDeviceUploadConfig contains the resolved upload config for one asset ID.
type DPDeviceUploadConfig struct {
	ConfigPath string
	Auth       DPResolvedEndpoint
	Gateway    DPResolvedEndpoint
	Profile    DPDeviceProfile
}

func loadDPDeviceUploadConfig(configPath string, assetID string) (*DPDeviceUploadConfig, error) {
	configPath = strings.TrimSpace(configPath)
	assetID = strings.TrimSpace(assetID)
	if configPath == "" {
		return nil, fmt.Errorf("KEYSTONE_SYNC_DP_CONFIG is required")
	}
	if assetID == "" {
		return nil, fmt.Errorf("asset_id is required")
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // operator-controlled config path
	if err != nil {
		return nil, fmt.Errorf("read DP config %s: %w", configPath, err)
	}

	var cfg DPConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse DP config %s: %w", configPath, err)
	}
	if cfg.Version != nil && *cfg.Version != 3 {
		return nil, fmt.Errorf("DP config %s has unsupported version %d", configPath, *cfg.Version)
	}

	authEndpoint, err := parseDPResolvedEndpoint(cfg.Endpoints.Auth)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoints.auth in DP config %s: %w", configPath, err)
	}
	gatewayEndpoint, err := parseDPResolvedEndpoint(cfg.Endpoints.Gateway)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoints.gateway in DP config %s: %w", configPath, err)
	}

	devices := make(map[string]DPDeviceProfile, len(cfg.Devices))
	for idx, device := range cfg.Devices {
		deviceID := strings.TrimSpace(device.DeviceID)
		if deviceID == "" {
			return nil, fmt.Errorf("DP config %s devices[%d].deviceId is empty", configPath, idx)
		}
		if _, exists := devices[deviceID]; exists {
			return nil, fmt.Errorf("DP config %s has duplicate deviceId %q", configPath, deviceID)
		}
		device.DeviceID = deviceID
		devices[deviceID] = device
	}

	profile, ok := devices[assetID]
	if !ok {
		return nil, fmt.Errorf("DP config %s has no device profile for asset_id %q", configPath, assetID)
	}
	profile.APIKey = strings.TrimSpace(profile.APIKey)
	if profile.APIKey == "" {
		return nil, fmt.Errorf("DP config %s device %q apiKey is empty", configPath, assetID)
	}
	if len(profile.Tags) == 0 {
		return nil, fmt.Errorf("DP config %s device %q tags must be non-empty", configPath, assetID)
	}
	for key := range profile.Tags {
		if key == "" {
			return nil, fmt.Errorf("DP config %s device %q has an empty tag key", configPath, assetID)
		}
	}

	return &DPDeviceUploadConfig{
		ConfigPath: configPath,
		Auth:       authEndpoint,
		Gateway:    gatewayEndpoint,
		Profile:    profile,
	}, nil
}

func parseDPResolvedEndpoint(raw string) (DPResolvedEndpoint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DPResolvedEndpoint{}, fmt.Errorf("endpoint is required")
	}

	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return DPResolvedEndpoint{}, err
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return DPResolvedEndpoint{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
		}
		if parsed.Host == "" || parsed.User != nil {
			return DPResolvedEndpoint{}, fmt.Errorf("endpoint must be host[:port]")
		}
		if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return DPResolvedEndpoint{}, fmt.Errorf("endpoint must not include path, query, or fragment")
		}
		host := parsed.Hostname()
		if host == "" {
			return DPResolvedEndpoint{}, fmt.Errorf("endpoint host is required")
		}
		target := parsed.Host
		if parsed.Port() == "" {
			defaultPort := "80"
			if parsed.Scheme == "https" {
				defaultPort = "443"
			}
			target = net.JoinHostPort(host, defaultPort)
		}
		return DPResolvedEndpoint{
			Target:     target,
			UseTLS:     parsed.Scheme == "https",
			ServerName: tlsServerNameForScheme(parsed.Scheme, host),
		}, nil
	}

	if strings.ContainsAny(value, "/?#") {
		return DPResolvedEndpoint{}, fmt.Errorf("bare endpoint must not include path, query, or fragment")
	}
	return DPResolvedEndpoint{Target: value, UseTLS: false}, nil
}

func tlsServerNameForScheme(scheme string, host string) string {
	if scheme == "https" {
		return host
	}
	return ""
}
