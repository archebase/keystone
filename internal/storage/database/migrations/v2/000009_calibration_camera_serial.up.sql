-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE calibration_sessions
ADD COLUMN camera_serial VARCHAR(255) NULL AFTER workspace_id;
