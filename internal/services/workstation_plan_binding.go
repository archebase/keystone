// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const planBindingRequestTimeout = 15 * time.Second

type workstationPlanBindingScope struct {
	WorkstationID int64
	RobotID       int64
	WorkspaceID   int64
	OperatorID    string
}

// WorkstationPlanBinder executes device-triggered plan binding in one in-memory worker.
// A binding request is intentionally not durable; callers can enqueue it again after a process restart.
type WorkstationPlanBinder struct {
	db      *sqlx.DB
	hilbert HilbertDCPlanBinder
	now     func() time.Time
	wake    chan struct{}

	mu      sync.Mutex
	pending map[int64]workstationPlanBindingScope
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewWorkstationPlanBinder creates the device-triggered plan binding service.
func NewWorkstationPlanBinder(db *sqlx.DB, hilbert HilbertDCPlanBinder) *WorkstationPlanBinder {
	return &WorkstationPlanBinder{
		db:      db,
		hilbert: hilbert,
		now:     func() time.Time { return time.Now().UTC() },
		wake:    make(chan struct{}, 1),
		pending: make(map[int64]workstationPlanBindingScope),
	}
}

// EnqueueUnboundPlans validates and coalesces one workstation scope, then wakes the worker.
// It returns without calling Hilbert; repeated requests for the same workstation are coalesced.
func (s *WorkstationPlanBinder) EnqueueUnboundPlans(
	ctx context.Context,
	workstationID, robotID, workspaceID int64,
	operatorID string,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workstation plan binding is unavailable")
	}
	if workstationID <= 0 || robotID <= 0 || workspaceID <= 0 || strings.TrimSpace(operatorID) == "" {
		return fmt.Errorf("invalid workstation plan binding scope")
	}
	deviceIDText, current, err := s.workstationDevice(ctx, workstationID, robotID, workspaceID)
	if err != nil {
		return err
	}
	if !current {
		return fmt.Errorf("workstation session is no longer current")
	}
	if deviceID, err := strconv.ParseInt(strings.TrimSpace(deviceIDText), 10, 64); err != nil || deviceID <= 0 {
		logger.Printf("[DC_PLAN_BINDING] skip workstation=%d: invalid robot device_id=%q", workstationID, deviceIDText)
		return nil
	}

	s.mu.Lock()
	s.pending[workstationID] = workstationPlanBindingScope{
		WorkstationID: workstationID,
		RobotID:       robotID,
		WorkspaceID:   workspaceID,
		OperatorID:    strings.TrimSpace(operatorID),
	}
	s.mu.Unlock()
	s.wakeWorker()
	return nil
}

// Start launches the event-driven plan binding worker.
func (s *WorkstationPlanBinder) Start() error {
	if s == nil || s.db == nil || s.hilbert == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.running = true
	go s.run(ctx, s.done)
	return nil
}

// Stop stops the background plan binding worker.
func (s *WorkstationPlanBinder) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *WorkstationPlanBinder) wakeWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *WorkstationPlanBinder) run(ctx context.Context, done chan<- struct{}) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		close(done)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			scope, ok := s.takePendingScope()
			if !ok {
				break
			}
			s.bindScope(ctx, scope)
		}
	}
}

func (s *WorkstationPlanBinder) takePendingScope() (workstationPlanBindingScope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for workstationID, scope := range s.pending {
		delete(s.pending, workstationID)
		return scope, true
	}
	return workstationPlanBindingScope{}, false
}

