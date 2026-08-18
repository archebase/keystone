-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

-- Hilbert accepts longer plan descriptions than the original Keystone projection.
ALTER TABLE dc_plan
    MODIFY COLUMN description TEXT;
