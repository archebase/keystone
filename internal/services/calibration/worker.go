// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"fmt"
	"time"

	"archebase.com/keystone-edge/internal/logger"
)

// StartReconciler starts the background durable state reconciler when Orbit is configured.
func (m *Manager) StartReconciler() error {
	if m == nil || m.orbit == nil || m.objects == nil {
		return nil
	}
	if m.db == nil {
		return fmt.Errorf("calibration reconciler dependencies are incomplete")
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

// StopReconciler stops the background reconciler and waits for it to exit.
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
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
		for {
			worked, err := m.ReconcileOnce(ctx)
			if err != nil {
				logger.Printf("[CALIBRATION] Reconcile failed: %v", err)
			}
			if !worked || err != nil {
				break
			}
		}
	}
}
