-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

UPDATE calibration_captures
SET processing_stage = 'stereo_split'
WHERE processing_stage = 'calibration'
  AND status IN ('uploading', 'uploaded', 'queued')
  AND stereo_split_execution IS NULL;

ALTER TABLE calibration_captures
MODIFY COLUMN processing_stage ENUM('stereo_split', 'calibration') NOT NULL DEFAULT 'stereo_split';