func (s *WorkstationPlanBinder) bindScope(ctx context.Context, scope workstationPlanBindingScope) {
	deviceIDText, current, err := s.workstationDevice(ctx, scope.WorkstationID, scope.RobotID, scope.WorkspaceID)
	if err != nil || !current {
		logger.Printf("[DC_PLAN_BINDING] skip workstation=%d: session unavailable: %v", scope.WorkstationID, err)
		return
	}
	deviceID, err := strconv.ParseInt(strings.TrimSpace(deviceIDText), 10, 64)
	if err != nil || deviceID <= 0 {
		logger.Printf("[DC_PLAN_BINDING] skip workstation=%d: invalid robot device_id=%q", scope.WorkstationID, deviceIDText)
		return
	}

	var planIDs []int64
	if err := s.db.SelectContext(ctx, &planIDs, `
		SELECT id FROM dc_plan
		WHERE workspace_id = ? AND operator = ?
			AND dc_device_id IS NULL
			AND COALESCE(status, 'pending_collection') <> 'collected'
			AND deleted_at IS NULL
		ORDER BY id
	`, scope.WorkspaceID, scope.OperatorID); err != nil {
		logger.Printf("[DC_PLAN_BINDING] query failed: workstation=%d error=%v", scope.WorkstationID, err)
		return
	}
	for _, planID := range planIDs {
		if ctx.Err() != nil {
			return
		}
		if _, current, err := s.workstationDevice(ctx, scope.WorkstationID, scope.RobotID, scope.WorkspaceID); err != nil || !current {
			logger.Printf("[DC_PLAN_BINDING] stop workstation=%d: session is no longer current", scope.WorkstationID)
			return
		}
		if err := s.bindOneWithTimeout(ctx, scope, deviceID, planID); err != nil {
			logger.Printf("[DC_PLAN_BINDING] plan binding failed: workstation=%d workspace=%d operator=%s plan=%d device=%d error=%v", scope.WorkstationID, scope.WorkspaceID, scope.OperatorID, planID, deviceID, err)
		}
	}
}

func (s *WorkstationPlanBinder) bindOneWithTimeout(
	ctx context.Context,
	scope workstationPlanBindingScope,
	deviceID, planID int64,
) error {
	bindCtx, cancel := context.WithTimeout(ctx, planBindingRequestTimeout)
	defer cancel()
	return s.bindOne(bindCtx, scope, deviceID, planID)
}

func (s *WorkstationPlanBinder) bindOne(ctx context.Context, scope workstationPlanBindingScope, deviceID, planID int64) error {
	var plan struct {
		CurrentDeviceID sql.NullInt64 `db:"dc_device_id"`
		Status          string        `db:"status"`
		Operator        string        `db:"operator"`
	}
	if err := s.db.GetContext(ctx, &plan, `
		SELECT dc_device_id, COALESCE(status, 'pending_collection') AS status, operator
		FROM dc_plan WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL LIMIT 1
	`, planID, scope.WorkspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("query plan: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(plan.Operator), scope.OperatorID) || strings.EqualFold(strings.TrimSpace(plan.Status), "collected") {
		return nil
	}
	if plan.CurrentDeviceID.Valid {
		if plan.CurrentDeviceID.Int64 == deviceID {
			return nil
		}
		return fmt.Errorf("plan already bound to device %d", plan.CurrentDeviceID.Int64)
	}
	bound, err := s.hilbert.PatchDCPlanDCDeviceID(ctx, scope.WorkspaceID, planID, deviceID)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("hilbert did not confirm device binding")
	}
	deviceName, err := ResolveDCPlanDeviceName(ctx, s.db, scope.WorkspaceID, deviceID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE dc_plan SET dc_device_id = ?, dc_device_name = ?, local_updated_at = ?
		WHERE id = ? AND workspace_id = ? AND operator = ?
			AND dc_device_id IS NULL
			AND COALESCE(status, 'pending_collection') <> 'collected'
			AND deleted_at IS NULL
	`, deviceID, deviceName, s.now(), planID, scope.WorkspaceID, scope.OperatorID)
	if err != nil {
		return fmt.Errorf("update local plan projection: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		var current sql.NullInt64
		if queryErr := s.db.GetContext(
			ctx,
			&current,
			`SELECT dc_device_id FROM dc_plan WHERE id = ?`,
			planID,
		); queryErr != nil || !current.Valid || current.Int64 != deviceID {
			return fmt.Errorf("local plan projection was changed concurrently")
		}
	}
	return nil
}

func (s *WorkstationPlanBinder) workstationDevice(ctx context.Context, workstationID, robotID, workspaceID int64) (string, bool, error) {
	var row struct {
		DeviceID string `db:"device_id"`
		Current  bool   `db:"is_current"`
	}
	query := `
		SELECT r.device_id, ws.is_current
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE ws.id = ? AND ws.workspace_id = ? AND ws.deleted_at IS NULL AND r.status = 'active'`
	args := []any{workstationID, workspaceID}
	if robotID > 0 {
		query += " AND ws.robot_id = ?"
		args = append(args, robotID)
	}
	query += " LIMIT 1"
	if err := s.db.GetContext(ctx, &row, query, args...); err != nil {
		return "", false, fmt.Errorf("query workstation binding device: %w", err)
	}
	return row.DeviceID, row.Current, nil
}
