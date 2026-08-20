-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

ALTER TABLE dc_plan
    ADD COLUMN status VARCHAR(100) NOT NULL DEFAULT 'pending_collection';
