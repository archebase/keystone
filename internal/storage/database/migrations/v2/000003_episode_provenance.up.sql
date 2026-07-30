-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Add nullable columns first so existing rows can be classified without defaults.
ALTER TABLE episodes
    ADD COLUMN ingestion_channel
        ENUM('axon_transfer', 'data_gateway') NULL
        COMMENT 'Keystone ingestion path that created the episode'
        AFTER local_dc_plan_id,
    ADD COLUMN storage_backend
        ENUM('minio', 'keystone_tos') NULL
        COMMENT 'Keystone storage backend holding source episode data'
        AFTER ingestion_channel,
    ADD COLUMN hilbert_raw_data_id BIGINT NULL
        COMMENT 'Hilbert raw data ID assigned during cloud sync'
        AFTER storage_backend;

-- Data Gateway compatibility uploads carry a durable server-written source marker.
UPDATE episodes
SET ingestion_channel = 'data_gateway',
    storage_backend = 'keystone_tos'
WHERE JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.source')) = 'dgwcompat';

-- The only other production Episode creation path is Axon Transfer.
UPDATE episodes
SET ingestion_channel = 'axon_transfer',
    storage_backend = 'minio'
WHERE ingestion_channel IS NULL;

-- Final creation-time provenance is mandatory and only the two approved pairs are valid.
ALTER TABLE episodes
    MODIFY COLUMN ingestion_channel
        ENUM('axon_transfer', 'data_gateway') NOT NULL
        COMMENT 'Keystone ingestion path that created the episode',
    MODIFY COLUMN storage_backend
        ENUM('minio', 'keystone_tos') NOT NULL
        COMMENT 'Keystone storage backend holding source episode data',
    ADD CONSTRAINT chk_episodes_provenance_pair CHECK (
        (ingestion_channel = 'axon_transfer' AND storage_backend = 'minio')
        OR
        (ingestion_channel = 'data_gateway' AND storage_backend = 'keystone_tos')
    ),
    ADD CONSTRAINT chk_episodes_hilbert_raw_data_id_positive CHECK (
        hilbert_raw_data_id IS NULL OR hilbert_raw_data_id > 0
    );
