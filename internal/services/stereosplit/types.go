// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package stereosplit owns the durable DECXIN stereo-split lifecycle.
package stereosplit

import (
	"errors"
	"time"
)

const (
	// Kind is the only derivative kind implemented by this module.
	Kind = "stereo_split"
	// MaxConfigurableConcurrent caps the administrator-controlled active Job limit.
	MaxConfigurableConcurrent = 100

	// ProcessingQueued is waiting for available Orbit capacity.
	ProcessingQueued = "queued"
	// ProcessingSubmitting has a frozen request being submitted to Orbit.
	ProcessingSubmitting = "submitting"
	// ProcessingPending is accepted by Orbit but not yet running.
	ProcessingPending = "pending"
	// ProcessingRunning is executing in Orbit.
	ProcessingRunning = "running"
	// ProcessingVerifying is validating the bound outputs and manifest.
	ProcessingVerifying = "verifying"
	// ProcessingSucceeded has validated outputs and proceeds through derivative QA.
	ProcessingSucceeded = "succeeded"
	// ProcessingFailed is a terminal processing failure.
	ProcessingFailed = "failed"
	// ProcessingCanceled is a terminal operator-requested cancellation.
	ProcessingCanceled = "canceled"

	// DeleteNotRequired means no Orbit cleanup request is needed.
	DeleteNotRequired = "not_required"
	// DeletePending means Orbit accepted or still needs a cleanup request.
	DeletePending = "pending"
	// DeleteCompleted means Orbit accepted deletion or reported the Job absent.
	// Kubernetes PV/PVC disappearance is handled asynchronously by the cluster.
	DeleteCompleted = "completed"

	// QANotStarted means derivative QA has not been scheduled.
	QANotStarted = "not_started"
	// QAPending means derivative QA is scheduled.
	QAPending = "pending"
	// QARunning means derivative QA is executing.
	QARunning = "running"
	// QAApproved means the derivative passed automatic QA.
	QAApproved = "approved"
	// QAFailed means derivative QA failed or rejected the output.
	QAFailed = "failed"

	// CloudSourceOriginal selects the original Episode as its canonical cloud representation.
	CloudSourceOriginal = "original"
	// CloudSourceStereoSplit selects the approved stereo-split derivative as canonical.
	CloudSourceStereoSplit = Kind

	// BulkAdmissionPending has not made an admission decision yet.
	BulkAdmissionPending = "pending"
	// BulkAdmissionAdmitted created or retried a derivative generation.
	BulkAdmissionAdmitted = "admitted"
	// BulkAdmissionSkipped records a stable ineligibility reason.
	BulkAdmissionSkipped = "skipped"
	// BulkAdmissionCanceled stopped before creating a derivative generation.
	BulkAdmissionCanceled = "canceled"

	// BulkReasonEligible admitted a first generation.
	BulkReasonEligible = "eligible"
	// BulkReasonEligibleRetry admitted a replacement generation after cleanup.
	BulkReasonEligibleRetry = "eligible_retry"
	// BulkReasonAlreadyDerived skips an Episode with a successful derivative.
	BulkReasonAlreadyDerived = "already_derived"
	// BulkReasonCloudSourceLocked skips an Episode committed to its original source.
	BulkReasonCloudSourceLocked = "cloud_source_locked"
	// BulkReasonProcessingActive skips an Episode with active derivative work.
	BulkReasonProcessingActive = "processing_active"
	// BulkReasonOrbitDeletePending skips a retry until cleanup is verified.
	BulkReasonOrbitDeletePending = "orbit_delete_pending"
	// BulkReasonSourceUnavailable skips an Episode without a usable TOS source.
	BulkReasonSourceUnavailable = "source_unavailable"
	// BulkReasonCanceledBeforeAdmit records cancellation before generation creation.
	BulkReasonCanceledBeforeAdmit = "canceled_before_admission"
)

var (
	// ErrDisabled rejects operations when derivative processing is disabled.
	ErrDisabled = errors.New("stereo split processing is disabled")
	// ErrNotFound indicates no stereo-split derivative exists for the Episode.
	ErrNotFound = errors.New("stereo split derivative not found")
	// ErrEpisodeNotFound indicates the source Episode does not exist.
	ErrEpisodeNotFound = errors.New("episode not found")
	// ErrSourceUnavailable indicates the Episode has no valid TOS source identity.
	ErrSourceUnavailable = errors.New("episode source is unavailable")
	// ErrCloudSourceLocked indicates the Episode already committed to its original source.
	ErrCloudSourceLocked = errors.New("episode cloud publish source is locked")
	// ErrAlreadyDerived indicates a successful derivative already exists.
	ErrAlreadyDerived = errors.New("episode is already derived")
	// ErrProcessingActive indicates a derivative generation is still active.
	ErrProcessingActive = errors.New("stereo split processing is active")
	// ErrRetryRequired indicates the current terminal generation must be retried explicitly.
	ErrRetryRequired = errors.New("stereo split retry is required")
	// ErrCleanupPending indicates Orbit cleanup has not yet been verified.
	ErrCleanupPending = errors.New("orbit delete is pending")
	// ErrQAUnavailable indicates derivative QA cannot currently run.
	ErrQAUnavailable = errors.New("stereo split qa is unavailable")
	// ErrQANotApproved indicates the derivative is not eligible for cloud sync.
	ErrQANotApproved = errors.New("stereo split qa is not approved")
	// ErrCloudSyncActive indicates cloud sync has started or completed for the derivative.
	ErrCloudSyncActive = errors.New("stereo split cloud sync is active or completed")
	// ErrConfigChanged indicates an optimistic image revision update lost a race.
	ErrConfigChanged = errors.New("stereo split processing config changed")
	// ErrInvalidMaxConcurrent rejects unsafe administrator concurrency settings.
	ErrInvalidMaxConcurrent = errors.New("stereo split max concurrent must be between 1 and 100")
	// ErrImageNotConfigured indicates no immutable processing image digest is selected.
	ErrImageNotConfigured = errors.New("stereo split image is not configured")
)

