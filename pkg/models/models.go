// models/models.go - 数据模型定义
package models

import "time"

// Task 任务模型
type Task struct {
	ID          string    `json:"id" db:"id"`
	BatchID     string    `json:"batch_id" db:"batch_id"`
	SceneID     string    `json:"scene_id" db:"scene_id"`
	WorkstationID string  `json:"workstation_id" db:"workstation_id"`
	RobotID     string    `json:"robot_id" db:"robot_id"`
	OperatorID  string    `json:"operator_id" db:"operator_id"`
	Status      string    `json:"status" db:"status"` // pending, in_progress, completed, failed
	Priority    int       `json:"priority" db:"priority"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// Episode Episode 模型
type Episode struct {
	ID            string    `json:"id" db:"id"`
	TaskID        string    `json:"task_id" db:"task_id"`
	BatchID       string    `json:"batch_id" db:"batch_id"`
	SceneID       string    `json:"scene_id" db:"scene_id"`
	RobotID       string    `json:"robot_id" db:"robot_id"`
	StartTime     time.Time `json:"start_time" db:"start_time"`
	EndTime       time.Time `json:"end_time" db:"end_time"`
	FileSizeBytes int64     `json:"file_size_bytes" db:"file_size_bytes"`
	FilePath      string    `json:"file_path" db:"file_path"`        // S3 路径
	SidecarPath   string    `json:"sidecar_path" db:"sidecar_path"`  // 元数据路径
	QAStatus      string    `json:"qa_status" db:"qa_status"`        // pending, approved, rejected
	QAScore       float64   `json:"qa_score" db:"qa_score"`
	QAComment     string    `json:"qa_comment" db:"qa_comment"`
	CloudSynced   bool      `json:"cloud_synced" db:"cloud_synced"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

// Workstation 工作站模型
type Workstation struct {
	ID           string    `json:"id" db:"id"`
	Name         string    `json:"name" db:"name"`
	Location     string    `json:"location" db:"location"`
	Status       string    `json:"status" db:"status"` // online, offline, maintenance
	CurrentRobotID string  `json:"current_robot_id" db:"current_robot_id"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// Scene 场景模型
type Scene struct {
	ID          string    `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description" db:"description"`
	Version     int       `json:"version" db:"version"`
	Active      bool      `json:"active" db:"active"`
	SyncedAt    time.Time `json:"synced_at" db:"synced_at"`
}

// HealthResponse 健康检查响应
type HealthResponse struct {
	Status    string                    `json:"status"`
	Timestamp string                    `json:"timestamp"`
	Components map[string]ComponentHealth `json:"components"`
}

// ComponentHealth 组件健康状态
type ComponentHealth struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
