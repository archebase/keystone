-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE calibration_processing_configs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    image_ref VARCHAR(1024) NULL,
    previous_image_ref VARCHAR(1024) NULL,
    max_concurrent INT NOT NULL DEFAULT 1,
    previous_max_concurrent INT NULL,
    created_by VARCHAR(191) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_calibration_max_concurrent CHECK (
        max_concurrent BETWEEN 1 AND 100
    ),
    INDEX idx_calibration_processing_configs_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- The first row is both the unconfigured bootstrap revision and the stable
-- row locked while administrators append settings revisions.
INSERT INTO calibration_processing_configs (
    image_ref, previous_image_ref, max_concurrent, created_by
) VALUES (NULL, NULL, 1, 'migration-bootstrap');

ALTER TABLE calibration_captures
    ADD COLUMN processor_config_revision_id BIGINT NULL BEFORE processor_image;
