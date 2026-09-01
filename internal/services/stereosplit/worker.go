// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/logger"
)

const (
	stereoSplitVerificationWorkers = 2
	stereoSplitStatusSyncWorkers   = 8
	stereoSplitDispatchWorkers     = 4
	stereoSplitCleanupWorkers      = 2
	stereoSplitVerificationLease   = time.Hour
	stereoSplitStatusSyncLease     = 2 * time.Minute
	stereoSplitDispatchLease       = 2 * time.Minute
)

// StartVerificationWorkers starts the fixed-size output verification pool.
func (m *Manager) StartVerificationWorkers() error {
	if m == nil || m.db == nil || m.objects == nil {
		return fmt.Errorf("start stereo split verification workers: dependencies are not configured")
	}
	if !m.cfg.Enabled {
		return nil
	}
	m.verificationMu.Lock()
	defer m.verificationMu.Unlock()
	if m.verificationCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.verificationCancel = cancel
	m.verificationDone = done
	go m.runVerificationWorkers(ctx, done)
	return nil
}

// StopVerificationWorkers stops the verification pool and waits for workers to exit.
func (m *Manager) StopVerificationWorkers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.verificationMu.Lock()
	cancel := m.verificationCancel
	done := m.verificationDone
	m.verificationCancel = nil
	m.verificationDone = nil
	m.verificationMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runVerificationWorkers(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	var wg sync.WaitGroup
	for worker := 0; worker < stereoSplitVerificationWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			m.runVerificationWorker(ctx, workerID)
		}(worker)
	}
	wg.Wait()
}

func (m *Manager) runVerificationWorker(ctx context.Context, workerID int) {
	worker := fmt.Sprintf("verify-%d", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		id, ok, err := m.claimVerification(ctx, worker)
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] verification claim failed: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.pollInterval()):
			}
			continue
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.pollInterval()):
			}
			continue
		}
		if err := m.verifySucceeded(ctx, id); err != nil && ctx.Err() == nil {
			logger.Printf("[STEREO_SPLIT] verification worker=%s derivative=%d failed: %v", worker, id, err)
		}
	}
}

