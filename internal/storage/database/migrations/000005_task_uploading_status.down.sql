-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

UPDATE tasks
SET status = 'in_progress'
WHERE status = 'uploading';

ALTER TABLE tasks
  MODIFY COLUMN status ENUM('pending', 'ready', 'in_progress', 'completed', 'failed', 'cancelled') DEFAULT 'pending';
