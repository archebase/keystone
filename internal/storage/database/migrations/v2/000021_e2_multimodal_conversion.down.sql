-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE episodes
    DROP CONSTRAINT chk_episodes_cloud_publish_source,
    ADD CONSTRAINT chk_episodes_cloud_publish_source CHECK (
        cloud_publish_source IS NULL
        OR cloud_publish_source IN (
            'original',
            'stereo_split',
            'depth_normalization'
        )
    );

DROP TABLE e2_multimodal_conversion_image_configs;
