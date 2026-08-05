-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE calibration_captures
MODIFY COLUMN processing_stage ENUM('stereo_split', 'calibration') NOT NULL DEFAULT 'calibration';

UPDATE calibration_captures
SET processing_stage = 'calibration'
WHERE processing_stage = 'stereo_split'
  AND status IN ('uploading', 'uploaded', 'queued')
  AND stereo_split_execution IS NULL;
