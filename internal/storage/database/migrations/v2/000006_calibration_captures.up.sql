-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE calibration_sessions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    session_id VARCHAR(36) NOT NULL,
    robot_id BIGINT NOT NULL,
    device_id VARCHAR(255) NOT NULL,
    workspace_id BIGINT NOT NULL,
    status ENUM('running', 'succeeded') NOT NULL DEFAULT 'running',
    successful_capture_id VARCHAR(36) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE INDEX idx_calibration_sessions_session (session_id),
    INDEX idx_calibration_sessions_device_status (device_id, status),
    INDEX idx_calibration_sessions_workspace_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE calibration_captures (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    capture_id VARCHAR(36) NOT NULL,
    calibration_session_id VARCHAR(36) NOT NULL,
    attempt_no BIGINT NOT NULL,
    status ENUM(
        'uploading', 'uploaded', 'queued', 'submitting', 'pending',
        'running', 'verifying', 'succeeded', 'failed', 'superseded'
    ) NOT NULL DEFAULT 'uploading',

    bucket VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,
    file_size_bytes BIGINT NULL,
    duration_sec DECIMAL(12, 3) NULL,
    checksum_sha256 CHAR(64) NOT NULL,
    object_etag VARCHAR(255) NULL,
    logical_upload_id VARCHAR(36) NOT NULL,
    upload_id VARCHAR(36) NOT NULL,
    source VARCHAR(64) NULL,
    local_operator VARCHAR(255) NULL,
    uploaded_at TIMESTAMP NULL,

    processor_image VARCHAR(1024) NULL,
    source_etag VARCHAR(255) NULL,
    orbit_submission_id VARCHAR(191) NULL,
    orbit_request JSON NULL,
    orbit_job_id VARCHAR(191) NULL,
    orbit_log_tail MEDIUMTEXT NULL,
    reconcile_after TIMESTAMP NULL,
    processing_started_at TIMESTAMP NULL,
    processing_finished_at TIMESTAMP NULL,

    result_object_key VARCHAR(1024) NULL,
    result_size_bytes BIGINT NULL,
    result_checksum_sha256 CHAR(64) NULL,
    result_json JSON NULL,
    algorithm_version VARCHAR(191) NULL,
    calibration_error TEXT NULL,
    created_by VARCHAR(191) NULL,

    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    UNIQUE INDEX idx_calibration_captures_capture (capture_id),
    UNIQUE INDEX idx_calibration_captures_session_attempt (calibration_session_id, attempt_no),
    UNIQUE INDEX idx_calibration_captures_submission (orbit_submission_id),
    INDEX idx_calibration_captures_session_status (calibration_session_id, status),
    INDEX idx_calibration_captures_status_reconcile (status, reconcile_after, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
