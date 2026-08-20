-- SPDX-FileCopyrightText: 2026 ArcheBase
-- SPDX-License-Identifier: MulanPSL-2.0

CREATE TABLE local_cleanup_job_objects (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    job_id BIGINT NOT NULL,
    bucket VARCHAR(255) NOT NULL,
    object_key VARCHAR(500) NOT NULL,
    status ENUM('pending', 'completed', 'failed') NOT NULL DEFAULT 'pending',
    error_message TEXT NULL,
    UNIQUE INDEX idx_local_cleanup_job_object (job_id, bucket, object_key),
    INDEX idx_local_cleanup_job_object_status (job_id, status),
    CONSTRAINT fk_local_cleanup_job_objects_job
        FOREIGN KEY (job_id) REFERENCES local_cleanup_jobs(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
