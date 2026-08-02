// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type bulkRunItemRow struct {
	EpisodeID            int64          `db:"episode_id"`
	DerivativeID         sql.NullInt64  `db:"derivative_id"`
	DerivativeGeneration sql.NullInt64  `db:"derivative_generation"`
	AdmissionStatus      string         `db:"admission_status"`
	ResultReason         sql.NullString `db:"result_reason"`
}

// AdmitBulk materializes one bulk-run member and applies the same admission
// rules as the single-Episode endpoints in one database transaction.
func (m *Manager) AdmitBulk(ctx context.Context, runID string, episodeID int64, actor string) (BulkAdmission, error) {
	if m == nil || m.db == nil {
		return BulkAdmission{}, fmt.Errorf("admit stereo split bulk item: database is not configured")
	}
	if !m.cfg.Enabled {
		return BulkAdmission{}, ErrDisabled
	}
	runID = strings.TrimSpace(runID)
	if runID == "" || episodeID <= 0 {
		return BulkAdmission{}, fmt.Errorf("admit stereo split bulk item: invalid run or episode")
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return BulkAdmission{}, fmt.Errorf("begin stereo split bulk admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var run struct {
		Action string `db:"action"`
		Status string `db:"status"`
	}
	if err := tx.GetContext(ctx, &run, `
		SELECT action, status FROM bulk_runs WHERE run_id = ?`+forUpdateClause(m.db), runID); err != nil {
		return BulkAdmission{}, fmt.Errorf("load stereo split bulk run: %w", err)
	}
	if run.Action != Kind {
		return BulkAdmission{}, fmt.Errorf("bulk run %q action is %q, want %q", runID, run.Action, Kind)
	}
	if run.Status == "cancel_requested" || run.Status == "canceled" {
		if err := tx.Commit(); err != nil {
			return BulkAdmission{}, fmt.Errorf("commit canceled stereo split bulk admission: %w", err)
		}
		return BulkAdmission{
			EpisodeID:       episodeID,
			AdmissionStatus: BulkAdmissionCanceled,
			Reason:          BulkReasonCanceledBeforeAdmit,
		}, nil
	}

	var episode episodeAdmissionRow
	episodeErr := tx.GetContext(ctx, &episode, `
		SELECT id, storage_backend, mcap_path, metadata, cloud_publish_source
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL`+forUpdateClause(m.db), episodeID)
	if episodeErr != nil && !errors.Is(episodeErr, sql.ErrNoRows) {
		return BulkAdmission{}, fmt.Errorf("lock stereo split bulk episode: %w", episodeErr)
	}

	current, derivativeErr := getDerivativeTx(ctx, tx, episodeID)
	if derivativeErr != nil && !errors.Is(derivativeErr, sql.ErrNoRows) {
		return BulkAdmission{}, fmt.Errorf("load stereo split bulk derivative: %w", derivativeErr)
	}

	item, itemErr := getBulkRunItemTx(ctx, tx, runID, episodeID)
	if itemErr != nil && !errors.Is(itemErr, sql.ErrNoRows) {
		return BulkAdmission{}, fmt.Errorf("load stereo split bulk item: %w", itemErr)
	}
	if itemErr == nil && item.AdmissionStatus != BulkAdmissionPending {
		if err := advanceBulkCursorTx(ctx, tx, runID, episodeID, m.now().UTC()); err != nil {
			return BulkAdmission{}, err
		}
		if err := tx.Commit(); err != nil {
			return BulkAdmission{}, fmt.Errorf("commit idempotent stereo split bulk admission: %w", err)
		}
		return bulkAdmissionFromRow(item), nil
	}
	if errors.Is(itemErr, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO bulk_run_items (
				bulk_run_id, episode_id, admission_status, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?)
		`, runID, episodeID, BulkAdmissionPending, m.now().UTC(), m.now().UTC()); err != nil {
			return BulkAdmission{}, fmt.Errorf("insert stereo split bulk item: %w", err)
		}
	}

	finishSkipped := func(reason string) (BulkAdmission, error) {
		derivativeID := any(nil)
		generation := any(nil)
		if derivativeErr == nil {
			derivativeID = current.ID
			generation = current.Generation
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE bulk_run_items
			SET derivative_id = ?, derivative_generation = ?, admission_status = ?,
			    result_reason = ?, updated_at = ?
			WHERE bulk_run_id = ? AND episode_id = ? AND admission_status = ?
		`, derivativeID, generation, BulkAdmissionSkipped, reason, m.now().UTC(),
			runID, episodeID, BulkAdmissionPending); err != nil {
			return BulkAdmission{}, fmt.Errorf("skip stereo split bulk item: %w", err)
		}
		if err := advanceBulkCursorTx(ctx, tx, runID, episodeID, m.now().UTC()); err != nil {
			return BulkAdmission{}, err
		}
		if err := tx.Commit(); err != nil {
			return BulkAdmission{}, fmt.Errorf("commit skipped stereo split bulk item: %w", err)
		}
		return BulkAdmission{
			EpisodeID:            episodeID,
			DerivativeID:         nullInt64Value(derivativeID),
			DerivativeGeneration: nullIntValue(generation),
			AdmissionStatus:      BulkAdmissionSkipped,
			Reason:               reason,
		}, nil
	}

	// The decision order is stable and intentionally matches the design.
	if derivativeErr == nil && current.ProcessingStatus == ProcessingSucceeded {
		return finishSkipped(BulkReasonAlreadyDerived)
	}
	if errors.Is(episodeErr, sql.ErrNoRows) {
		return finishSkipped(BulkReasonSourceUnavailable)
	}
	if strings.EqualFold(strings.TrimSpace(episode.CloudPublishSource.String), CloudSourceOriginal) {
		return finishSkipped(BulkReasonCloudSourceLocked)
	}
	var syncEvidence int
	if err := tx.GetContext(ctx, &syncEvidence, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil {
		return BulkAdmission{}, fmt.Errorf("check stereo split bulk sync evidence: %w", err)
	}
	if syncEvidence > 0 && !strings.EqualFold(strings.TrimSpace(episode.CloudPublishSource.String), CloudSourceStereoSplit) {
		return finishSkipped(BulkReasonCloudSourceLocked)
	}
	if derivativeErr == nil {
		switch current.ProcessingStatus {
		case ProcessingQueued, ProcessingSubmitting, ProcessingPending, ProcessingRunning, ProcessingVerifying:
			return finishSkipped(BulkReasonProcessingActive)
		case ProcessingFailed, ProcessingCanceled:
			if current.OrbitDeleteStatus != DeleteCompleted && current.OrbitDeleteStatus != DeleteNotRequired {
				return finishSkipped(BulkReasonOrbitDeletePending)
			}
		}
	}
	if _, _, err := normalizeEpisodeSource(episode); err != nil {
		return finishSkipped(BulkReasonSourceUnavailable)
	}

	now := m.now().UTC()
	reason := BulkReasonEligible
	var admitted Derivative
	if errors.Is(derivativeErr, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO episode_derivatives (
				episode_id, kind, generation, processing_status, orbit_delete_status,
				qa_status, created_by, updated_by, created_at, updated_at
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
		`, episodeID, Kind, ProcessingQueued, DeleteNotRequired, QANotStarted,
			strings.TrimSpace(actor), strings.TrimSpace(actor), now, now)
		if err != nil {
			return BulkAdmission{}, fmt.Errorf("insert stereo split bulk derivative: %w", err)
		}
		derivativeID, err := result.LastInsertId()
		if err != nil {
			return BulkAdmission{}, fmt.Errorf("read stereo split bulk derivative id: %w", err)
		}
		admitted, err = getDerivativeByIDTx(ctx, tx, derivativeID)
		if err != nil {
			return BulkAdmission{}, fmt.Errorf("load stereo split bulk derivative: %w", err)
		}
	} else {
		if current.ProcessingStatus != ProcessingFailed && current.ProcessingStatus != ProcessingCanceled {
			return BulkAdmission{}, fmt.Errorf("unsupported stereo split bulk status %q", current.ProcessingStatus)
		}
		if err := freezeTerminalBulkItemsTx(ctx, tx, current); err != nil {
			return BulkAdmission{}, err
		}
		if err := resetDerivativeGenerationTx(ctx, tx, current.ID, actor, now); err != nil {
			return BulkAdmission{}, err
		}
		admitted, err = getDerivativeByIDTx(ctx, tx, current.ID)
		if err != nil {
			return BulkAdmission{}, fmt.Errorf("load retried stereo split bulk derivative: %w", err)
		}
		reason = BulkReasonEligibleRetry
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE bulk_run_items
		SET derivative_id = ?, derivative_generation = ?, admission_status = ?,
		    result_reason = ?, updated_at = ?
		WHERE bulk_run_id = ? AND episode_id = ? AND admission_status = ?
	`, admitted.ID, admitted.Generation, BulkAdmissionAdmitted, reason, now,
		runID, episodeID, BulkAdmissionPending); err != nil {
		return BulkAdmission{}, fmt.Errorf("admit stereo split bulk item: %w", err)
	}
	if err := advanceBulkCursorTx(ctx, tx, runID, episodeID, now); err != nil {
		return BulkAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return BulkAdmission{}, fmt.Errorf("commit stereo split bulk admission: %w", err)
	}
	m.wakeReconciler()
	return BulkAdmission{
		EpisodeID:            episodeID,
		DerivativeID:         admitted.ID,
		DerivativeGeneration: admitted.Generation,
		AdmissionStatus:      BulkAdmissionAdmitted,
		Reason:               reason,
	}, nil
}

func advanceBulkCursorTx(ctx context.Context, tx *sqlx.Tx, runID string, episodeID int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE bulk_runs
		SET materialize_cursor = CASE
		      WHEN materialize_cursor IS NULL OR materialize_cursor < ? THEN ?
		      ELSE materialize_cursor
		    END,
		    updated_at = ?
		WHERE run_id = ? AND action = ?
	`, episodeID, episodeID, now, runID, Kind); err != nil {
		return fmt.Errorf("advance stereo split bulk cursor: %w", err)
	}
	return nil
}

func getBulkRunItemTx(ctx context.Context, tx *sqlx.Tx, runID string, episodeID int64) (bulkRunItemRow, error) {
	var item bulkRunItemRow
	err := tx.GetContext(ctx, &item, `
		SELECT episode_id, derivative_id, derivative_generation, admission_status, result_reason
		FROM bulk_run_items WHERE bulk_run_id = ? AND episode_id = ?`+forUpdateClause(txDB(tx)), runID, episodeID)
	return item, err
}

// txDB is intentionally nil: rows are already serialized by the Episode lock,
// and sqlite tests do not support FOR UPDATE. MySQL obtains the item lock via
// the subsequent conditional UPDATE.
func txDB(_ *sqlx.Tx) *sqlx.DB { return nil }

func bulkAdmissionFromRow(row bulkRunItemRow) BulkAdmission {
	return BulkAdmission{
		EpisodeID:            row.EpisodeID,
		DerivativeID:         row.DerivativeID.Int64,
		DerivativeGeneration: int(row.DerivativeGeneration.Int64),
		AdmissionStatus:      row.AdmissionStatus,
		Reason:               row.ResultReason.String,
	}
}

func nullInt64Value(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func nullIntValue(value any) int {
	return int(nullInt64Value(value))
}

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
		return fmt.Errorf("reset stereo split generation: %w", err)
	}
	return nil
}

func freezeTerminalBulkItemsTx(ctx context.Context, tx *sqlx.Tx, derivative Derivative) error {
	if !derivativeReadyForBulkSnapshot(derivative) {
		return fmt.Errorf("stereo split generation %d is not ready for bulk result snapshot", derivative.Generation)
	}
	snapshot, err := json.Marshal(map[string]any{
		"derivative_id":       derivative.ID,
		"episode_id":          derivative.EpisodeID,
		"generation":          derivative.Generation,
		"processing_status":   derivative.ProcessingStatus,
		"qa_status":           derivative.QAStatus,
		"orbit_delete_status": derivative.OrbitDeleteStatus,
		"processing_error":    derivative.ProcessingError,
		"qa_error":            derivative.QAError,
		"mcap_path":           derivative.McapPath,
		"checksum":            derivative.Checksum,
		"file_size_bytes":     derivative.FileSizeBytes,
	})
	if err != nil {
		return fmt.Errorf("encode stereo split bulk result snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE bulk_run_items
		SET result_snapshot = ?, updated_at = ?
		WHERE derivative_id = ? AND derivative_generation = ?
		  AND admission_status = ? AND result_snapshot IS NULL
	`, string(snapshot), time.Now().UTC(), derivative.ID, derivative.Generation, BulkAdmissionAdmitted); err != nil {
		return fmt.Errorf("freeze stereo split bulk result snapshot: %w", err)
	}
	return nil
}

func derivativeReadyForBulkSnapshot(derivative Derivative) bool {
	deleteDone := derivative.OrbitDeleteStatus == DeleteCompleted || derivative.OrbitDeleteStatus == DeleteNotRequired
	if !deleteDone {
		return false
	}
	switch derivative.ProcessingStatus {
	case ProcessingFailed, ProcessingCanceled:
		return true
	case ProcessingSucceeded:
		return derivative.QAStatus == QAApproved || derivative.QAStatus == QAFailed
	default:
		return false
	}
}

// FreezeBulkResultSnapshotsOnce freezes one completed generation into every
// bulk item that admitted it. A later retry can then safely overwrite the
// single current derivative row without changing historical batch results.
func (m *Manager) FreezeBulkResultSnapshotsOnce(ctx context.Context) (bool, error) {
	if m == nil || m.db == nil {
		return false, fmt.Errorf("freeze stereo split bulk snapshots: database is not configured")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin stereo split bulk snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var derivativeID int64
	err = tx.GetContext(ctx, &derivativeID, `
		SELECT d.id
		FROM bulk_run_items i
		INNER JOIN episode_derivatives d
		  ON d.id = i.derivative_id AND d.generation = i.derivative_generation
		WHERE i.admission_status = ? AND i.result_snapshot IS NULL
		  AND d.orbit_delete_status IN (?, ?)
		  AND (
		    d.processing_status IN (?, ?)
		    OR (d.processing_status = ? AND d.qa_status IN (?, ?))
		  )
		ORDER BY i.id ASC
		LIMIT 1
	`, BulkAdmissionAdmitted, DeleteNotRequired, DeleteCompleted,
		ProcessingFailed, ProcessingCanceled, ProcessingSucceeded, QAApproved, QAFailed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("select stereo split bulk snapshot candidate: %w", err)
	}
	derivative, err := getDerivativeByIDTx(ctx, tx, derivativeID)
	if err != nil {
		return false, fmt.Errorf("load stereo split bulk snapshot derivative: %w", err)
	}
	if err := freezeTerminalBulkItemsTx(ctx, tx, derivative); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit stereo split bulk snapshot: %w", err)
	}
	return true, nil
}
