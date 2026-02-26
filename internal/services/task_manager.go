// services/task_manager.go - Task management service
package services

import (
	"log"
	"sync"
)

// TaskManager task manager
type TaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

// Task represents a task
type Task struct {
	ID           string
	BatchID      string
	SceneID      string
	WorkstationID string
	RobotID      string
	OperatorID   string
	Status       string // pending, in_progress, completed, failed
	Priority     int
}

// NewTaskManager creates a new task manager
func NewTaskManager() *TaskManager {
	tm := &TaskManager{
		tasks: make(map[string]*Task),
	}
	log.Println("[SERVICE] TaskManager initialized")
	return tm
}

// Create creates a new task
func (tm *TaskManager) Create(task *Task) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.tasks[task.ID]; exists {
		return ErrTaskAlreadyExists
	}

	tm.tasks[task.ID] = task
	log.Printf("[TASK-MANAGER] Created task: %s", task.ID)
	return nil
}

// Get retrieves a task by ID
func (tm *TaskManager) Get(id string) (*Task, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	task, exists := tm.tasks[id]
	if !exists {
		return nil, ErrTaskNotFound
	}
	return task, nil
}

// List returns all tasks
func (tm *TaskManager) List() []*Task {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	tasks := make([]*Task, 0, len(tm.tasks))
	for _, task := range tm.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// UpdateStatus updates the status of a task
func (tm *TaskManager) UpdateStatus(id, status string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, exists := tm.tasks[id]
	if !exists {
		return ErrTaskNotFound
	}

	oldStatus := task.Status
	task.Status = status
	log.Printf("[TASK-MANAGER] Task %s status: %s -> %s", id, oldStatus, status)
	return nil
}

// Error definitions
var (
	ErrTaskAlreadyExists = &TaskError{Code: "TASK_EXISTS", Message: "task already exists"}
	ErrTaskNotFound      = &TaskError{Code: "TASK_NOT_FOUND", Message: "task not found"}
)

// TaskError represents a task-related error
type TaskError struct {
	Code    string
	Message string
}

func (e *TaskError) Error() string {
	return e.Message
}
