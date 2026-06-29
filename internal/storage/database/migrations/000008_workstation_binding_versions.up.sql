-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE workstations
    ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT TRUE COMMENT 'Current binding version visible for new work',
    ADD COLUMN superseded_at TIMESTAMP NULL COMMENT 'When this binding was replaced by a newer workstation row',
    ADD COLUMN superseded_by BIGINT NULL COMMENT 'Newer workstation id that replaced this binding';

ALTER TABLE workstations DROP INDEX idx_datacollector_del;
ALTER TABLE workstations DROP COLUMN _collector_unique;

ALTER TABLE workstations
    ADD COLUMN _current_collector_unique VARCHAR(200) GENERATED ALWAYS AS (
        IF(is_current AND deleted_at IS NULL, CAST(data_collector_id AS CHAR), NULL)
    ) STORED,
    ADD COLUMN _current_robot_unique VARCHAR(200) GENERATED ALWAYS AS (
        IF(is_current AND deleted_at IS NULL, CAST(robot_id AS CHAR), NULL)
    ) STORED,
    ADD UNIQUE INDEX idx_current_collector (_current_collector_unique),
    ADD UNIQUE INDEX idx_current_robot (_current_robot_unique),
    ADD INDEX idx_current (is_current),
    ADD INDEX idx_superseded_by (superseded_by);
