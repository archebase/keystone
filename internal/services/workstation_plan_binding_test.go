// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type testWorkstationPlanHilbertBinder struct {
	failedPlanID int64
	calls        []int64
	called       chan int64
}

func (b *testWorkstationPlanHilbertBinder) PatchDCPlanDCDeviceID(_ context.Context, _, planID, _ int64) (bool, error) {
	b.calls = append(b.calls, planID)
	if b.called != nil {
		b.called <- planID
	}
	if planID == b.failedPlanID {
		return false, errors.New("Hilbert unavailable")
	}
	return true, nil
}

func testWorkstationPlanScope() workstationPlanBindingScope {
	return workstationPlanBindingScope{WorkstationID: 11, RobotID: 101, WorkspaceID: 10, OperatorID: "alice"}
}

func TestWorkstationPlanBinderBindsOnlyCurrentWorkstationScope(t *testing.T) {
	db := newWorkstationPlanBindingTestDB(t)
	defer db.Close()
	seedWorkstationPlanBindingScope(t, db)
	hilbert := &testWorkstationPlanHilbertBinder{}
	binder := NewWorkstationPlanBinder(db, hilbert)
	binder.bindScope(context.Background(), testWorkstationPlanScope())
	if len(hilbert.calls) != 1 || hilbert.calls[0] != 1001 {
		t.Fatalf("Hilbert calls = %v, want [1001]", hilbert.calls)
	}
	var deviceID int64
	if err := db.Get(&deviceID, `SELECT dc_device_id FROM dc_plan WHERE id = 1001`); err != nil {
		t.Fatalf("query bound plan: %v", err)
	}
	if deviceID != 501 {
		t.Fatalf("dc_device_id = %d, want 501", deviceID)
	}
}

func TestWorkstationPlanBinderContinuesAfterPlanFailure(t *testing.T) {
	db := newWorkstationPlanBindingTestDB(t)
	defer db.Close()
	seedWorkstationPlanBindingScope(t, db)
	if _, err := db.Exec(`INSERT INTO dc_plan (id, workspace_id, operator, status) VALUES (1002, 10, 'alice', 'pending_collection')`); err != nil {
		t.Fatalf("seed second plan: %v", err)
	}
	hilbert := &testWorkstationPlanHilbertBinder{failedPlanID: 1001}
	binder := NewWorkstationPlanBinder(db, hilbert)
	binder.bindScope(context.Background(), testWorkstationPlanScope())
	if len(hilbert.calls) != 2 {
		t.Fatalf("Hilbert calls = %v, want two attempts", hilbert.calls)
	}
	var deviceID int64
	if err := db.Get(&deviceID, `SELECT dc_device_id FROM dc_plan WHERE id = 1002`); err != nil {
		t.Fatalf("query second plan: %v", err)
	}
	if deviceID != 501 {
		t.Fatalf("second plan dc_device_id = %d, want 501", deviceID)
	}
}

func TestWorkstationPlanBinderSkipsInvalidHilbertDeviceID(t *testing.T) {
	db := newWorkstationPlanBindingTestDB(t)
	defer db.Close()
	seedWorkstationPlanBindingScope(t, db)
	if _, err := db.Exec(`UPDATE robots SET device_id = 'not-numeric' WHERE id = 101`); err != nil {
		t.Fatalf("set invalid device id: %v", err)
	}
	hilbert := &testWorkstationPlanHilbertBinder{}
	binder := NewWorkstationPlanBinder(db, hilbert)
	binder.bindScope(context.Background(), testWorkstationPlanScope())
	if len(hilbert.calls) != 0 {
		t.Fatalf("Hilbert calls = %v, want none", hilbert.calls)
	}
}

func TestWorkstationPlanBinderWorkerProcessesEnqueuedScope(t *testing.T) {
	db := newWorkstationPlanBindingTestDB(t)
	defer db.Close()
	seedWorkstationPlanBindingScope(t, db)
	hilbert := &testWorkstationPlanHilbertBinder{called: make(chan int64, 1)}
	binder := NewWorkstationPlanBinder(db, hilbert)
	if err := binder.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := binder.Stop(ctx); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	}()
	if err := binder.EnqueueUnboundPlans(context.Background(), 11, 101, 10, "alice"); err != nil {
		t.Fatalf("EnqueueUnboundPlans() error = %v", err)
	}
	select {
	case planID := <-hilbert.called:
		if planID != 1001 {
			t.Fatalf("planID = %d, want 1001", planID)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not process enqueued scope")
	}
}

func newWorkstationPlanBindingTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE data_collectors (id INTEGER PRIMARY KEY, name TEXT NOT NULL, operator_id TEXT NOT NULL, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE robots (id INTEGER PRIMARY KEY, device_id TEXT NOT NULL, workspace_id INTEGER NOT NULL, device_name TEXT, device_type TEXT, status TEXT NOT NULL, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE workstations (id INTEGER PRIMARY KEY, robot_id INTEGER NOT NULL, robot_name TEXT, robot_serial TEXT, data_collector_id INTEGER NOT NULL, collector_name TEXT, collector_operator_id TEXT, workspace_id INTEGER NOT NULL, name TEXT, status TEXT, metadata TEXT, created_at TIMESTAMP, updated_at TIMESTAMP, is_current BOOLEAN NOT NULL, superseded_at TIMESTAMP NULL, deleted_at TIMESTAMP NULL)`,
		`CREATE TABLE dc_plan (id INTEGER PRIMARY KEY, workspace_id INTEGER NOT NULL, operator TEXT NOT NULL, status TEXT, dc_device_id INTEGER, dc_device_name TEXT, local_updated_at TIMESTAMP NULL, deleted_at TIMESTAMP NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func seedWorkstationPlanBindingScope(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO data_collectors (id, name, operator_id) VALUES (7, 'Alice', 'alice');
		INSERT INTO robots (id, device_id, workspace_id, device_name, device_type, status) VALUES (101, '501', 10, 'E2-501', 'Ego Portal E2', 'active');
		INSERT INTO workstations (id, robot_id, data_collector_id, collector_operator_id, workspace_id, is_current) VALUES (11, 101, 7, 'alice', 10, TRUE);
		INSERT INTO dc_plan (id, workspace_id, operator, status, dc_device_id) VALUES
			(1001, 10, 'alice', 'pending_collection', NULL),
			(2001, 20, 'alice', 'pending_collection', NULL),
			(1003, 10, 'bob', 'pending_collection', NULL),
			(1004, 10, 'alice', 'collected', NULL),
			(1005, 10, 'alice', 'pending_collection', 999)
	`); err != nil {
		t.Fatalf("seed binding scope: %v", err)
	}
}