// Config controls the fixed stereo-split implementation. Values affecting a
// Job are frozen into the derivative row before Orbit is called.
type Config struct {
	Enabled             bool
	OutputBucket        string
	OutputPrefix        string
	Resources           Resources
	ActiveDeadline      int64
	TTLSecondsAfterDone int32
	PollInterval        time.Duration
	MaxSourceBytes      int64
	LogTailBytes        int
}

// Resources contains Kubernetes resource requests and limits sent to Orbit.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// Derivative is the stable, current stereo-split record for one Episode.
type Derivative struct {
	ID                        int64      `db:"id" json:"id"`
	EpisodeID                 int64      `db:"episode_id" json:"episode_id"`
	Kind                      string     `db:"kind" json:"kind"`
	Generation                int        `db:"generation" json:"generation"`
	ProcessorConfigRevisionID *int64     `db:"processor_config_revision_id" json:"processor_config_revision_id,omitempty"`
	ProcessorImage            string     `db:"processor_image" json:"processor_image,omitempty"`
	SourceURI                 string     `db:"source_uri" json:"source_uri,omitempty"`
	SourceETag                string     `db:"source_etag" json:"source_etag,omitempty"`
	SourceChecksum            string     `db:"source_checksum" json:"source_checksum,omitempty"`
	SourceSizeBytes           *int64     `db:"source_size_bytes" json:"source_size_bytes,omitempty"`
	ProcessingStatus          string     `db:"processing_status" json:"processing_status"`
	CancelRequestedAt         *time.Time `db:"cancel_requested_at" json:"cancel_requested_at,omitempty"`
	OrbitSubmissionID         string     `db:"orbit_submission_id" json:"orbit_submission_id,omitempty"`
	OrbitJobID                string     `db:"orbit_job_id" json:"orbit_job_id,omitempty"`
	OutputBucket              string     `db:"-" json:"output_bucket,omitempty"`
	OutputPrefix              string     `db:"output_prefix" json:"output_prefix,omitempty"`
	McapPath                  string     `db:"mcap_path" json:"mcap_path,omitempty"`
	MetadataPath              string     `db:"metadata_path" json:"metadata_path,omitempty"`
	ManifestPath              string     `db:"manifest_path" json:"manifest_path,omitempty"`
	Checksum                  string     `db:"checksum" json:"checksum,omitempty"`
	FileSizeBytes             *int64     `db:"file_size_bytes" json:"file_size_bytes,omitempty"`
	DurationSec               *float64   `db:"duration_sec" json:"duration_sec,omitempty"`
	ProcessingResult          any        `db:"-" json:"processing_result,omitempty"`
	ProcessingError           string     `db:"processing_error" json:"processing_error,omitempty"`
	OrbitLogTail              string     `db:"orbit_log_tail" json:"orbit_log_tail,omitempty"`
	OrbitDeleteStatus         string     `db:"orbit_delete_status" json:"orbit_delete_status"`
	OrbitDeleteError          string     `db:"orbit_delete_error" json:"orbit_delete_error,omitempty"`
	QAStatus                  string     `db:"qa_status" json:"qa_status"`
	QAScore                   *float64   `db:"qa_score" json:"qa_score,omitempty"`
	QualityFlag               string     `db:"quality_flag" json:"quality_flag,omitempty"`
	QAResult                  any        `db:"-" json:"qa_result,omitempty"`
	QAError                   string     `db:"qa_error" json:"qa_error,omitempty"`
	CreatedAt                 time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt                 time.Time  `db:"updated_at" json:"updated_at"`
}

// BulkAdmission is the immutable admission decision for one run member.
type BulkAdmission struct {
	EpisodeID            int64  `json:"episode_id"`
	DerivativeID         int64  `json:"derivative_id,omitempty"`
	DerivativeGeneration int    `json:"derivative_generation,omitempty"`
	AdmissionStatus      string `json:"admission_status"`
	Reason               string `json:"reason"`
}