func (m *Manager) claimVerification(ctx context.Context, _ string) (int64, bool, error) {
	// reconcile_after doubles as a durable single-replica verification lease.
	// A long lease prevents a large calibration MCAP from being claimed twice;
	// an interrupted worker becomes eligible again after the lease expires.
	m.verificationClaim.Lock()
	defer m.verificationClaim.Unlock()
	var id int64
	err := m.db.GetContext(ctx, &id, `
		SELECT id FROM episode_derivatives
		WHERE kind = ? AND processing_status = ?
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
		ORDER BY updated_at ASC, id ASC LIMIT 1
	`, Kind, ProcessingVerifying, m.now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("select verification candidate: %w", err)
	}
	now := m.now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET reconcile_after = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND processing_status = ?
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
	`, now.Add(stereoSplitVerificationLease), now, id, Kind, ProcessingVerifying, now)
	if err != nil {
		return 0, false, fmt.Errorf("claim verification candidate: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("check verification claim: %w", err)
	}
	return id, rows == 1, nil
}

// StartDispatchWorkers starts the bounded queued-work dispatch pool.
func (m *Manager) StartDispatchWorkers() error {
	if m == nil || m.db == nil || m.orbit == nil || m.objects == nil {
		return fmt.Errorf("start stereo split dispatch workers: dependencies are not configured")
	}
	if !m.cfg.Enabled {
		return nil
	}
	m.dispatchRunMu.Lock()
	defer m.dispatchRunMu.Unlock()
	if m.dispatchCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.dispatchCancel = cancel
	m.dispatchDone = done
	m.dispatchClaimTTL = stereoSplitDispatchLease
	go m.runDispatchWorkers(ctx, done)
	return nil
}

// StopDispatchWorkers stops the queued-work dispatch pool.
func (m *Manager) StopDispatchWorkers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.dispatchRunMu.Lock()
	cancel := m.dispatchCancel
	done := m.dispatchDone
	m.dispatchCancel = nil
	m.dispatchDone = nil
	m.dispatchRunMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runDispatchWorkers(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	var wg sync.WaitGroup
	for worker := 0; worker < stereoSplitDispatchWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			m.runDispatchWorker(ctx, workerID)
		}(worker)
	}
	wg.Wait()
}

func (m *Manager) runDispatchWorker(ctx context.Context, workerID int) {
	worker := fmt.Sprintf("dispatch-%d", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		candidate, ok, err := m.claimDispatchCandidate(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] dispatch worker=%s claim failed: %v", worker, err)
			}
			m.waitStatusSync(ctx)
			continue
		}
		if !ok {
			m.waitStatusSync(ctx)
			continue
		}
		started := time.Now()
		if err := m.freezeQueued(ctx, candidate); err != nil {
			m.releaseDispatchInFlight()
			if errors.Is(err, errScratchStorageExceeded) {
				err = m.failBeforeSubmission(ctx, candidate.ID, err)
			} else {
				m.releaseDispatchClaim(ctx, candidate.ID, err)
			}
			if err != nil && ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] dispatch worker=%s derivative=%d prepare failed: %v", worker, candidate.ID, err)
			}
			continue
		}
		if err := m.reconcileSubmitting(ctx, candidate.ID); err != nil {
			m.releaseDispatchInFlight()
			if ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] dispatch worker=%s derivative=%d submit failed: %v", worker, candidate.ID, err)
			}
			continue
		}
		m.releaseDispatchInFlight()
		logger.Printf("[STEREO_SPLIT] dispatch worker=%s derivative=%d completed elapsed_ms=%d", worker, candidate.ID, time.Since(started).Milliseconds())
	}
}

func (m *Manager) claimDispatchCandidate(ctx context.Context) (frozenDerivativeRow, bool, error) {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()
	now := m.now().UTC()
	capacity, err := m.loadDispatchCapacity(ctx, false)
	if err != nil {
		return frozenDerivativeRow{}, false, err
	}
	if capacity.Active+m.dispatchInFlight >= capacity.Limit {
		return frozenDerivativeRow{}, false, nil
	}
	var candidate frozenDerivativeRow
	err = m.db.GetContext(ctx, &candidate, `
		SELECT id, episode_id, generation, processing_status,
		       cancel_requested_at,
		       orbit_submission_id, orbit_request, orbit_job_id,
		       submit_attempt_count, orbit_submit_absent_at, orbit_job_missing_since,
		       orbit_delete_status, qa_status
		FROM episode_derivatives
		WHERE kind = ? AND processing_status = ?
		  AND cancel_requested_at IS NULL
		  AND (reconcile_after IS NULL OR reconcile_after <= ? OR updated_at <= ?)
		ORDER BY updated_at ASC, id ASC
		LIMIT 1
	`, Kind, ProcessingQueued, now, now.Add(-m.dispatchClaimLease()))
	if errors.Is(err, sql.ErrNoRows) {
		return frozenDerivativeRow{}, false, nil
	}
	if err != nil {
		return frozenDerivativeRow{}, false, fmt.Errorf("select dispatch candidate: %w", err)
	}
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET reconcile_after = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND processing_status = ?
		  AND cancel_requested_at IS NULL
		  AND (reconcile_after IS NULL OR reconcile_after <= ? OR updated_at <= ?)
	`, now.Add(m.dispatchClaimLease()), now, candidate.ID, Kind, ProcessingQueued,
		now, now.Add(-m.dispatchClaimLease()))
	if err != nil {
		return frozenDerivativeRow{}, false, fmt.Errorf("claim dispatch candidate: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return frozenDerivativeRow{}, false, fmt.Errorf("check dispatch claim: %w", err)
	}
	if rows == 1 {
		m.dispatchInFlight++
	}
	return candidate, rows == 1, nil
}

func (m *Manager) releaseDispatchInFlight() {
	m.dispatchMu.Lock()
	if m.dispatchInFlight > 0 {
		m.dispatchInFlight--
	}
	m.dispatchMu.Unlock()
}

