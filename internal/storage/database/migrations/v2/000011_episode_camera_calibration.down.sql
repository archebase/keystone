-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episode_derivatives
    DROP COLUMN calibration_result_sha256,
    DROP COLUMN calibration_result_size_bytes,
    DROP COLUMN calibration_result_etag,
    DROP COLUMN calibration_result_uri,
    DROP COLUMN calibration_capture_id,
    DROP COLUMN calibration_session_id,
    DROP COLUMN calibration_camera_serial;

ALTER TABLE episodes
    DROP INDEX idx_episodes_camera_calibration,
    DROP COLUMN calibration_result_sha256,
    DROP COLUMN calibration_capture_id,
    DROP COLUMN camera_serial;

ALTER TABLE calibration_sessions
    DROP INDEX idx_calibration_sessions_camera_status;
