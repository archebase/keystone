-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE camera_calibrations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    camera_serial VARCHAR(255) NOT NULL,
    bucket VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,
    size_bytes BIGINT NOT NULL,
    sha256 CHAR(64) NOT NULL,
    source ENUM('pipeline', 'manual') NOT NULL,
    calibration_session_id VARCHAR(36) NULL,
    capture_id VARCHAR(36) NULL,
    updated_by VARCHAR(191) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE INDEX idx_camera_calibrations_serial (camera_serial)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
