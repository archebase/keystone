-- SPDX-FileCopyrightText: 2026 ArcheBase
--
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE IF NOT EXISTS device_id_sequences (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    factory_id BIGINT NOT NULL,
    robot_type_id BIGINT NOT NULL,
    next_sequence BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE INDEX idx_factory_robot_type (factory_id, robot_type_id),
    INDEX idx_factory (factory_id),
    INDEX idx_robot_type (robot_type_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
