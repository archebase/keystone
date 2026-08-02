-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE sync_logs
    DROP COLUMN source_snapshot;

DROP TABLE bulk_run_items;

ALTER TABLE bulk_runs
    DROP COLUMN counts_frozen_at,
    DROP COLUMN final_counts,
    DROP COLUMN materialized_at,
    DROP COLUMN materialize_cursor,
    DROP COLUMN snapshot_max_episode_id,
    DROP COLUMN preview_counts,
    DROP COLUMN request;

DROP TABLE episode_derivatives;

ALTER TABLE episodes
    DROP INDEX idx_episodes_hilbert_raw_data_id,
    DROP INDEX idx_episodes_cloud_publish_source,
    DROP CHECK chk_episodes_cloud_publish_source,
    DROP COLUMN cloud_publish_claimed_at,
    DROP COLUMN cloud_publish_source;

DROP TABLE stereo_split_image_configs;
