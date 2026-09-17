-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

-- Composite indexes backing the dc plan sync lookups.
--
-- Without them the planner can only fall back to single column indexes whose
-- leading column is non selective (status = 'pending', deleted_at IS NULL), so a
-- statement addressing one plan still scans a large index range. When that
-- statement is a SELECT ... FOR UPDATE, InnoDB locks every row it scans: one per
-- plan UPDATE held hundreds of row locks and took seconds, the dc plan sync could
-- not finish inside the 180s startup probe and the container was killed before it
-- committed, and two concurrent syncs acquiring the same rows in opposite order
-- deadlocked.
--
-- Every statement is guarded because databases patched while the incident was
-- handled already carry these indexes. A plain ADD INDEX would abort the
-- migration, and a failed migration is fatal at startup and leaves
-- schema_migrations_v2 dirty, which blocks every later migration.
--
-- The adds are pinned to ALGORITHM=INPLACE, LOCK=NONE so a database that does not
-- carry the indexes yet builds them online instead of taking a table lock that
-- could outlive the startup probe.

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'tasks'
          AND index_name = 'idx_tasks_plan_status_del') = 0,
    'ALTER TABLE tasks ADD INDEX idx_tasks_plan_status_del (dc_plan_id, status, deleted_at), ALGORITHM=INPLACE, LOCK=NONE',
    'DO 0');
PREPARE add_idx FROM @idx_ddl;
EXECUTE add_idx;
DEALLOCATE PREPARE add_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'data_collectors'
          AND index_name = 'idx_collectors_operator_del') = 0,
    'ALTER TABLE data_collectors ADD INDEX idx_collectors_operator_del (operator_id, deleted_at), ALGORITHM=INPLACE, LOCK=NONE',
    'DO 0');
PREPARE add_idx FROM @idx_ddl;
EXECUTE add_idx;
DEALLOCATE PREPARE add_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'robots'
          AND index_name = 'idx_robots_device_del') = 0,
    'ALTER TABLE robots ADD INDEX idx_robots_device_del (device_id, deleted_at), ALGORITHM=INPLACE, LOCK=NONE',
    'DO 0');
PREPARE add_idx FROM @idx_ddl;
EXECUTE add_idx;
DEALLOCATE PREPARE add_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'workstations'
          AND index_name = 'idx_workstations_ws_del') = 0,
    'ALTER TABLE workstations ADD INDEX idx_workstations_ws_del (workspace_id, deleted_at), ALGORITHM=INPLACE, LOCK=NONE',
    'DO 0');
PREPARE add_idx FROM @idx_ddl;
EXECUTE add_idx;
DEALLOCATE PREPARE add_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'episodes'
          AND index_name = 'idx_episodes_del_plan_synced_qa') = 0,
    'ALTER TABLE episodes ADD INDEX idx_episodes_del_plan_synced_qa (deleted_at, dc_plan_id, cloud_synced, qa_status, duration_sec), ALGORITHM=INPLACE, LOCK=NONE',
    'DO 0');
PREPARE add_idx FROM @idx_ddl;
EXECUTE add_idx;
DEALLOCATE PREPARE add_idx;
