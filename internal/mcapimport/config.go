// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package mcapimport imports a local MCAP file through Keystone's Data Gateway.
package mcapimport

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	defaultParallelUploads = 4
	defaultRPCTimeout      = 30 * time.Second
)

// Config contains all inputs needed for one MCAP import.
type Config struct {
	Endpoint      string
	UseTLS        bool
	TLSCAFile     string
	TLSServerName string
	RPCTimeout    time.Duration

	FilePath     string
	WorkspaceID  int64
	DCPlanID     int64
	TaskID       string
	CaptureID    string
	DurationSec  float64
	CameraSerial string
	Parallel     int

	DeviceID         string
	DeviceCredential string // #nosec G117 -- device credential is held in memory only
	DeviceName       string
	DeviceAuthToken  string // #nosec G117 -- one-time initialization token is held in memory only
	CredentialsFile  string
}

type persistedDeviceCredential struct {
	DeviceID     string `json:"device_id"`
	DeviceAPIKey string `json:"device_api_key"` // #nosec G117 -- intentionally persisted in an operator-owned 0600 file
	WorkspaceID  int64  `json:"workspace_id"`
}

// ParseConfig parses command-line arguments and validates the resulting configuration.
func ParseConfig(args []string) (Config, error) {
	cfg := Config{
		DeviceCredential: strings.TrimSpace(os.Getenv("KEYSTONE_IMPORT_DEVICE_API_KEY")),
		DeviceAuthToken:  strings.TrimSpace(os.Getenv("KEYSTONE_IMPORT_DEVICE_AUTH_TOKEN")),
	}
	flags := flag.NewFlagSet("keystone-import-mcap", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	flags.StringVar(&cfg.Endpoint, "endpoint", "", "Keystone Data Gateway gRPC endpoint (host:port)")
	flags.BoolVar(&cfg.UseTLS, "tls", true, "use TLS for the Data Gateway connection")
	flags.StringVar(&cfg.TLSCAFile, "tls-ca-file", "", "optional PEM CA bundle")
	flags.StringVar(&cfg.TLSServerName, "tls-server-name", "", "optional TLS server-name override")
	flags.DurationVar(&cfg.RPCTimeout, "rpc-timeout", defaultRPCTimeout, "timeout for each gRPC request")

	flags.StringVar(&cfg.FilePath, "file", "", "local MCAP file to upload")
	flags.Int64Var(&cfg.WorkspaceID, "workspace-id", 0, "target workspace ID")
	flags.Int64Var(&cfg.DCPlanID, "dc-plan-id", 0, "data-collection plan ID bound to the task")
	flags.StringVar(&cfg.TaskID, "task-id", "", "existing uploadable Keystone task ID")
	flags.StringVar(&cfg.CaptureID, "capture-id", "", "capture ID (defaults to a generated UUID)")
	flags.Float64Var(&cfg.DurationSec, "duration-sec", 0, "optional positive recording duration in seconds")
	flags.StringVar(&cfg.CameraSerial, "camera-serial", "", "optional camera serial for calibration association")
	flags.IntVar(&cfg.Parallel, "parallel", defaultParallelUploads, "number of concurrent TOS part uploads")

	flags.StringVar(&cfg.DeviceID, "device-id", strings.TrimSpace(os.Getenv("KEYSTONE_IMPORT_DEVICE_ID")), "initialized device ID")
	flags.StringVar(&cfg.DeviceName, "device-name", strings.TrimSpace(os.Getenv("KEYSTONE_IMPORT_DEVICE_NAME")), "device name for first-time initialization")
	flags.StringVar(&cfg.CredentialsFile, "device-credentials-file", "", "load device credentials, or save them after first-time initialization")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(cfg.CredentialsFile) != "" {
		absCredentialsPath, err := filepath.Abs(cfg.CredentialsFile)
		if err != nil {
			return Config{}, fmt.Errorf("resolve device credentials path: %w", err)
		}
		cfg.CredentialsFile = absCredentialsPath
	}
	directAny := strings.TrimSpace(cfg.DeviceID) != "" || strings.TrimSpace(cfg.DeviceCredential) != ""
	initAny := strings.TrimSpace(cfg.DeviceName) != "" || strings.TrimSpace(cfg.DeviceAuthToken) != ""
	if !directAny && !initAny && cfg.CredentialsFile != "" {
		persisted, err := loadDeviceCredential(cfg.CredentialsFile)
		if err != nil {
			return Config{}, err
		}
		if cfg.WorkspaceID > 0 && persisted.WorkspaceID != cfg.WorkspaceID {
			return Config{}, fmt.Errorf("device credentials belong to workspace %d, not %d", persisted.WorkspaceID, cfg.WorkspaceID)
		}
		cfg.DeviceID = persisted.DeviceID
		cfg.DeviceCredential = persisted.DeviceAPIKey
	}
	normalizedCameraSerial, err := normalizeCameraSerial(cfg.CameraSerial)
	if err != nil {
		return Config{}, err
	}
	cfg.CameraSerial = normalizedCameraSerial
	if strings.TrimSpace(cfg.CaptureID) == "" {
		cfg.CaptureID = uuid.NewString()
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	absPath, err := filepath.Abs(cfg.FilePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve MCAP file path: %w", err)
	}
	cfg.FilePath = absPath
	return cfg, nil
}

// Validate checks configuration without making network calls.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("--endpoint is required")
	}
	if c.RPCTimeout <= 0 {
		return fmt.Errorf("--rpc-timeout must be greater than zero")
	}
	if strings.TrimSpace(c.FilePath) == "" {
		return fmt.Errorf("--file is required")
	}
	info, err := os.Stat(c.FilePath)
	if err != nil {
		return fmt.Errorf("stat MCAP file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--file must refer to a regular file")
	}
	if info.Size() <= 0 {
		return fmt.Errorf("--file must not be empty")
	}
	if !strings.EqualFold(filepath.Ext(c.FilePath), ".mcap") {
		return fmt.Errorf("--file must have a .mcap extension")
	}
	if c.WorkspaceID <= 0 {
		return fmt.Errorf("--workspace-id must be a positive integer")
	}
	if c.DCPlanID <= 0 {
		return fmt.Errorf("--dc-plan-id must be a positive integer")
	}
	if strings.TrimSpace(c.TaskID) == "" {
		return fmt.Errorf("--task-id is required")
	}
	if strings.TrimSpace(c.CaptureID) == "" {
		return fmt.Errorf("--capture-id is required")
	}
	if c.DurationSec < 0 {
		return fmt.Errorf("--duration-sec must be greater than zero when provided")
	}
	if _, err := normalizeCameraSerial(c.CameraSerial); err != nil {
		return err
	}
	if c.Parallel < 1 || c.Parallel > 32 {
		return fmt.Errorf("--parallel must be between 1 and 32")
	}

	directAny := strings.TrimSpace(c.DeviceID) != "" || strings.TrimSpace(c.DeviceCredential) != ""
	initAny := strings.TrimSpace(c.DeviceName) != "" || strings.TrimSpace(c.DeviceAuthToken) != ""
	if directAny && initAny {
		return fmt.Errorf("use either initialized-device credentials or first-time device initialization, not both")
	}
	if directAny {
		if strings.TrimSpace(c.DeviceID) == "" || strings.TrimSpace(c.DeviceCredential) == "" {
			return fmt.Errorf("--device-id and KEYSTONE_IMPORT_DEVICE_API_KEY must be provided together")
		}
		return nil
	}
	if initAny {
		if strings.TrimSpace(c.DeviceName) == "" || strings.TrimSpace(c.DeviceAuthToken) == "" {
			return fmt.Errorf("--device-name and KEYSTONE_IMPORT_DEVICE_AUTH_TOKEN must be provided together")
		}
		if strings.TrimSpace(c.CredentialsFile) == "" {
			return fmt.Errorf("--device-credentials-file is required for first-time device initialization")
		}
		if _, err := os.Stat(c.CredentialsFile); err == nil {
			return fmt.Errorf("--device-credentials-file already exists; refusing to overwrite it")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat device credentials file: %w", err)
		}
		parent, err := os.Stat(filepath.Dir(c.CredentialsFile))
		if err != nil {
			return fmt.Errorf("stat device credentials directory: %w", err)
		}
		if !parent.IsDir() {
			return fmt.Errorf("device credentials parent path is not a directory")
		}
		return nil
	}
	return fmt.Errorf("device authentication is required")
}

