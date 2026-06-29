-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE workstations
    DROP INDEX idx_current_collector,
    DROP INDEX idx_current_robot,
    DROP INDEX idx_current,
    DROP INDEX idx_superseded_by;

ALTER TABLE workstations
    DROP COLUMN _current_collector_unique,
    DROP COLUMN _current_robot_unique,
    DROP COLUMN is_current,
    DROP COLUMN superseded_at,
    DROP COLUMN superseded_by;

ALTER TABLE workstations
    ADD COLUMN _collector_unique VARCHAR(200) GENERATED ALWAYS AS (
        CONCAT(IFNULL(data_collector_id, ''), '|', IFNULL(deleted_at, ''))
    ) STORED,
    ADD UNIQUE INDEX idx_datacollector_del (_collector_unique);
