// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package autosync

import (
	"context"
	"fmt"
	"time"

	"archebase.com/keystone-edge/internal/logger"
)

// StartReconciler starts the durable automatic processing loop. Downstream
// concurrency remains owned by the stereo-split manager and Sync Worker.
func (m *Manager) StartReconciler() error {
	if m == nil || m.db == nil {
		return fmt.Errorf("start auto sync reconciler: database is not configured")
	}
	if m.qa == nil || m.cloud == nil {
		return fmt.Errorf("start auto sync reconciler: QA and cloud sync are required")
	}

	m.runnerMu.Lock()
	defer m.runnerMu.Unlock()
	if m.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	go m.runReconciler(ctx, done)
	m.wakeWorker()
	return nil
}

// StopReconciler stops the automatic processing loop and waits for its current
// bounded database or enqueue operation to finish.
func (m *Manager) StopReconciler(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.runnerMu.Lock()
	cancel := m.cancel
	done := m.done
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

func (m *Manager) runReconciler(ctx context.Context, done chan struct{}) {
	defer func() {
		m.runnerMu.Lock()
		if m.done == done {
			m.cancel = nil
			m.done = nil
		}
		m.runnerMu.Unlock()
		close(done)
	}()
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		if _, err := m.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("[AUTO_SYNC] Reconcile failed: %v", err)
		}
	}
}
