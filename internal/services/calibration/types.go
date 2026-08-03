// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package calibration owns calibration Capture processing and Session success.
package calibration

import (
	"context"
	"errors"
	"io"
	"time"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
)

// Capture and Session lifecycle states persisted by the calibration module.
const (
	// MaxConfigurableConcurrent caps the administrator-controlled active Job limit.
	MaxConfigurableConcurrent = 100

	StatusUploading  = "uploading"
	StatusUploaded   = "uploaded"
	StatusQueued     = "queued"
	StatusSubmitting = "submitting"
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusVerifying  = "verifying"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusSuperseded = "superseded"

	SessionRunning   = "running"
	SessionSucceeded = "succeeded"
)

// Domain errors returned by the calibration module's public interface.
var (
	ErrProcessingUnavailable = errors.New("calibration processing is unavailable")
	ErrCaptureNotFound       = errors.New("calibration capture not found")
	ErrSessionNotFound       = errors.New("calibration session not found")
	ErrCaptureUploading      = errors.New("calibration capture upload is not complete")
	ErrCaptureProcessed      = errors.New("calibration capture is already processed")
	ErrSessionSucceeded      = errors.New("calibration session already succeeded")
	ErrImageNotConfigured    = errors.New("calibration processing image is not configured")
	ErrConfigChanged         = errors.New("calibration processing configuration changed")
	ErrInvalidMaxConcurrent  = errors.New("invalid calibration max concurrent")
)

// Resources contains Kubernetes requests and limits sent to Orbit.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// Config controls fixed calibration processing behavior.
type Config struct {
	Resources           Resources
	ActiveDeadline      int64
	TTLSecondsAfterDone int32
	PollInterval        time.Duration
	MaxResultBytes      int64
	LogTailBytes        int
}

// CaptureSummary is the metadata for one MCAP upload without its full calibration result.
type CaptureSummary struct {
	ID                        int64     `db:"id" json:"id"`
	CaptureID                 string    `db:"capture_id" json:"capture_id"`
	CalibrationSessionID      string    `db:"calibration_session_id" json:"calibration_session_id"`
	AttemptNo                 int64     `db:"attempt_no" json:"attempt_no"`
	Status                    string    `db:"status" json:"status"`
	RobotID                   int64     `db:"robot_id" json:"robot_id"`
	DeviceID                  string    `db:"device_id" json:"device_id"`
	WorkspaceID               int64     `db:"workspace_id" json:"workspace_id"`
	Bucket                    string    `db:"bucket" json:"bucket"`
	ObjectKey                 string    `db:"object_key" json:"object_key"`
	FileSizeBytes             int64     `db:"file_size_bytes" json:"file_size_bytes"`
	DurationSec               float64   `db:"duration_sec" json:"duration_sec,omitempty"`
	ChecksumSHA256            string    `db:"checksum_sha256" json:"checksum_sha256"`
	ObjectETag                string    `db:"object_etag" json:"object_etag,omitempty"`
	Source                    string    `db:"source" json:"source,omitempty"`
	LocalOperator             string    `db:"local_operator" json:"local_operator,omitempty"`
	ProcessorConfigRevisionID int64     `db:"processor_config_revision_id" json:"processor_config_revision_id,omitempty"`
	ProcessorImage            string    `db:"processor_image" json:"processor_image,omitempty"`
	SourceETag                string    `db:"source_etag" json:"source_etag,omitempty"`
	OrbitSubmissionID         string    `db:"orbit_submission_id" json:"orbit_submission_id,omitempty"`
	OrbitJobID                string    `db:"orbit_job_id" json:"orbit_job_id,omitempty"`
	VerificationAttemptCount  int       `db:"verification_attempt_count" json:"-"`
	ResultObjectKey           string    `db:"result_object_key" json:"result_object_key,omitempty"`
	ResultSizeBytes           int64     `db:"result_size_bytes" json:"result_size_bytes,omitempty"`
	ResultChecksumSHA256      string    `db:"result_checksum_sha256" json:"result_checksum_sha256,omitempty"`
	AlgorithmVersion          string    `db:"algorithm_version" json:"algorithm_version,omitempty"`
	CalibrationError          string    `db:"calibration_error" json:"calibration_error,omitempty"`
	CreatedAt                 time.Time `db:"created_at" json:"created_at"`
	UpdatedAt                 time.Time `db:"updated_at" json:"updated_at"`
}

// Capture is one MCAP upload and its one-to-one calibration result.
type Capture struct {
	CaptureSummary
	ResultJSON string `db:"result_json" json:"-"`
	Result     any    `db:"-" json:"result,omitempty"`
}

// SessionStatus is the non-sensitive status exposed to a device poller.
type SessionStatus struct {
	SessionID           string    `db:"session_id" json:"session_id"`
	Status              string    `db:"status" json:"status"`
	SuccessfulCaptureID string    `db:"successful_capture_id" json:"successful_capture_id,omitempty"`
	CaptureCount        int64     `db:"capture_count" json:"capture_count"`
	UploadedCount       int64     `db:"uploaded_count" json:"uploaded_count"`
	ProcessedCount      int64     `db:"processed_count" json:"processed_count"`
	UpdatedAt           time.Time `db:"updated_at" json:"updated_at"`
}

// ListFilter selects one bounded page of admin-visible Captures.
type ListFilter struct {
	Status    string
	SessionID string
	DeviceID  string
	Limit     int
	Offset    int
}

// Orbit is the external execution seam used by the reconciler.
type Orbit interface {
	Submit(ctx context.Context, request orbitapi.SubmitRequest) (orbitapi.SubmitResponse, error)
	Get(ctx context.Context, id string) (orbitapi.Job, error)
	Logs(ctx context.Context, id string) (string, error)
	Stop(ctx context.Context, id string) (orbitapi.Job, error)
}

// ObjectStore is the TOS identity and content seam used for result verification.
type ObjectStore interface {
	StatObject(ctx context.Context, bucket, objectName string) (size int64, etag string, err error)
	OpenObject(ctx context.Context, bucket, objectName string) (io.ReadCloser, error)
}
