-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

UPDATE dc_plan
SET description = LEFT(description, 200)
WHERE description IS NOT NULL AND CHAR_LENGTH(description) > 200;

ALTER TABLE dc_plan
    MODIFY COLUMN description VARCHAR(200);
