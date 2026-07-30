-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- This rollback discards Hilbert Raw Data associations written after deployment.
-- Export episode_id/hilbert_raw_data_id mappings before applying it when they must
-- survive an application rollback.
ALTER TABLE episodes
    DROP CHECK chk_episodes_hilbert_raw_data_id_positive,
    DROP CHECK chk_episodes_provenance_pair,
    DROP COLUMN hilbert_raw_data_id,
    DROP COLUMN storage_backend,
    DROP COLUMN ingestion_channel;
