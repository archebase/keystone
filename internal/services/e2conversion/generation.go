// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package e2conversion

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// resetDerivativeGenerationTx resets one terminal E2 generation for an explicit retry.
func resetDerivativeGenerationTx(ctx context.Context, tx *sqlx.Tx, derivativeID int64, actor string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET generation = generation + 1,
		    processor_config_revision_id = NULL, processor_image = NULL,
		    source_uri = NULL, source_etag = NULL, source_checksum = NULL, source_size_bytes = NULL,
		    processing_status = ?, cancel_requested_at = NULL, reconcile_after = NULL,
		    orbit_submission_id = NULL, orbit_request = NULL, orbit_snapshot_frozen_at = NULL,
		    orbit_job_id = NULL, orbit_submit_absent_at = NULL, orbit_job_missing_since = NULL,
		    output_prefix = NULL, mcap_path = NULL, metadata_path = NULL,
		    manifest_path = NULL, checksum = NULL, file_size_bytes = NULL, duration_sec = NULL,
		    processing_duration_sec = NULL,
		    processing_result = NULL, processing_error = NULL, orbit_log_tail = NULL,
		    submit_attempt_count = 0, verification_attempt_count = 0,
		    processing_started_at = NULL, processing_finished_at = NULL,
		    orbit_delete_status = ?, orbit_delete_attempt_count = 0,
		    orbit_delete_next_retry_at = NULL, orbit_delete_error = NULL, orbit_delete_accepted_at = NULL,
		    qa_status = ?, qa_attempt_count = 0, qa_next_retry_at = NULL, qa_score = NULL,
		    quality_flag = NULL, qa_result = NULL, qa_error = NULL,
		    qa_started_at = NULL, qa_finished_at = NULL,
		    updated_by = NULLIF(?, ''), updated_at = ?
		WHERE id = ?
	`, ProcessingQueued, DeleteNotRequired, QANotStarted, strings.TrimSpace(actor), now, derivativeID); err != nil {
		return fmt.Errorf("reset E2 conversion generation: %w", err)
	}
	return nil
}
