-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE stereo_split_image_configs (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    image_ref VARCHAR(1024) NULL,
    previous_image_ref VARCHAR(1024) NULL,
    max_concurrent INT NOT NULL DEFAULT 1,
    previous_max_concurrent INT NULL,
    created_by VARCHAR(191) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_stereo_split_max_concurrent CHECK (
        max_concurrent BETWEEN 1 AND 100
    ),
    INDEX idx_stereo_split_image_configs_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- A stable mutex revision exists before an image is configured. The first
-- administrator save appends the first executable image revision.
INSERT INTO stereo_split_image_configs (
    image_ref, previous_image_ref, max_concurrent, created_by
) VALUES (NULL, NULL, 1, 'migration-bootstrap');

ALTER TABLE episodes
    ADD COLUMN cloud_publish_source VARCHAR(32) NULL AFTER hilbert_raw_data_id,
    ADD COLUMN cloud_publish_claimed_at TIMESTAMP NULL AFTER cloud_publish_source,
    ADD CONSTRAINT chk_episodes_cloud_publish_source CHECK (
        cloud_publish_source IS NULL
        OR cloud_publish_source IN ('original', 'stereo_split')
    ),
    ADD INDEX idx_episodes_cloud_publish_source (
        cloud_publish_source, cloud_synced, updated_at
    ),
    ADD INDEX idx_episodes_hilbert_raw_data_id (hilbert_raw_data_id);

CREATE TABLE episode_derivatives (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    episode_id BIGINT NOT NULL,
    kind VARCHAR(64) NOT NULL,
    generation INT NOT NULL,

    processor_config_revision_id BIGINT NULL,
    processor_image VARCHAR(1024) NULL,
    source_uri VARCHAR(2048) NULL,
    source_etag VARCHAR(191) NULL,
    source_checksum VARCHAR(128) NULL,
    source_size_bytes BIGINT NULL,

    processing_status VARCHAR(32) NOT NULL DEFAULT 'queued',
    cancel_requested_at TIMESTAMP NULL,
    reconcile_after TIMESTAMP NULL,
    orbit_submission_id VARCHAR(191) NULL,
    orbit_request JSON NULL,
    orbit_snapshot_frozen_at TIMESTAMP NULL,
    orbit_job_id VARCHAR(191) NULL,
    orbit_submit_absent_at TIMESTAMP NULL,
    orbit_job_missing_since TIMESTAMP NULL,
    output_prefix VARCHAR(1024) NULL,
    mcap_path VARCHAR(1024) NULL,
    metadata_path VARCHAR(1024) NULL,
    manifest_path VARCHAR(1024) NULL,
    checksum VARCHAR(128) NULL,
    file_size_bytes BIGINT NULL,
    duration_sec DECIMAL(10, 2) NULL,
    processing_result JSON NULL,
    processing_error TEXT NULL,
    orbit_log_tail MEDIUMTEXT NULL,
    submit_attempt_count INT NOT NULL DEFAULT 0,
    verification_attempt_count INT NOT NULL DEFAULT 0,
    processing_started_at TIMESTAMP NULL,
    processing_finished_at TIMESTAMP NULL,

    orbit_delete_status VARCHAR(32) NOT NULL DEFAULT 'not_required',
    orbit_delete_attempt_count INT NOT NULL DEFAULT 0,
    orbit_delete_next_retry_at TIMESTAMP NULL,
    orbit_delete_error TEXT NULL,
    orbit_delete_accepted_at TIMESTAMP NULL,

    qa_status VARCHAR(32) NOT NULL DEFAULT 'not_started',
    qa_attempt_count INT NOT NULL DEFAULT 0,
    qa_next_retry_at TIMESTAMP NULL,
    qa_score DECIMAL(4, 3) NULL,
    quality_flag TEXT NULL,
    qa_result JSON NULL,
    qa_error TEXT NULL,
    qa_started_at TIMESTAMP NULL,
    qa_finished_at TIMESTAMP NULL,

    created_by VARCHAR(191) NULL,
    updated_by VARCHAR(191) NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

    UNIQUE INDEX idx_episode_derivatives_episode_kind (episode_id, kind),
    UNIQUE INDEX idx_episode_derivatives_submission (orbit_submission_id),
    INDEX idx_episode_derivatives_processing (
        processing_status, reconcile_after, updated_at
    ),
    INDEX idx_episode_derivatives_delete (
        orbit_delete_status, orbit_delete_next_retry_at, updated_at
    ),
    INDEX idx_episode_derivatives_qa (
        qa_status, qa_next_retry_at, updated_at
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE bulk_runs
    ADD COLUMN request JSON NULL AFTER action,
    ADD COLUMN preview_counts JSON NULL AFTER request,
    ADD COLUMN snapshot_max_episode_id BIGINT NULL AFTER preview_counts,
    ADD COLUMN materialize_cursor BIGINT NULL AFTER snapshot_max_episode_id,
    ADD COLUMN materialized_at TIMESTAMP NULL AFTER materialize_cursor,
    ADD COLUMN final_counts JSON NULL AFTER materialized_at,
    ADD COLUMN counts_frozen_at TIMESTAMP NULL AFTER final_counts;

CREATE TABLE bulk_run_items (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    bulk_run_id VARCHAR(64) NOT NULL,
    episode_id BIGINT NOT NULL,
    derivative_id BIGINT NULL,
    derivative_generation INT NULL,
    admission_status VARCHAR(32) NOT NULL DEFAULT 'pending',
    result_reason VARCHAR(64) NULL,
    result_snapshot JSON NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE INDEX idx_bulk_run_items_run_episode (bulk_run_id, episode_id),
    INDEX idx_bulk_run_items_run_status (bulk_run_id, admission_status, id),
    INDEX idx_bulk_run_items_derivative (derivative_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE sync_logs
    ADD COLUMN source_snapshot JSON NULL AFTER source_path;

-- Every pre-feature sync attempt used the original Episode object. Conservatively
-- lock any Episode with sync evidence to that source, including failed/canceled
-- attempts whose external side effects cannot be disproved.
UPDATE episodes e
SET e.cloud_publish_source = 'original',
    e.cloud_publish_claimed_at = COALESCE(
        (SELECT MIN(sl.started_at) FROM sync_logs sl WHERE sl.episode_id = e.id),
        e.cloud_synced_at,
        CURRENT_TIMESTAMP
    )
WHERE e.cloud_synced = TRUE
   OR e.hilbert_raw_data_id IS NOT NULL
   OR EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id);

-- Backfill the immutable upload input before the new worker starts. This covers
-- pending, failed and canceled rows that may be recovered after deployment, and
-- also makes completed history self-describing.
UPDATE sync_logs sl
INNER JOIN episodes e ON e.id = sl.episode_id
SET sl.source_snapshot = JSON_OBJECT(
    'source_type', 'original',
    'backend', CASE
        WHEN e.storage_backend = 'keystone_tos' THEN 'tos'
        ELSE 'minio'
    END,
    'bucket', CASE
        WHEN e.storage_backend = 'keystone_tos'
            THEN COALESCE(NULLIF(JSON_UNQUOTE(JSON_EXTRACT(e.metadata, '$.bucket')), ''), '')
        ELSE SUBSTRING_INDEX(TRIM(LEADING '/' FROM e.mcap_path), '/', 1)
    END,
    'object_key', CASE
        WHEN e.storage_backend = 'keystone_tos'
            THEN COALESCE(
                NULLIF(JSON_UNQUOTE(JSON_EXTRACT(e.metadata, '$.object_key')), ''),
                TRIM(LEADING '/' FROM e.mcap_path)
            )
        ELSE SUBSTRING(
            TRIM(LEADING '/' FROM e.mcap_path),
            LENGTH(SUBSTRING_INDEX(TRIM(LEADING '/' FROM e.mcap_path), '/', 1)) + 2
        )
    END,
    'size_bytes', e.file_size_bytes,
    'sha256', e.checksum,
    'bag_name', CONCAT(e.episode_id, '.mcap')
)
WHERE sl.source_snapshot IS NULL
  AND e.file_size_bytes > 0
  AND e.checksum REGEXP '^[0-9A-Fa-f]{64}$';
