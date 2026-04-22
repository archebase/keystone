-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- migrations/000003_tasks_order_status_index.up.sql
-- Add composite index to support the two-query completed_count aggregation in ListOrders.
-- The query pattern is:
--   SELECT order_id, COUNT(*) FROM tasks
--   WHERE deleted_at IS NULL AND status = 'completed' AND order_id IN (...)
--   GROUP BY order_id
-- Without this index the engine falls back to the individual idx_order / idx_status
-- single-column indexes, requiring a full scan of all tasks per order.

CREATE INDEX idx_tasks_order_status_del ON tasks (order_id, status, deleted_at);
