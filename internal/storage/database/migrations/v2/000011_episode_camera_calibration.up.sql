-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE calibration_sessions
    ADD INDEX idx_calibration_sessions_camera_status (
        camera_serial, status, updated_at
    );

ALTER TABLE episodes
    ADD COLUMN camera_serial VARCHAR(255) NULL AFTER metadata,
    ADD COLUMN calibration_capture_id VARCHAR(36) NULL AFTER camera_serial,
    ADD COLUMN calibration_result_sha256 CHAR(64) NULL AFTER calibration_capture_id,
    ADD INDEX idx_episodes_camera_calibration (
        camera_serial, calibration_capture_id
    );

ALTER TABLE episode_derivatives
    ADD COLUMN calibration_camera_serial VARCHAR(255) NULL AFTER source_size_bytes,
    ADD COLUMN calibration_session_id VARCHAR(36) NULL AFTER calibration_camera_serial,
    ADD COLUMN calibration_capture_id VARCHAR(36) NULL AFTER calibration_session_id,
    ADD COLUMN calibration_result_uri VARCHAR(2048) NULL AFTER calibration_capture_id,
    ADD COLUMN calibration_result_etag VARCHAR(191) NULL AFTER calibration_result_uri,
    ADD COLUMN calibration_result_size_bytes BIGINT NULL AFTER calibration_result_etag,
    ADD COLUMN calibration_result_sha256 CHAR(64) NULL AFTER calibration_result_size_bytes;