func (m *Manager) releaseDispatchClaim(ctx context.Context, derivativeID int64, cause error) {
	now := m.now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET reconcile_after = ?, processing_error = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND processing_status = ?
	`, now.Add(m.pollInterval()), cause.Error(), now, derivativeID, Kind, ProcessingQueued); err != nil && ctx.Err() == nil {
		logger.Printf("[STEREO_SPLIT] release dispatch claim derivative=%d failed: %v", derivativeID, err)
	}
}

func (m *Manager) dispatchClaimLease() time.Duration {
	if m.dispatchClaimTTL > 0 {
		return m.dispatchClaimTTL
	}
	return stereoSplitDispatchLease
}

// StartCleanupWorkers starts the bounded terminal Orbit cleanup pool.
func (m *Manager) StartCleanupWorkers() error {
	if m == nil || m.db == nil || m.orbit == nil {
		return fmt.Errorf("start stereo split cleanup workers: dependencies are not configured")
	}
	if !m.cfg.Enabled {
		return nil
	}
	m.cleanupMu.Lock()
	defer m.cleanupMu.Unlock()
	if m.cleanupCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cleanupCancel = cancel
	m.cleanupDone = done
	go m.runCleanupWorkers(ctx, done)
	return nil
}

// StopCleanupWorkers stops the terminal Orbit cleanup pool.
func (m *Manager) StopCleanupWorkers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.cleanupMu.Lock()
	cancel := m.cleanupCancel
	done := m.cleanupDone
	m.cleanupCancel = nil
	m.cleanupDone = nil
	m.cleanupMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runCleanupWorkers(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	var wg sync.WaitGroup
	for worker := 0; worker < stereoSplitCleanupWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			m.runCleanupWorker(ctx, workerID)
		}(worker)
	}
	wg.Wait()
}

func (m *Manager) runCleanupWorker(ctx context.Context, workerID int) {
	worker := fmt.Sprintf("cleanup-%d", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		id, ok, err := m.claimCleanupCandidate(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] cleanup worker=%s claim failed: %v", worker, err)
			}
			m.waitStatusSync(ctx)
			continue
		}
		if !ok {
			m.waitStatusSync(ctx)
			continue
		}
		if err := m.reconcileDelete(ctx, id); err != nil && ctx.Err() == nil {
			logger.Printf("[STEREO_SPLIT] cleanup worker=%s derivative=%d failed: %v", worker, id, err)
		}
	}
}

func (m *Manager) claimCleanupCandidate(ctx context.Context) (int64, bool, error) {
	m.cleanupClaim.Lock()
	defer m.cleanupClaim.Unlock()
	var id int64
	now := m.now().UTC()
	err := m.db.GetContext(ctx, &id, `
		SELECT id FROM episode_derivatives
		WHERE kind = ? AND orbit_delete_status = ?
		  AND (orbit_delete_next_retry_at IS NULL OR orbit_delete_next_retry_at <= ?)
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
		ORDER BY updated_at ASC, id ASC
		LIMIT 1
	`, Kind, DeletePending, now, now)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("select cleanup candidate: %w", err)
	}
	leaseUntil := now.Add(stereoSplitStatusSyncLease)
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives SET reconcile_after = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND orbit_delete_status = ?
		  AND (orbit_delete_next_retry_at IS NULL OR orbit_delete_next_retry_at <= ?)
		  AND (reconcile_after IS NULL OR reconcile_after <= ?)
	`, leaseUntil, now, id, Kind, DeletePending, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("claim cleanup candidate: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("check cleanup claim: %w", err)
	}
	return id, rows == 1, nil
}

// StartStatusSyncWorkers starts the bounded Orbit status polling pool. The
// pool owns non-cancelled pending/running records so terminal Orbit states can
// release dispatch capacity without waiting behind submission or cleanup work.
func (m *Manager) StartStatusSyncWorkers() error {
	if m == nil || m.db == nil || m.orbit == nil {
		return fmt.Errorf("start stereo split status sync workers: dependencies are not configured")
	}
	if !m.cfg.Enabled {
		return nil
	}
	m.statusSyncMu.Lock()
	defer m.statusSyncMu.Unlock()
	if m.statusSyncCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.statusSyncCancel = cancel
	m.statusSyncDone = done
	go m.runStatusSyncWorkers(ctx, done)
	return nil
}

// StopStatusSyncWorkers stops the Orbit status polling pool.
func (m *Manager) StopStatusSyncWorkers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.statusSyncMu.Lock()
	cancel := m.statusSyncCancel
	done := m.statusSyncDone
	m.statusSyncCancel = nil
	m.statusSyncDone = nil
	m.statusSyncMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runStatusSyncWorkers(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	var wg sync.WaitGroup
	for worker := 0; worker < stereoSplitStatusSyncWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			m.runStatusSyncWorker(ctx, workerID)
		}(worker)
	}
	wg.Wait()
}

