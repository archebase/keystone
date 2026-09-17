-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

-- Guarded for the same reason as the up migration: a database may carry these
-- indexes already, and a failed statement aborts the migration.

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'tasks'
          AND index_name = 'idx_tasks_plan_status_del') > 0,
    'ALTER TABLE tasks DROP INDEX idx_tasks_plan_status_del',
    'DO 0');
PREPARE drop_idx FROM @idx_ddl;
EXECUTE drop_idx;
DEALLOCATE PREPARE drop_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'data_collectors'
          AND index_name = 'idx_collectors_operator_del') > 0,
    'ALTER TABLE data_collectors DROP INDEX idx_collectors_operator_del',
    'DO 0');
PREPARE drop_idx FROM @idx_ddl;
EXECUTE drop_idx;
DEALLOCATE PREPARE drop_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'robots'
          AND index_name = 'idx_robots_device_del') > 0,
    'ALTER TABLE robots DROP INDEX idx_robots_device_del',
    'DO 0');
PREPARE drop_idx FROM @idx_ddl;
EXECUTE drop_idx;
DEALLOCATE PREPARE drop_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'workstations'
          AND index_name = 'idx_workstations_ws_del') > 0,
    'ALTER TABLE workstations DROP INDEX idx_workstations_ws_del',
    'DO 0');
PREPARE drop_idx FROM @idx_ddl;
EXECUTE drop_idx;
DEALLOCATE PREPARE drop_idx;

SET @idx_ddl := IF((SELECT COUNT(*) FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'episodes'
          AND index_name = 'idx_episodes_del_plan_synced_qa') > 0,
    'ALTER TABLE episodes DROP INDEX idx_episodes_del_plan_synced_qa',
    'DO 0');
PREPARE drop_idx FROM @idx_ddl;
EXECUTE drop_idx;
DEALLOCATE PREPARE drop_idx;
