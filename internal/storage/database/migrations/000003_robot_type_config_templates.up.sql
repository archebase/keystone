-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE IF NOT EXISTS robot_type_config_templates (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    robot_type_id BIGINT NOT NULL,
    filename VARCHAR(128) NOT NULL,
    content MEDIUMTEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,
    _active_unique VARCHAR(300) GENERATED ALWAYS AS (
        IF(deleted_at IS NULL, CONCAT(robot_type_id, '|', filename), NULL)
    ) STORED,
    UNIQUE INDEX idx_robot_type_config_templates_active (_active_unique),
    INDEX idx_robot_type_config_templates_robot_type (robot_type_id),
    INDEX idx_robot_type_config_templates_filename (filename),
    INDEX idx_robot_type_config_templates_deleted (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
