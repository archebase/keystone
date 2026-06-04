-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE tasks
  MODIFY COLUMN status ENUM('pending', 'ready', 'in_progress', 'uploading', 'completed', 'failed', 'cancelled') DEFAULT 'pending';
