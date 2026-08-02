// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"fmt"
	"time"

	"archebase.com/keystone-edge/internal/logger"
)

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