func (m *Manager) runStatusSyncWorker(ctx context.Context, workerID int) {
	worker := fmt.Sprintf("status-%d", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		id, ok, err := m.claimStatusSyncCandidate(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("[STEREO_SPLIT] status sync worker=%s claim failed: %v", worker, err)
			}
			m.waitStatusSync(ctx)
			continue
		}
		if !ok {
			m.waitStatusSync(ctx)
			continue
		}
		if err := m.reconcileOrbitStatus(ctx, id); err != nil && ctx.Err() == nil {
			logger.Printf("[STEREO_SPLIT] status sync worker=%s derivative=%d failed: %v", worker, id, err)
		}
	}
}

func (m *Manager) waitStatusSync(ctx context.Context) {
	timer := time.NewTimer(m.pollInterval())
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (m *Manager) claimStatusSyncCandidate(ctx context.Context) (int64, bool, error) {
	m.statusSyncClaim.Lock()
	defer m.statusSyncClaim.Unlock()
	var id int64
	now := m.now().UTC()
	err := m.db.GetContext(ctx, &id, `
		SELECT id FROM episode_derivatives
		WHERE kind = ?
		  AND cancel_requested_at IS NULL
		  AND processing_status IN (?, ?)
		  AND (reconcile_after IS NULL OR reconcile_after <= ? OR updated_at <= ?)
		ORDER BY updated_at ASC, id ASC
		LIMIT 1
	`, Kind, ProcessingPending, ProcessingRunning, now, now.Add(-stereoSplitStatusSyncLease))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("select status sync candidate: %w", err)
	}
	leaseUntil := now.Add(stereoSplitStatusSyncLease)
	result, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET reconcile_after = ?, updated_at = ?
		WHERE id = ? AND kind = ? AND cancel_requested_at IS NULL
		  AND processing_status IN (?, ?)
		  AND (reconcile_after IS NULL OR reconcile_after <= ? OR updated_at <= ?)
	`, leaseUntil, now, id, Kind, ProcessingPending, ProcessingRunning, now, now.Add(-stereoSplitStatusSyncLease))
	if err != nil {
		return 0, false, fmt.Errorf("claim status sync candidate: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("check status sync claim: %w", err)
	}
	return id, rows == 1, nil
}

// StartReconciler starts the single-replica durable lifecycle loop. The loop
// drains ready work before sleeping so a large database queue does not add one
// poll interval of latency per Episode.
func (m *Manager) StartReconciler() error {
	if m == nil || m.db == nil {
		return fmt.Errorf("start stereo split reconciler: database is not configured")
	}
	if !m.cfg.Enabled {
		return nil
	}
	if m.orbit == nil || m.objects == nil {
		return fmt.Errorf("start stereo split reconciler: Orbit and TOS reader are required")
	}

	m.runnerMu.Lock()
	defer m.runnerMu.Unlock()
	if m.runnerCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.runnerCancel = cancel
	m.runnerDone = done
	go m.runReconciler(ctx, done)
	m.wakeReconciler()
	return nil
}

// StopReconciler asks the lifecycle loop to stop and waits for the current
// bounded Orbit/TOS call to return or for the shutdown context to expire.
func (m *Manager) StopReconciler(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.runnerMu.Lock()
	cancel := m.runnerCancel
	done := m.runnerDone
	m.runnerCancel = nil
	m.runnerDone = nil
	m.runnerMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runReconciler(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(m.pollInterval())
	defer ticker.Stop()

	for {
		worked, err := m.ReconcileOnce(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Printf("[STEREO_SPLIT] Reconcile failed: %v", err)
		}
		snapshotWorked, snapshotErr := m.FreezeBulkResultSnapshotsOnce(ctx)
		if snapshotErr != nil && ctx.Err() == nil {
			logger.Printf("[STEREO_SPLIT] Bulk result snapshot failed: %v", snapshotErr)
		}
		if ctx.Err() != nil {
			return
		}
		if (worked && err == nil) || (snapshotWorked && snapshotErr == nil) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *Manager) wakeReconciler() {
	if m == nil || m.wake == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
