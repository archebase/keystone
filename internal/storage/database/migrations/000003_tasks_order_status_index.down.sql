-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000003_tasks_order_status_index.down.sql
-- Revert composite index on tasks

DROP INDEX idx_tasks_order_status_del ON tasks;
