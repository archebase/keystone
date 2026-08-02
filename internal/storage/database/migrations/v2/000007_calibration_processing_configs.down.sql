-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE calibration_captures
    DROP COLUMN processor_config_revision_id;

DROP TABLE calibration_processing_configs;
