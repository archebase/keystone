// services/task_manager_test.go - Task manager tests
package services

import (
	"fmt"
	"sync"
	"testing"
)

func TestNewTaskManager(t *testing.T) {
	tm := NewTaskManager()

	if tm == nil {
		t.Fatal("NewTaskManager() returned nil")
	}

	if tm.tasks == nil {
		t.Error("NewTaskManager().tasks not initialized")
	}

	// Should be empty list
	tasks := tm.List()
	if len(tasks) != 0 {
		t.Errorf("NewTaskManager().List() = %v, want empty", tasks)
	}
}

func TestTaskManagerCreate(t *testing.T) {
	tm := NewTaskManager()

	task := &Task{
		ID:            "task-001",
		BatchID:       "batch-001",
		SceneID:       "scene-001",
		WorkstationID: "ws-001",
		RobotID:       "robot-001",
		OperatorID:    "operator-001",
		Status:        "pending",
		Priority:      1,
	}

	err := tm.Create(task)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Verify task was created
	got, err := tm.Get("task-001")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.ID != task.ID {
		t.Errorf("Get().ID = %v, want %v", got.ID, task.ID)
	}

	if got.Status != task.Status {
		t.Errorf("Get().Status = %v, want %v", got.Status, task.Status)
	}
}

func TestTaskManagerCreateDuplicate(t *testing.T) {
	tm := NewTaskManager()

	task := &Task{
		ID:      "task-001",
		BatchID: "batch-001",
		SceneID: "scene-001",
		RobotID: "robot-001",
		Status:  "pending",
	}

	// First create should succeed
	err := tm.Create(task)
	if err != nil {
		t.Fatalf("First Create() error = %v", err)
	}

	// Second create should fail
	err = tm.Create(task)
	if err != ErrTaskAlreadyExists {
		t.Errorf("Duplicate Create() error = %v, want %v", err, ErrTaskAlreadyExists)
	}
}

func TestTaskManagerGet(t *testing.T) {
	tm := NewTaskManager()

	// Get non-existent task
	_, err := tm.Get("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("Get() non-existent task error = %v, want %v", err, ErrTaskNotFound)
	}

	// Create task then get
	task := &Task{
		ID:      "task-001",
		BatchID: "batch-001",
		SceneID: "scene-001",
		RobotID: "robot-001",
		Status:  "pending",
	}
	tm.Create(task)

	got, err := tm.Get("task-001")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.ID != "task-001" {
		t.Errorf("Get().ID = %v, want task-001", got.ID)
	}
}

func TestTaskManagerList(t *testing.T) {
	tm := NewTaskManager()

	// Empty list
	tasks := tm.List()
	if len(tasks) != 0 {
		t.Errorf("List() = %v, want empty", tasks)
	}

	// Add a few tasks
	for i := 1; i <= 3; i++ {
		task := &Task{
			ID:      fmt.Sprintf("task-%03d", i),
			BatchID: "batch-001",
			SceneID: "scene-001",
			RobotID: "robot-001",
			Status:  "pending",
		}
		tm.Create(task)
	}

	tasks = tm.List()
	if len(tasks) != 3 {
		t.Errorf("List() length = %v, want 3", len(tasks))
	}
}

func TestTaskManagerUpdateStatus(t *testing.T) {
	tm := NewTaskManager()

	// Create task
	task := &Task{
		ID:      "task-001",
		BatchID: "batch-001",
		SceneID: "scene-001",
		RobotID: "robot-001",
		Status:  "pending",
	}
	tm.Create(task)

	// Update status
	err := tm.UpdateStatus("task-001", "in_progress")
	if err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	// Verify status updated
	got, _ := tm.Get("task-001")
	if got.Status != "in_progress" {
		t.Errorf("UpdateStatus() then Get().Status = %v, want in_progress", got.Status)
	}

	// Continue update to completed
	tm.UpdateStatus("task-001", "completed")
	got, _ = tm.Get("task-001")
	if got.Status != "completed" {
		t.Errorf("UpdateStatus() then Get().Status = %v, want completed", got.Status)
	}
}

func TestTaskManagerUpdateStatusNotFound(t *testing.T) {
	tm := NewTaskManager()

	err := tm.UpdateStatus("nonexistent", "in_progress")
	if err != ErrTaskNotFound {
		t.Errorf("UpdateStatus() non-existent task error = %v, want %v", err, ErrTaskNotFound)
	}
}

func TestTaskManagerConcurrent(t *testing.T) {
	tm := NewTaskManager()
	const goroutines = 50
	const tasksPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Concurrent create tasks
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < tasksPerGoroutine; j++ {
				taskID := fmt.Sprintf("task-%d-%d", idx, j)
				task := &Task{
					ID:      taskID,
					BatchID: "batch-001",
					SceneID: "scene-001",
					RobotID: "robot-001",
					Status:  "pending",
				}
				tm.Create(task)
			}
		}(i)
	}

	wg.Wait()

	// Verify task count
	tasks := tm.List()
	expectedTasks := goroutines * tasksPerGoroutine
	if len(tasks) != expectedTasks {
		t.Errorf("Concurrent create List() length = %v, want %v", len(tasks), expectedTasks)
	}
}

func TestTaskError(t *testing.T) {
	tests := []struct {
		name    string
		err     *TaskError
		wantMsg string
	}{
		{
			name:    "Task already exists error",
			err:     ErrTaskAlreadyExists,
			wantMsg: "task already exists",
		},
		{
			name:    "Task not found error",
			err:     ErrTaskNotFound,
			wantMsg: "task not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Error() != tt.wantMsg {
				t.Errorf("TaskError.Error() = %v, want %v", tt.err.Error(), tt.wantMsg)
			}
		})
	}
}
