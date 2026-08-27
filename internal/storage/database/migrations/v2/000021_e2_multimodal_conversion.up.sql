-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE e2_multimodal_conversion_image_configs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    image_ref VARCHAR(1024) NULL,
    previous_image_ref VARCHAR(1024) NULL,
    max_concurrent INT NOT NULL DEFAULT 1,
    previous_max_concurrent INT NULL,
    resource_limits_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    previous_resource_limits_enabled BOOLEAN NULL,
    created_by VARCHAR(191) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_e2_multimodal_conversion_max_concurrent CHECK (
        max_concurrent BETWEEN 1 AND 100
    ),
    INDEX idx_e2_multimodal_conversion_image_configs_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO e2_multimodal_conversion_image_configs (
    image_ref,
    previous_image_ref,
    max_concurrent,
    previous_max_concurrent,
    resource_limits_enabled,
    previous_resource_limits_enabled,
    created_by
) VALUES (
    NULL,
    NULL,
    1,
    NULL,
    TRUE,
    NULL,
    'migration-bootstrap'
);

ALTER TABLE episodes
    DROP CONSTRAINT chk_episodes_cloud_publish_source,
    ADD CONSTRAINT chk_episodes_cloud_publish_source CHECK (
        cloud_publish_source IS NULL
        OR cloud_publish_source IN (
            'original',
            'stereo_split',
            'depth_normalization',
            'e2_multimodal_conversion'
        )
    );