func normalizeCameraSerial(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len([]rune(value)) > 255 {
		return "", fmt.Errorf("--camera-serial must not exceed 255 characters")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("--camera-serial must not contain control characters")
		}
	}
	return value, nil
}

func loadDeviceCredential(path string) (persistedDeviceCredential, error) {
	// #nosec G304 -- path is explicitly supplied by the CLI operator.
	file, err := os.Open(path)
	if err != nil {
		return persistedDeviceCredential{}, fmt.Errorf("open device credentials file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		statErr := fmt.Errorf("stat device credentials file: %w", err)
		if closeErr := file.Close(); closeErr != nil {
			return persistedDeviceCredential{}, errors.Join(statErr, fmt.Errorf("close device credentials file: %w", closeErr))
		}
		return persistedDeviceCredential{}, statErr
	}
	if info.Mode().Perm()&0o077 != 0 {
		permissionErr := fmt.Errorf("device credentials file permissions must be 0600 or stricter")
		if closeErr := file.Close(); closeErr != nil {
			return persistedDeviceCredential{}, errors.Join(permissionErr, fmt.Errorf("close device credentials file: %w", closeErr))
		}
		return persistedDeviceCredential{}, permissionErr
	}
	var persisted persistedDeviceCredential
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&persisted)
	closeErr := file.Close()
	if decodeErr != nil {
		return persistedDeviceCredential{}, errors.Join(
			fmt.Errorf("decode device credentials file: %w", decodeErr),
			wrapCloseError("device credentials file", closeErr),
		)
	}
	if closeErr != nil {
		return persistedDeviceCredential{}, fmt.Errorf("close device credentials file: %w", closeErr)
	}
	if strings.TrimSpace(persisted.DeviceID) == "" || strings.TrimSpace(persisted.DeviceAPIKey) == "" || persisted.WorkspaceID <= 0 {
		return persistedDeviceCredential{}, fmt.Errorf("device credentials file is incomplete")
	}
	return persisted, nil
}

func wrapCloseError(subject string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", subject, err)
}
