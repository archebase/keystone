// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

func validateTaskGroupUniqueness(taskGroups []TaskGroupItem) (dupA int, dupB int, ok bool) {
	seen := make(map[string]int, len(taskGroups))
	for i, tg := range taskGroups {
		if tg.SOPID <= 0 || tg.SubsceneID <= 0 {
			continue
		}
		key := fmt.Sprintf("%d_%d", tg.SOPID, tg.SubsceneID)
		if prev, exists := seen[key]; exists {
			return prev, i, true
		}
		seen[key] = i
	}
	return 0, 0, false
}

// BatchHandler handles batch-related HTTP requests.
type BatchHandler struct {
	db          *sqlx.DB
	recorderHub *services.RecorderHub
	// recorderRPCTimeout controls how long we wait for recorder RPC responses
	// when batch cancellation/recall triggers device-side clear (ready) or cancel (in_progress).
	recorderRPCTimeout time.Duration
}

type taskDeviceRow struct {
	TaskID   string `db:"task_id"`
	DeviceID string `db:"device_id"`
	Status   string `db:"status"`
}

// taskDeviceBatchRow is used when collecting recorder notify targets grouped by batch.
type taskDeviceBatchRow struct {
	BatchID  int64  `db:"batch_id"`
	TaskID   string `db:"task_id"`
	DeviceID string `db:"device_id"`
	Status   string `db:"status"`
}

// orderCompletionRecorderNotify groups Axon recorder RPC targets per batch after order completion.
type orderCompletionRecorderNotify struct {
	BatchID int64
	Rows    []taskDeviceRow
}

// NewBatchHandler creates a new BatchHandler.
func NewBatchHandler(db *sqlx.DB, recorderHub *services.RecorderHub, recorderRPCTimeout time.Duration) *BatchHandler {
	return &BatchHandler{db: db, recorderHub: recorderHub, recorderRPCTimeout: recorderRPCTimeout}
}

// RegisterRoutes registers batch routes under the provided router group.
func (h *BatchHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/batches", h.ListBatches)
	apiV1.POST("/batches", h.CreateBatch)
	apiV1.GET("/batches/:id", h.GetBatch)
	apiV1.DELETE("/batches/:id", h.DeleteBatch)
	apiV1.PATCH("/batches/:id", h.PatchBatch)
	apiV1.POST("/batches/:id/tasks", h.AdjustBatchTasks)
	apiV1.POST("/batches/:id/recall", h.RecallBatch)
	apiV1.GET("/batches/:id/tasks", h.ListBatchTasks)
}

// recalledEpisodeLabel is appended to episodes.labels (JSON string array) when a batch is recalled (see RecallBatch).
const recalledEpisodeLabel = "recalled_batch"

// batchAdvanceTriggerStatuses lists task statuses that trigger tryAdvanceBatchStatus after a task
// update via PUT /tasks. Tasks become cancelled only via PATCH batch cancel, which does not invoke
// that hook; transfer completion sets completed and also calls tryAdvanceBatchStatus.
var batchAdvanceTriggerStatuses = map[string]struct{}{
	"completed": {},
	"failed":    {},
}

var validBatchStatuses = map[string]struct{}{
	"pending":   {},
	"active":    {},
	"completed": {},
	"cancelled": {},
	"recalled":  {},
}

// BatchListItem represents a batch item in list responses.
type BatchListItem struct {
	ID               string `json:"id" db:"id"`
	BatchID          string `json:"batch_id" db:"batch_id"`
	OrderID          string `json:"order_id" db:"order_id"`
	WorkstationID    string `json:"workstation_id" db:"workstation_id"`
	OrganizationID   string `json:"organization_id" db:"organization_id"`
	OrganizationName string `json:"organization_name,omitempty" db:"organization_name"`
	Name             string `json:"name" db:"name"`
	Notes            string `json:"notes,omitempty" db:"notes"`
	Status           string `json:"status" db:"status"`
	CompletedCount   int    `json:"completed_count" db:"completed_count"`
	TaskCount        int    `json:"task_count" db:"task_count"`
	CancelledCount   int    `json:"cancelled_count" db:"cancelled_count"`
	FailedCount      int    `json:"failed_count" db:"failed_count"`
	EpisodeCount     int    `json:"episode_count" db:"episode_count"`
	StartedAt        string `json:"started_at,omitempty"`
	EndedAt          string `json:"ended_at,omitempty"`
	Metadata         any    `json:"metadata,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

// ListBatchesResponse represents the response body for listing batches.
type ListBatchesResponse struct {
	Items   []BatchListItem `json:"items"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	HasNext bool            `json:"hasNext,omitempty"`
	HasPrev bool            `json:"hasPrev,omitempty"`
}

type batchRow struct {
	ID               int64          `db:"id"`
	BatchID          string         `db:"batch_id"`
	OrderID          int64          `db:"order_id"`
	WorkstationID    int64          `db:"workstation_id"`
	OrganizationID   int64          `db:"organization_id"`
	OrganizationName sql.NullString `db:"organization_name"`
	Name             sql.NullString `db:"name"`
	Notes            sql.NullString `db:"notes"`
	Status           string         `db:"status"`
	CompletedCount   int            `db:"completed_count"`
	TaskCount        int            `db:"task_count"`
	CancelledCount   int            `db:"cancelled_count"`
	FailedCount      int            `db:"failed_count"`
	EpisodeCount     int            `db:"episode_count"`
	StartedAt        sql.NullTime   `db:"started_at"`
	EndedAt          sql.NullTime   `db:"ended_at"`
	Metadata         sql.NullString `db:"metadata"`
	CreatedAt        sql.NullTime   `db:"created_at"`
	UpdatedAt        sql.NullTime   `db:"updated_at"`
}

func parseNullableJSON(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	raw := strings.TrimSpace(v.String)
	if raw == "" || raw == "null" {
		return nil
	}

	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func batchListItemFromRow(r batchRow) BatchListItem {
	notes := ""
	if r.Notes.Valid {
		notes = r.Notes.String
	}
	orgName := ""
	if r.OrganizationName.Valid {
		orgName = r.OrganizationName.String
	}

	startedAt := ""
	if r.StartedAt.Valid {
		startedAt = r.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	endedAt := ""
	if r.EndedAt.Valid {
		endedAt = r.EndedAt.Time.UTC().Format(time.RFC3339)
	}

	createdAt := ""
	if r.CreatedAt.Valid {
		createdAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	nameOut := ""
	if r.Name.Valid {
		nameOut = r.Name.String
	}
	return BatchListItem{
		ID:               fmt.Sprintf("%d", r.ID),
		BatchID:          r.BatchID,
		OrderID:          fmt.Sprintf("%d", r.OrderID),
		WorkstationID:    fmt.Sprintf("%d", r.WorkstationID),
		OrganizationID:   fmt.Sprintf("%d", r.OrganizationID),
		OrganizationName: orgName,
		Name:             nameOut,
		Notes:            notes,
		Status:           r.Status,
		CompletedCount:   r.CompletedCount,
		TaskCount:        r.TaskCount,
		CancelledCount:   r.CancelledCount,
		FailedCount:      r.FailedCount,
		EpisodeCount:     r.EpisodeCount,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		Metadata:         parseNullableJSON(r.Metadata),
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}
}

// ListBatches handles batch listing requests with optional filtering.
//
// @Summary      List batches
// @Description  Lists batches with optional order/workstation/status filters
// @Tags         batches
// @Produce      json
// @Param        order_id       query     string  false  "Filter by order ID"
// @Param        workstation_id query     string  false  "Filter by workstation ID"
// @Param        organization_id query    string  false  "Filter by organization ID"
// @Param        device_id      query     string  false  "Filter by robot device ID"
// @Param        collector_operator_id query string false "Filter by collector operator ID"
// @Param        scene_id        query     string  false  "Filter by scene ID (derived from order)"
// @Param        status         query     string  false  "Filter by status"
// @Param        limit          query     int     false  "Max results"       default(50)
// @Param        offset         query     int     false  "Pagination offset" default(0)
// @Success      200            {object}  ListBatchesResponse
// @Failure      400            {object}  map[string]string
// @Failure      500            {object}  map[string]string
// @Router       /batches [get]
func (h *BatchHandler) ListBatches(c *gin.Context) {
	const defaultLimit = 50

	orderID := strings.TrimSpace(c.Query("order_id"))
	workstationID := strings.TrimSpace(c.Query("workstation_id"))
	orgID := strings.TrimSpace(c.Query("organization_id"))
	deviceID := strings.TrimSpace(c.Query("device_id"))
	collectorOpID := strings.TrimSpace(c.Query("collector_operator_id"))
	sceneID := strings.TrimSpace(c.Query("scene_id"))
	status := strings.TrimSpace(c.Query("status"))

	limit := defaultLimit
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = parsedLimit
	}

	offset := 0
	if rawOffset := strings.TrimSpace(c.Query("offset")); rawOffset != "" {
		parsedOffset, err := strconv.Atoi(rawOffset)
		if err != nil || parsedOffset < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be a non-negative integer"})
			return
		}
		offset = parsedOffset
	}

	if status != "" {
		if _, ok := validBatchStatuses[status]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
	}

	conditions := []string{"b.deleted_at IS NULL"}
	args := make([]interface{}, 0, 6)

	if orderID != "" {
		conditions = append(conditions, "CAST(b.order_id AS CHAR) = ?")
		args = append(args, orderID)
	}
	if workstationID != "" {
		conditions = append(conditions, "CAST(b.workstation_id AS CHAR) = ?")
		args = append(args, workstationID)
	}
	if orgID != "" {
		conditions = append(conditions, "CAST(b.organization_id AS CHAR) = ?")
		args = append(args, orgID)
	}
	if status != "" {
		conditions = append(conditions, "b.status = ?")
		args = append(args, status)
	}
	if deviceID != "" {
		// robot_serial is denormalized from robots.device_id on workstations.
		conditions = append(conditions, "ws.robot_serial = ?")
		args = append(args, deviceID)
	}
	if collectorOpID != "" {
		// collector_operator_id is denormalized from data_collectors.operator_id on workstations.
		conditions = append(conditions, "ws.collector_operator_id = ?")
		args = append(args, collectorOpID)
	}
	if sceneID != "" {
		conditions = append(conditions, "CAST(o.scene_id AS CHAR) = ?")
		args = append(args, sceneID)
	}

	whereClause := strings.Join(conditions, " AND ")

	var total int
	countQuery := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM batches b
		LEFT JOIN workstations ws ON ws.id = b.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN orders o ON o.id = b.order_id AND o.deleted_at IS NULL
		WHERE %s
	`, whereClause)
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[BATCH] Failed to count batches: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list batches"})
		return
	}

	query := fmt.Sprintf(`
		SELECT
			b.id,
			b.batch_id,
			b.order_id,
			b.workstation_id,
			b.organization_id,
			org.name AS organization_name,
			b.name,
			b.notes,
			b.status,
			COALESCE(tc.task_count, 0) AS task_count,
			COALESCE(tc.completed_count, 0) AS completed_count,
			COALESCE(tc.cancelled_count, 0) AS cancelled_count,
			COALESCE(tc.failed_count, 0) AS failed_count,
			COALESCE(b.episode_count, 0) AS episode_count,
			b.started_at,
			b.ended_at,
			CAST(b.metadata AS CHAR) AS metadata,
			b.created_at,
			b.updated_at
		FROM batches b
		LEFT JOIN workstations ws ON ws.id = b.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN organizations org ON org.id = b.organization_id AND org.deleted_at IS NULL
		LEFT JOIN orders o ON o.id = b.order_id AND o.deleted_at IS NULL
		LEFT JOIN (
			SELECT
				batch_id,
				COUNT(*) AS task_count,
				COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0) AS completed_count,
				COALESCE(SUM(CASE WHEN status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled_count,
				COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS failed_count
			FROM tasks
			WHERE deleted_at IS NULL
			GROUP BY batch_id
		) tc ON tc.batch_id = b.id
		WHERE %s
		ORDER BY b.id DESC
		LIMIT ? OFFSET ?
	`, whereClause)

	queryArgs := append(append([]interface{}{}, args...), limit, offset)

	var rows []batchRow
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[BATCH] Failed to query batches: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list batches"})
		return
	}

	batches := make([]BatchListItem, 0, len(rows))
	for _, r := range rows {
		batches = append(batches, batchListItemFromRow(r))
	}

	hasNext := (offset + limit) < total
	hasPrev := offset > 0

	c.JSON(http.StatusOK, ListBatchesResponse{
		Items:   batches,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// GetBatch returns a single batch by its numeric ID.
//
// @Summary      Get batch
// @Description  Returns a single batch by id
// @Tags         batches
// @Produce      json
// @Param        id   path      int  true  "Batch ID"
// @Success      200  {object}  BatchListItem
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /batches/{id} [get]
func (h *BatchHandler) GetBatch(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	query := `
		SELECT
			b.id,
			b.batch_id,
			b.order_id,
			b.workstation_id,
			b.organization_id,
			org.name AS organization_name,
			b.name,
			b.notes,
			b.status,
			COALESCE(tc.task_count, 0) AS task_count,
			COALESCE(tc.completed_count, 0) AS completed_count,
			COALESCE(tc.cancelled_count, 0) AS cancelled_count,
			COALESCE(tc.failed_count, 0) AS failed_count,
			COALESCE(b.episode_count, 0) AS episode_count,
			b.started_at,
			b.ended_at,
			CAST(b.metadata AS CHAR) AS metadata,
			b.created_at,
			b.updated_at
		FROM batches b
		LEFT JOIN organizations org ON org.id = b.organization_id AND org.deleted_at IS NULL
		LEFT JOIN (
			SELECT
				batch_id,
				COUNT(*) AS task_count,
				COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0) AS completed_count,
				COALESCE(SUM(CASE WHEN status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled_count,
				COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS failed_count
			FROM tasks
			WHERE deleted_at IS NULL
			GROUP BY batch_id
		) tc ON tc.batch_id = b.id
		WHERE b.id = ? AND b.deleted_at IS NULL
	`

	var r batchRow
	if err := h.db.Get(&r, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to query batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get batch"})
		return
	}

	c.JSON(http.StatusOK, batchListItemFromRow(r))
}

// DeleteBatch handles batch deletion requests (soft delete).
// Only batches with status "cancelled" or "pending" can be targeted.
// If the batch has any completed task, the batch row is not deleted: only non-completed tasks are soft-deleted.
// If after that all remaining tasks are completed, the batch status is set to completed (from pending/cancelled).
// Otherwise the batch and all its tasks are soft-deleted.
//
// @Summary      Delete batch
// @Description  Soft-deletes cancelled or pending batch when it has no completed tasks; if it has completed tasks, only non-completed tasks are removed and the batch may be advanced to completed when appropriate.
// @Tags         batches
// @Accept       json
// @Produce      json
// @Param        id path     int  true  "Batch ID"
// @Success      200  {object}  map[string]interface{}  "batch_deleted (bool), tasks_removed (int)"
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /batches/{id} [delete]
func (h *BatchHandler) DeleteBatch(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	var status string
	if err := h.db.Get(&status, "SELECT status FROM batches WHERE id = ? AND deleted_at IS NULL", id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to query batch status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
		return
	}

	if status != "cancelled" && status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "batch can only be deleted when status is cancelled or pending"})
		return
	}

	var completedCount int
	if err := h.db.Get(&completedCount, "SELECT COUNT(*) FROM tasks WHERE batch_id = ? AND deleted_at IS NULL AND status = 'completed'", id); err != nil {
		logger.Printf("[BATCH] Failed to count completed tasks for batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
		return
	}

	now := time.Now().UTC()
	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] Failed to begin transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var tasksRemoved int64
	outBatchStatus := status

	if completedCount > 0 {
		res, err := tx.Exec("UPDATE tasks SET deleted_at = ?, updated_at = ? WHERE batch_id = ? AND deleted_at IS NULL AND status <> 'completed'", now, now, id)
		if err != nil {
			logger.Printf("[BATCH] Failed to soft delete non-completed batch tasks: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
			return
		}
		tasksRemoved, _ = res.RowsAffected()

		var remNonCompleted, remCompleted int
		if err := tx.Get(&remNonCompleted, "SELECT COUNT(*) FROM tasks WHERE batch_id = ? AND deleted_at IS NULL AND status <> 'completed'", id); err != nil {
			logger.Printf("[BATCH] Failed to count remaining non-completed tasks: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
			return
		}
		if err := tx.Get(&remCompleted, "SELECT COUNT(*) FROM tasks WHERE batch_id = ? AND deleted_at IS NULL AND status = 'completed'", id); err != nil {
			logger.Printf("[BATCH] Failed to count remaining completed tasks: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
			return
		}
		if remCompleted > 0 && remNonCompleted == 0 {
			if _, err := tx.Exec(`UPDATE batches SET status = 'completed', ended_at = COALESCE(ended_at, ?), updated_at = ? WHERE id = ? AND deleted_at IS NULL AND status IN ('pending', 'cancelled')`, now, now, id); err != nil {
				logger.Printf("[BATCH] Failed to advance batch to completed after cleanup: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
				return
			}
			outBatchStatus = "completed"
		}
	} else {
		if _, err := tx.Exec("UPDATE batches SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id); err != nil {
			logger.Printf("[BATCH] Failed to delete batch: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
			return
		}
		res, err := tx.Exec("UPDATE tasks SET deleted_at = ?, updated_at = ? WHERE batch_id = ? AND deleted_at IS NULL", now, now, id)
		if err != nil {
			logger.Printf("[BATCH] Failed to soft delete batch tasks: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
			return
		}
		tasksRemoved, _ = res.RowsAffected()
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] Failed to commit delete batch transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
		return
	}

	resp := gin.H{
		"batch_deleted":   completedCount == 0,
		"tasks_removed":   tasksRemoved,
		"completed_tasks": completedCount,
	}
	if completedCount > 0 {
		resp["batch_status"] = outBatchStatus
	}
	c.JSON(http.StatusOK, resp)
}

// PatchBatchRequest is the request body for patching a batch.
type PatchBatchRequest struct {
	Status string `json:"status"`
}

// PatchBatchResponse is the response body for patching a batch.
type PatchBatchResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// PatchBatch handles batch status updates.
// Only supports cancellation transitions:
// - pending -> cancelled
// - active  -> cancelled
//
// Note: pending->active is automatic (triggered when a task reaches completed or failed).
// Note: active->completed is automatic (when all non-deleted tasks are completed, failed, or cancelled).
// Note: For recall, use POST /batches/:id/recall.
//
// @Summary      Patch batch
// @Description  Updates batch status. Only cancellation transitions are allowed via PATCH.
// @Tags         batches
// @Accept       json
// @Produce      json
// @Param        id   path      int               true  "Batch ID"
// @Param        body body      PatchBatchRequest true  "Patch batch status"
// @Success      200  {object}  PatchBatchResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /batches/{id} [patch]
func (h *BatchHandler) PatchBatch(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	var req PatchBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status is required"})
		return
	}

	// Only cancellation is allowed via PATCH.
	// pending->active is automatic; active->completed is automatic; recall uses POST .../recall.
	if req.Status != "cancelled" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "PATCH only supports transitioning to 'cancelled'; use POST /batches/:id/recall for recall"})
		return
	}

	type statusRow struct {
		Status    string       `db:"status"`
		StartedAt sql.NullTime `db:"started_at"`
		UpdatedAt sql.NullTime `db:"updated_at"`
	}
	var cur statusRow
	if err := h.db.Get(&cur, "SELECT status, started_at, updated_at FROM batches WHERE id = ? AND deleted_at IS NULL", id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to query batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}

	// Only pending and active batches can be cancelled.
	if cur.Status != "pending" && cur.Status != "active" {
		c.JSON(http.StatusConflict, gin.H{
			"error":          fmt.Sprintf("cannot cancel batch in status '%s'; only pending or active batches can be cancelled", cur.Status),
			"current_status": cur.Status,
		})
		return
	}

	now := time.Now().UTC()
	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] Failed to begin transaction for patch batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// If cancelling a pending/active batch, we must also notify Axon Recorder for tasks that are already
	// configured (ready) or recording (in_progress): clear vs cancel respectively.
	// Collect task->device mapping before we mutate task statuses.
	toNotify := make([]taskDeviceRow, 0)
	if cur.Status == "pending" || cur.Status == "active" {
		if err := tx.Select(&toNotify, `
			SELECT
				t.task_id AS task_id,
				r.device_id AS device_id,
				t.status AS status
			FROM tasks t
			JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
			JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
			WHERE t.batch_id = ? AND t.deleted_at IS NULL
			  AND t.status IN ('ready', 'in_progress')
		`, id); err != nil {
			logger.Printf("[BATCH] Failed to query tasks for recorder notify (batch=%d): %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
			return
		}
	}

	// Update batch status (idempotent; only transitions to cancelled).
	if _, err := tx.Exec(
		"UPDATE batches SET status = 'cancelled', updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		now, id,
	); err != nil {
		logger.Printf("[BATCH] Failed to patch batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}

	// If cancelling a pending/active batch, also cancel its pending/ready/in_progress tasks.
	// This prevents orphan runnable tasks under a cancelled batch.
	if cur.Status == "pending" || cur.Status == "active" {
		if _, err := tx.Exec(
			`UPDATE tasks
			 SET status = 'cancelled', updated_at = ?
			 WHERE batch_id = ? AND deleted_at IS NULL
			   AND status IN ('pending', 'ready', 'in_progress')`,
			now, id,
		); err != nil {
			logger.Printf("[BATCH] Failed to cascade cancel tasks for batch %d: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
			return
		}
	}

	var patchWsID int64
	if err := tx.Get(&patchWsID, "SELECT workstation_id FROM batches WHERE id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[BATCH] Failed to read workstation_id for batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}
	if err := syncWorkstationStatusFromBatchesTx(tx, patchWsID); err != nil {
		logger.Printf("[BATCH] Failed to sync workstation status after patch batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] Failed to commit patch batch transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}

	// Best-effort: after commit, notify recorder devices (clear for ready, cancel for in_progress).
	// Notification failures should not affect the batch cancellation result.
	if (cur.Status == "pending" || cur.Status == "active") && h.recorderHub != nil && len(toNotify) > 0 {
		go h.notifyRecorderCancelTasks(context.Background(), id, toNotify)
	}

	startedAtOut := ""
	if cur.StartedAt.Valid {
		startedAtOut = cur.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, PatchBatchResponse{
		ID:        fmt.Sprintf("%d", id),
		Status:    "cancelled",
		StartedAt: startedAtOut,
		UpdatedAt: now.Format(time.RFC3339),
	})
}

func (h *BatchHandler) notifyRecorderCancelTasks(ctx context.Context, batchID int64, rows []taskDeviceRow) {
	notifyRecorderCancelTasksWithHub(ctx, h.recorderHub, h.recorderRPCTimeout, batchID, rows)
}

// notifyRecorderCancelTasksWithHub sends clear (ready) / cancel (in_progress) RPCs to Axon recorder.
// hub may be nil (no-op). Used by PATCH batch cancel and order-completion batch finalization.
func notifyRecorderCancelTasksWithHub(ctx context.Context, hub *services.RecorderHub, rpcTimeout time.Duration, batchID int64, rows []taskDeviceRow) {
	if hub == nil || len(rows) == 0 {
		return
	}
	timeout := rpcTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for _, r := range rows {
		deviceID := strings.TrimSpace(r.DeviceID)
		taskID := strings.TrimSpace(r.TaskID)
		if deviceID == "" || taskID == "" {
			continue
		}
		st := strings.TrimSpace(r.Status)
		var err error
		switch st {
		case "ready":
			// READY on device: clear cached config without treating it as an active recording cancel.
			_, err = hub.SendRPC(ctx, deviceID, "clear", nil, timeout)
		case "in_progress":
			_, err = hub.SendRPC(ctx, deviceID, "cancel", map[string]interface{}{"task_id": taskID}, timeout)
		default:
			continue
		}
		if err != nil {
			logger.Printf("[BATCH] Batch %d: failed to notify recorder (status=%s): device=%s task=%s err=%v", batchID, st, deviceID, taskID, err)
		}
	}
}

// TaskGroupItem represents a single task group in a batch creation/adjustment request.
type TaskGroupItem struct {
	SOPID      int64 `json:"sop_id"`
	SubsceneID int64 `json:"subscene_id"`
	Quantity   int   `json:"quantity"`
}

// CreateBatchRequest is the request body for creating a batch with tasks.
type CreateBatchRequest struct {
	OrderID       int64           `json:"order_id"`
	WorkstationID int64           `json:"workstation_id"`
	Notes         string          `json:"notes,omitempty"`
	TaskGroups    []TaskGroupItem `json:"task_groups"`
	Metadata      json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// CreatedTaskItem represents a single created task in the response.
type CreatedTaskItem struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	SOPID      string `json:"sop_id"`
	SubsceneID string `json:"subscene_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// CreateBatchResponse is the response body for creating a batch.
type CreateBatchResponse struct {
	Batch BatchListItem     `json:"batch"`
	Tasks []CreatedTaskItem `json:"tasks"`
}

// CreateBatch creates a new batch and its tasks in a single transaction.
//
// @Summary      Create batch with tasks
// @Description  Creates a batch and all its tasks atomically. task_groups defines how many tasks per SOP/subscene combination.
// @Tags         batches
// @Accept       json
// @Produce      json
// @Param        body body      CreateBatchRequest true  "Batch creation payload"
// @Success      201  {object}  CreateBatchResponse
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /batches [post]
func (h *BatchHandler) CreateBatch(c *gin.Context) {
	var req CreateBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	if req.OrderID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id is required"})
		return
	}
	if req.WorkstationID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workstation_id is required"})
		return
	}
	if len(req.TaskGroups) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_groups must not be empty"})
		return
	}

	// Validate task_groups
	totalQuantity := 0
	for i, tg := range req.TaskGroups {
		if tg.SOPID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task_groups[%d].sop_id is required", i)})
			return
		}
		if tg.SubsceneID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task_groups[%d].subscene_id is required", i)})
			return
		}
		if tg.Quantity < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task_groups[%d].quantity must be >= 1", i)})
			return
		}
		totalQuantity += tg.Quantity
	}
	if a, b, dup := validateTaskGroupUniqueness(req.TaskGroups); dup {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("duplicate task_groups entries: task_groups[%d] and task_groups[%d] have the same sop_id and subscene_id", a, b),
		})
		return
	}
	if totalQuantity > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "total quantity across all task_groups must be <= 1000"})
		return
	}

	now := time.Now().UTC()

	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] Failed to start transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Lock order and validate quota
	type orderQuotaRow struct {
		TargetCount    int   `db:"target_count"`
		OrganizationID int64 `db:"organization_id"`
	}
	var orderQuota orderQuotaRow
	if err := tx.Get(&orderQuota, "SELECT target_count, organization_id FROM orders WHERE id = ? AND deleted_at IS NULL LIMIT 1 FOR UPDATE", req.OrderID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("order not found: %d", req.OrderID)})
			return
		}
		logger.Printf("[BATCH] Failed to lock order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	// Count existing tasks for this order
	var existingCompletedCount int
	if err := tx.Get(&existingCompletedCount, "SELECT COUNT(*) FROM tasks WHERE order_id = ? AND deleted_at IS NULL AND status = 'completed'", req.OrderID); err != nil {
		logger.Printf("[BATCH] Failed to count completed tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	remaining := orderQuota.TargetCount - existingCompletedCount
	if totalQuantity > remaining {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":           fmt.Sprintf("quota exceeded: target_count=%d, completed_count=%d, remaining=%d, requested=%d", orderQuota.TargetCount, existingCompletedCount, remaining, totalQuantity),
			"target_count":    orderQuota.TargetCount,
			"completed_count": existingCompletedCount,
			"remaining":       remaining,
			"requested":       totalQuantity,
		})
		return
	}

	// Validate workstation
	type wsRow struct {
		ID             int64 `db:"id"`
		FactoryID      int64 `db:"factory_id"`
		OrganizationID int64 `db:"organization_id"`
	}
	var ws wsRow
	if err := tx.Get(&ws, "SELECT id, factory_id, organization_id FROM workstations WHERE id = ? AND deleted_at IS NULL LIMIT 1", req.WorkstationID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("workstation not found: %d", req.WorkstationID)})
			return
		}
		logger.Printf("[BATCH] Failed to validate workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	// Enforce tenant isolation: workstation must belong to the same organization as the order.
	if ws.OrganizationID != orderQuota.OrganizationID {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf(
				"workstation %d belongs to organization %d but order %d belongs to organization %d",
				req.WorkstationID, ws.OrganizationID, req.OrderID, orderQuota.OrganizationID,
			),
		})
		return
	}

	// Persist organization_id on batches for filtering (derived from order).
	organizationID := orderQuota.OrganizationID

	batchName := ""
	// Generate batch_id (unique even under bulk creates)
	batchIDStr, err := newPublicBatchID(now, 0)
	if err != nil {
		logger.Printf("[BATCH] Failed to generate batch_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}
	nameArg := sql.NullString{}
	var taskBatchName any

	// Handle metadata
	var metadataStr sql.NullString
	if len(req.Metadata) > 0 {
		raw := strings.TrimSpace(string(req.Metadata))
		if raw != "" && raw != "null" {
			metadataStr = sql.NullString{String: raw, Valid: true}
		}
	}

	// Handle notes
	var notesStr sql.NullString
	if notes := strings.TrimSpace(req.Notes); notes != "" {
		notesStr = sql.NullString{String: notes, Valid: true}
	}

	// Insert batch
	res, err := tx.Exec(
		`INSERT INTO batches (batch_id, order_id, workstation_id, organization_id, name, notes, status, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		batchIDStr, req.OrderID, req.WorkstationID, organizationID, nameArg, notesStr, metadataStr, now, now,
	)
	if err != nil {
		logger.Printf("[BATCH] Failed to insert batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}
	newBatchID, err := res.LastInsertId()
	if err != nil {
		logger.Printf("[BATCH] Failed to get batch insert id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	// Insert tasks for each task group
	createdTasks := make([]CreatedTaskItem, 0, totalQuantity)
	seqOffset := 0
	for _, tg := range req.TaskGroups {
		// Validate SOP
		if err := tx.Get(new(int), "SELECT 1 FROM sops WHERE id = ? AND deleted_at IS NULL LIMIT 1", tg.SOPID); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("sop not found: %d", tg.SOPID)})
				return
			}
			logger.Printf("[BATCH] Failed to validate sop_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
			return
		}

		// Validate subscene and get scene info
		type subsceneRow struct {
			ID      int64  `db:"id"`
			SceneID int64  `db:"scene_id"`
			Scene   string `db:"scene_name"`
			Name    string `db:"name"`
			Layout  string `db:"initial_scene_layout"`
		}
		var subscene subsceneRow
		if err := tx.Get(&subscene, `
			SELECT ss.id, ss.scene_id, s.name AS scene_name, ss.name,
			       COALESCE(ss.initial_scene_layout, '') AS initial_scene_layout
			FROM subscenes ss
			JOIN scenes s ON s.id = ss.scene_id AND s.deleted_at IS NULL
			WHERE ss.id = ? AND ss.deleted_at IS NULL
			LIMIT 1`, tg.SubsceneID); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("subscene not found: %d", tg.SubsceneID)})
				return
			}
			logger.Printf("[BATCH] Failed to validate subscene_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
			return
		}

		for i := 0; i < tg.Quantity; i++ {
			taskID, err := newPublicTaskID(now, seqOffset)
			if err != nil {
				logger.Printf("[BATCH] Failed to generate task_id: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
				return
			}
			seqOffset++

			resTask, err := tx.Exec(
				`INSERT INTO tasks (
					task_id, batch_id, order_id, sop_id, workstation_id,
					scene_id, subscene_id, batch_name, scene_name, subscene_name,
					factory_id, organization_id, initial_scene_layout,
					status, assigned_at, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
				taskID, newBatchID, req.OrderID, tg.SOPID, req.WorkstationID,
				subscene.SceneID, tg.SubsceneID, taskBatchName, subscene.Scene, subscene.Name,
				ws.FactoryID, organizationID, subscene.Layout,
				now, now, now,
			)
			if err != nil {
				logger.Printf("[BATCH] Failed to insert task: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
				return
			}
			newTaskID, err := resTask.LastInsertId()
			if err != nil {
				logger.Printf("[BATCH] Failed to get task insert id: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
				return
			}
			createdTasks = append(createdTasks, CreatedTaskItem{
				ID:         fmt.Sprintf("%d", newTaskID),
				TaskID:     taskID,
				SOPID:      fmt.Sprintf("%d", tg.SOPID),
				SubsceneID: fmt.Sprintf("%d", tg.SubsceneID),
				Status:     "pending",
				CreatedAt:  now.Format(time.RFC3339),
			})
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	c.JSON(http.StatusCreated, CreateBatchResponse{
		Batch: BatchListItem{
			ID:             fmt.Sprintf("%d", newBatchID),
			BatchID:        batchIDStr,
			OrderID:        fmt.Sprintf("%d", req.OrderID),
			WorkstationID:  fmt.Sprintf("%d", req.WorkstationID),
			Name:           batchName,
			Status:         "pending",
			CompletedCount: 0,
			TaskCount:      totalQuantity,
			FailedCount:    0,
			EpisodeCount:   0,
			CreatedAt:      now.Format(time.RFC3339),
			UpdatedAt:      now.Format(time.RFC3339),
		},
		Tasks: createdTasks,
	})
}

// AdjustBatchTasksRequest is the request body for adjusting batch tasks declaratively.
type AdjustBatchTasksRequest struct {
	TaskGroups []TaskGroupItem `json:"task_groups"`
}

// AdjustBatchTasksResponse is the response body for adjusting batch tasks.
type AdjustBatchTasksResponse struct {
	CreatedTasks   []CreatedTaskItem `json:"created_tasks"`
	DeletedTaskIDs []string          `json:"deleted_task_ids"`
}

// AdjustBatchTasks handles declarative task quantity adjustment for a batch.
// Each task_group entry specifies the TARGET quantity (not a delta) for that (sop_id, subscene_id) combination.
//
// @Summary      Adjust batch tasks
// @Description  Declaratively sets the target task count per SOP/subscene combination. Only pending/active batches allowed.
// @Tags         batches
// @Accept       json
// @Produce      json
// @Param        id   path      int                      true  "Batch ID"
// @Param        body body      AdjustBatchTasksRequest  true  "Task groups with target quantities"
// @Success      200  {object}  AdjustBatchTasksResponse
// @Failure      400  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /batches/{id}/tasks [post]
func (h *BatchHandler) AdjustBatchTasks(c *gin.Context) {
	idStr := c.Param("id")
	batchNumID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || batchNumID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	var req AdjustBatchTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if len(req.TaskGroups) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_groups must not be empty"})
		return
	}
	if a, b, dup := validateTaskGroupUniqueness(req.TaskGroups); dup {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("duplicate task_groups entries: task_groups[%d] and task_groups[%d] have the same sop_id and subscene_id", a, b),
		})
		return
	}

	now := time.Now().UTC()

	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] Failed to start transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Lock and validate batch
	type batchStatusRow struct {
		ID            int64  `db:"id"`
		OrderID       int64  `db:"order_id"`
		WorkstationID int64  `db:"workstation_id"`
		Status        string `db:"status"`
	}
	var batch batchStatusRow
	if err := tx.Get(&batch,
		"SELECT id, order_id, workstation_id, status FROM batches WHERE id = ? AND deleted_at IS NULL FOR UPDATE",
		batchNumID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to lock batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}

	if batch.Status != "pending" && batch.Status != "active" {
		c.JSON(http.StatusConflict, gin.H{
			"error":          fmt.Sprintf("batch status is '%s'; only pending or active batches can be adjusted", batch.Status),
			"current_status": batch.Status,
		})
		return
	}

	// Lock order for quota check and read organization_id (consistent with CreateBatch).
	type orderQuotaRow struct {
		TargetCount    int   `db:"target_count"`
		OrganizationID int64 `db:"organization_id"`
	}
	var orderQuota orderQuotaRow
	if err := tx.Get(&orderQuota, "SELECT target_count, organization_id FROM orders WHERE id = ? AND deleted_at IS NULL LIMIT 1 FOR UPDATE", batch.OrderID); err != nil {
		logger.Printf("[BATCH] Failed to lock order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}
	targetCount := orderQuota.TargetCount
	// organization_id is derived from the order (same as CreateBatch).
	organizationID := orderQuota.OrganizationID

	// Count current order-level completed count for quota check (completed-only).
	var orderCompletedCount int
	if err := tx.Get(&orderCompletedCount, "SELECT COUNT(*) FROM tasks WHERE order_id = ? AND deleted_at IS NULL AND status = 'completed'", batch.OrderID); err != nil {
		logger.Printf("[BATCH] Failed to count order completed tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}

	// Validate workstation for new tasks
	type wsRow struct {
		ID             int64 `db:"id"`
		FactoryID      int64 `db:"factory_id"`
		OrganizationID int64 `db:"organization_id"`
	}
	var ws wsRow
	if err := tx.Get(&ws, "SELECT id, factory_id, organization_id FROM workstations WHERE id = ? AND deleted_at IS NULL LIMIT 1", batch.WorkstationID); err != nil {
		logger.Printf("[BATCH] Failed to get workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}

	// Enforce tenant isolation: workstation must belong to the same organization as the order.
	if ws.OrganizationID != orderQuota.OrganizationID {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf(
				"workstation %d belongs to organization %d but order %d belongs to organization %d",
				batch.WorkstationID, ws.OrganizationID, batch.OrderID, orderQuota.OrganizationID,
			),
		})
		return
	}

	// Get batch name for denormalization
	var batchName string
	if err := tx.Get(&batchName, "SELECT COALESCE(name, '') FROM batches WHERE id = ? AND deleted_at IS NULL", batchNumID); err != nil {
		logger.Printf("[BATCH] Failed to get batch name: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}

	// subsceneInfo holds denormalized subscene data for task insertion.
	type subsceneInfo struct {
		SceneID int64  `db:"scene_id"`
		Scene   string `db:"scene"`
		Name    string `db:"name"`
		Layout  string `db:"layout"`
	}

	// Per-group analysis
	type groupPlan struct {
		tg          TaskGroupItem
		current     int
		locked      int
		pendingOnly int
		toInsert    int
		toDelete    int
		deleteIDs   []int64
		subscene    subsceneInfo
	}

	plans := make([]groupPlan, 0, len(req.TaskGroups))
	batchDelta := 0

	for _, tg := range req.TaskGroups {
		if tg.SOPID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "sop_id is required in each task_group"})
			return
		}
		if tg.SubsceneID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "subscene_id is required in each task_group"})
			return
		}
		if tg.Quantity < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quantity must be >= 0"})
			return
		}

		// Count current, locked, pending-only for this (sop_id, subscene_id) in this batch
		var counts struct {
			Current     int `db:"current_count"`
			LockedCount int `db:"locked_count"`
		}
		if err := tx.Get(&counts, `
			SELECT
				COUNT(*) AS current_count,
				COALESCE(SUM(CASE WHEN status != 'pending' OR episode_id IS NOT NULL THEN 1 ELSE 0 END), 0) AS locked_count
			FROM tasks
			WHERE batch_id = ? AND sop_id = ? AND subscene_id = ? AND deleted_at IS NULL`,
			batchNumID, tg.SOPID, tg.SubsceneID); err != nil {
			logger.Printf("[BATCH] Failed to count tasks for group: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
			return
		}

		pendingOnly := counts.Current - counts.LockedCount

		// Validate: cannot reduce below locked count
		if tg.Quantity < counts.LockedCount {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("cannot reduce sop_id=%d subscene_id=%d below locked count %d (requested %d)",
					tg.SOPID, tg.SubsceneID, counts.LockedCount, tg.Quantity),
			})
			return
		}

		plan := groupPlan{
			tg:          tg,
			current:     counts.Current,
			locked:      counts.LockedCount,
			pendingOnly: pendingOnly,
		}

		if tg.Quantity > counts.Current {
			plan.toInsert = tg.Quantity - counts.Current
		} else if tg.Quantity < counts.Current {
			toDelete := counts.Current - tg.Quantity
			if toDelete > pendingOnly {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("cannot delete %d tasks for sop_id=%d subscene_id=%d: only %d pending tasks available",
						toDelete, tg.SOPID, tg.SubsceneID, pendingOnly),
				})
				return
			}
			plan.toDelete = toDelete

			// Select IDs to delete (LIFO: newest first)
			var deleteIDs []int64
			if err := tx.Select(&deleteIDs, `
				SELECT id FROM tasks
				WHERE batch_id = ? AND sop_id = ? AND subscene_id = ? AND deleted_at IS NULL
				  AND status = 'pending' AND episode_id IS NULL
				ORDER BY created_at DESC, id DESC
				LIMIT ?`,
				batchNumID, tg.SOPID, tg.SubsceneID, toDelete); err != nil {
				logger.Printf("[BATCH] Failed to select tasks to delete: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			plan.deleteIDs = deleteIDs
		}

		// Validate subscene for inserts
		if plan.toInsert > 0 {
			if err := tx.Get(new(int), "SELECT 1 FROM sops WHERE id = ? AND deleted_at IS NULL LIMIT 1", tg.SOPID); err != nil {
				if err == sql.ErrNoRows {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("sop not found: %d", tg.SOPID)})
					return
				}
				logger.Printf("[BATCH] Failed to validate sop: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			if err := tx.Get(&plan.subscene, `
				SELECT ss.scene_id, s.name AS scene, ss.name, COALESCE(ss.initial_scene_layout, '') AS layout
				FROM subscenes ss
				JOIN scenes s ON s.id = ss.scene_id AND s.deleted_at IS NULL
				WHERE ss.id = ? AND ss.deleted_at IS NULL LIMIT 1`, tg.SubsceneID); err != nil {
				if err == sql.ErrNoRows {
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("subscene not found: %d", tg.SubsceneID)})
					return
				}
				logger.Printf("[BATCH] Failed to validate subscene: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
		}

		batchDelta += plan.toInsert - plan.toDelete
		plans = append(plans, plan)
	}

	// Quota check (completed-only): batch_delta (new tasks) must not exceed remaining = target_count - completed_count.
	remaining := targetCount - orderCompletedCount
	if batchDelta > remaining {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":           fmt.Sprintf("quota exceeded: target_count=%d, completed_count=%d, remaining=%d, batch_delta=%d", targetCount, orderCompletedCount, remaining, batchDelta),
			"target_count":    targetCount,
			"completed_count": orderCompletedCount,
			"remaining":       remaining,
			"batch_delta":     batchDelta,
		})
		return
	}

	// Execute: first all deletes, then all inserts
	deletedTaskIDs := make([]string, 0)
	for _, plan := range plans {
		for _, delID := range plan.deleteIDs {
			if _, err := tx.Exec(
				"UPDATE tasks SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
				now, now, delID,
			); err != nil {
				logger.Printf("[BATCH] Failed to soft-delete task %d: %v", delID, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			deletedTaskIDs = append(deletedTaskIDs, fmt.Sprintf("%d", delID))
		}
	}

	createdTasks := make([]CreatedTaskItem, 0)
	seqOffset := 0
	for _, plan := range plans {
		for i := 0; i < plan.toInsert; i++ {
			taskID, err := newPublicTaskID(now, seqOffset)
			if err != nil {
				logger.Printf("[BATCH] Failed to generate task_id: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			seqOffset++

			resTask, err := tx.Exec(
				`INSERT INTO tasks (
					task_id, batch_id, order_id, sop_id, workstation_id,
					scene_id, subscene_id, batch_name, scene_name, subscene_name,
					factory_id, organization_id, initial_scene_layout,
					status, assigned_at, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
				taskID, batchNumID, batch.OrderID, plan.tg.SOPID, batch.WorkstationID,
				plan.subscene.SceneID, plan.tg.SubsceneID, batchName, plan.subscene.Scene, plan.subscene.Name,
				ws.FactoryID, organizationID, plan.subscene.Layout,
				now, now, now,
			)
			if err != nil {
				logger.Printf("[BATCH] Failed to insert task: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			newTaskID, err := resTask.LastInsertId()
			if err != nil {
				logger.Printf("[BATCH] Failed to get task insert id: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
				return
			}
			createdTasks = append(createdTasks, CreatedTaskItem{
				ID:         fmt.Sprintf("%d", newTaskID),
				TaskID:     taskID,
				SOPID:      fmt.Sprintf("%d", plan.tg.SOPID),
				SubsceneID: fmt.Sprintf("%d", plan.tg.SubsceneID),
				Status:     "pending",
				CreatedAt:  now.Format(time.RFC3339),
			})
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to adjust batch tasks"})
		return
	}

	// If task deletions/inserts made the batch terminal, advance status accordingly.
	// Example: target reduced from 2 -> 1 after 1 task completed; remaining tasks may all be terminal.
	tryAdvanceBatchStatus(h.db, batchNumID)

	c.JSON(http.StatusOK, AdjustBatchTasksResponse{
		CreatedTasks:   createdTasks,
		DeletedTaskIDs: deletedTaskIDs,
	})
}

// RecallBatch transitions a batch from active or completed to recalled.
// Cancels pending/ready/in_progress tasks in the batch, and appends recalledEpisodeLabel to episodes.labels
// for episodes linked to completed tasks (downstream filtering).
//
// @Summary      Recall batch
// @Description  Recalls a batch: sets status to recalled, cancels non-terminal tasks, appends recalled_batch to related episodes' labels. Only active or completed batches.
// @Tags         batches
// @Produce      json
// @Param        id path int true "Batch ID"
// @Success      200 {object} PatchBatchResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /batches/{id}/recall [post]
func (h *BatchHandler) RecallBatch(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	now := time.Now().UTC()
	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] Failed to begin transaction for recall batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	type statusRow struct {
		Status    string       `db:"status"`
		StartedAt sql.NullTime `db:"started_at"`
	}
	var cur statusRow
	if err := tx.Get(&cur, "SELECT status, started_at FROM batches WHERE id = ? AND deleted_at IS NULL FOR UPDATE", id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to query batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	if cur.Status != "active" && cur.Status != "completed" {
		c.JSON(http.StatusConflict, gin.H{
			"error":          fmt.Sprintf("cannot recall batch in status '%s'; only active or completed batches can be recalled", cur.Status),
			"current_status": cur.Status,
		})
		return
	}

	toNotify := make([]taskDeviceRow, 0)
	if err := tx.Select(&toNotify, `
		SELECT
			t.task_id AS task_id,
			r.device_id AS device_id,
			t.status AS status
		FROM tasks t
		JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE t.batch_id = ? AND t.deleted_at IS NULL
		  AND t.status IN ('ready', 'in_progress')
	`, id); err != nil {
		logger.Printf("[BATCH] Failed to query tasks for recorder notify (batch=%d): %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	if _, err := tx.Exec(
		"UPDATE batches SET status = 'recalled', updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		now, id,
	); err != nil {
		logger.Printf("[BATCH] Failed to recall batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	if _, err := tx.Exec(
		`UPDATE tasks
		 SET status = 'cancelled', updated_at = ?
		 WHERE batch_id = ? AND deleted_at IS NULL
		   AND status IN ('pending', 'ready', 'in_progress')`,
		now, id,
	); err != nil {
		logger.Printf("[BATCH] Failed to cascade cancel tasks for recall batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	// Episodes are linked to tasks via episodes.task_id (see transfer upload path). tasks.episode_id
	// may be unset, so we must join on e.task_id = t.id, not t.episode_id = e.id.
	if _, err := tx.Exec(
		`UPDATE episodes e
		 INNER JOIN tasks t ON e.task_id = t.id AND t.deleted_at IS NULL AND e.deleted_at IS NULL
		 SET e.labels = IF(
		   JSON_CONTAINS(COALESCE(e.labels, JSON_ARRAY()), JSON_QUOTE(?), '$'),
		   e.labels,
		   JSON_ARRAY_APPEND(COALESCE(e.labels, JSON_ARRAY()), '$', ?)
		 ),
		 e.updated_at = ?
		 WHERE t.batch_id = ? AND t.status = 'completed'`,
		recalledEpisodeLabel, recalledEpisodeLabel, now, id,
	); err != nil {
		logger.Printf("[BATCH] Failed to update episode labels for recall batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	var recallWsID int64
	if err := tx.Get(&recallWsID, "SELECT workstation_id FROM batches WHERE id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[BATCH] Failed to read workstation_id for recall batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}
	if err := syncWorkstationStatusFromBatchesTx(tx, recallWsID); err != nil {
		logger.Printf("[BATCH] Failed to sync workstation status after recall batch %d: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] Failed to commit recall batch transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to recall batch"})
		return
	}

	if h.recorderHub != nil && len(toNotify) > 0 {
		go h.notifyRecorderCancelTasks(context.Background(), id, toNotify)
	}

	startedAtOut := ""
	if cur.StartedAt.Valid {
		startedAtOut = cur.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, PatchBatchResponse{
		ID:        fmt.Sprintf("%d", id),
		Status:    "recalled",
		StartedAt: startedAtOut,
		UpdatedAt: now.Format(time.RFC3339),
	})
}

// ListBatchTasks lists all tasks belonging to a batch.
//
// @Summary      List batch tasks
// @Description  Returns all tasks belonging to the specified batch
// @Tags         batches
// @Produce      json
// @Param        id path int true "Batch ID"
// @Success      200 {object} ListTasksResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /batches/{id}/tasks [get]
func (h *BatchHandler) ListBatchTasks(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	// Verify batch exists
	var exists int
	if err := h.db.Get(&exists, "SELECT 1 FROM batches WHERE id = ? AND deleted_at IS NULL LIMIT 1", id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[BATCH] Failed to verify batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list batch tasks"})
		return
	}

	items := make([]TaskListItem, 0)
	if err := h.db.Select(&items, `
		SELECT
			CAST(id AS CHAR) AS id,
			task_id AS task_id,
			CAST(batch_id AS CHAR) AS batch_id,
			CAST(order_id AS CHAR) AS order_id,
			CAST(sop_id AS CHAR) AS sop_id,
			CASE WHEN workstation_id IS NULL THEN NULL ELSE CAST(workstation_id AS CHAR) END AS workstation_id,
			CAST(scene_id AS CHAR) AS scene_id,
			COALESCE(scene_name, '') AS scene_name,
			CAST(subscene_id AS CHAR) AS subscene_id,
			COALESCE(subscene_name, '') AS subscene_name,
			status,
			CASE WHEN assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(assigned_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS assigned_at
		FROM tasks
		WHERE batch_id = ? AND deleted_at IS NULL
		ORDER BY created_at ASC, id ASC`, id); err != nil {
		logger.Printf("[BATCH] Failed to query batch tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list batch tasks"})
		return
	}

	c.JSON(http.StatusOK, ListTasksResponse{
		Items:  items,
		Total:  len(items),
		Limit:  len(items),
		Offset: 0,
	})
}

// syncWorkstationStatusFromBatchesTx sets workstations.status to active if any non-deleted batch
// for this workstation is active; otherwise inactive. If the workstation is offline (e.g. data
// collector logged out) or on break (operator pause), status is left unchanged so those states win
// over batch-driven updates.
func syncWorkstationStatusFromBatchesTx(tx *sqlx.Tx, workstationID int64) error {
	if workstationID <= 0 {
		return nil
	}
	var curStatus string
	if err := tx.Get(&curStatus, `
		SELECT status FROM workstations
		WHERE id = ? AND deleted_at IS NULL
		FOR UPDATE
	`, workstationID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if curStatus == "offline" || curStatus == "break" {
		return nil
	}
	var hasActive bool
	if err := tx.Get(&hasActive, `
		SELECT EXISTS(
			SELECT 1 FROM batches
			WHERE workstation_id = ? AND status = 'active' AND deleted_at IS NULL
		)
	`, workstationID); err != nil {
		return err
	}
	newStatus := "inactive"
	if hasActive {
		newStatus = "active"
	}
	if curStatus == newStatus {
		return nil
	}
	now := time.Now().UTC()
	_, err := tx.Exec(`
		UPDATE workstations
		SET status = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`, newStatus, now, workstationID)
	return err
}

// finalizeOpenBatchesAfterOrderCompletedTx runs inside the same transaction as the order -> completed transition.
// It cancels non-terminal tasks (pending, ready, in_progress) on batches that are still pending or active for
// this order, then sets those batches to completed so batch state matches a finished order.
// Workstation status is re-synced for each affected workstation.
// Before mutating tasks, it returns rows for notifyRecorderCancelTasksWithHub (ready -> clear, in_progress -> cancel).
func finalizeOpenBatchesAfterOrderCompletedTx(tx *sqlx.Tx, orderID int64, now time.Time) ([]orderCompletionRecorderNotify, error) {
	var wsIDs []int64
	if err := tx.Select(&wsIDs, `
		SELECT DISTINCT workstation_id
		FROM batches
		WHERE order_id = ? AND deleted_at IS NULL AND status IN ('pending', 'active')
	`, orderID); err != nil {
		return nil, err
	}
	if len(wsIDs) == 0 {
		return nil, nil
	}

	// Collect ready/in_progress tasks for Axon RPC (same join as PATCH batch cancel).
	var notifyRaw []taskDeviceBatchRow
	if err := tx.Select(&notifyRaw, `
		SELECT
			t.batch_id AS batch_id,
			t.task_id AS task_id,
			r.device_id AS device_id,
			t.status AS status
		FROM tasks t
		INNER JOIN batches b ON b.id = t.batch_id AND b.deleted_at IS NULL
		JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE t.order_id = ?
		  AND t.deleted_at IS NULL
		  AND b.status IN ('pending', 'active')
		  AND t.status IN ('ready', 'in_progress')
	`, orderID); err != nil {
		return nil, err
	}
	byBatch := make(map[int64][]taskDeviceRow)
	for _, row := range notifyRaw {
		byBatch[row.BatchID] = append(byBatch[row.BatchID], taskDeviceRow{
			TaskID:   row.TaskID,
			DeviceID: row.DeviceID,
			Status:   row.Status,
		})
	}
	recorderNotifies := make([]orderCompletionRecorderNotify, 0, len(byBatch))
	for bid, rows := range byBatch {
		recorderNotifies = append(recorderNotifies, orderCompletionRecorderNotify{BatchID: bid, Rows: rows})
	}

	if _, err := tx.Exec(`
		UPDATE tasks t
		INNER JOIN batches b ON b.id = t.batch_id AND b.deleted_at IS NULL
		SET t.status = 'cancelled', t.updated_at = ?
		WHERE t.order_id = ?
		  AND t.deleted_at IS NULL
		  AND b.status IN ('pending', 'active')
		  AND t.status IN ('pending', 'ready', 'in_progress')
	`, now, orderID); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`
		UPDATE batches
		SET status = 'completed',
		    started_at = COALESCE(started_at, ?),
		    ended_at = COALESCE(ended_at, ?),
		    updated_at = ?
		WHERE order_id = ? AND deleted_at IS NULL AND status IN ('pending', 'active')
	`, now, now, now, orderID); err != nil {
		return nil, err
	}

	for _, wsID := range wsIDs {
		if err := syncWorkstationStatusFromBatchesTx(tx, wsID); err != nil {
			return nil, err
		}
	}
	return recorderNotifies, nil
}

// tryAdvanceBatchStatus checks and advances batch status based on task completion.
// It should be called within or after a task status change transaction.
// - If batch is pending and a task just reached a terminal state: advance to active.
// - If batch is active and ALL tasks are in terminal state: advance to completed.
// Task cancellation to reach an all-cancelled set is done via PATCH batch (cancel), which sets
// the batch to cancelled already; this helper does not advance batch to cancelled.
// This function uses its own transaction and is safe to call after the task update commits.
func tryAdvanceBatchStatus(db *sqlx.DB, batchID int64) {
	tx, err := db.Beginx()
	if err != nil {
		logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to begin tx for batch %d: %v", batchID, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	type batchInfo struct {
		Status string `db:"status"`
	}
	var info batchInfo
	if err := tx.Get(&info, "SELECT status FROM batches WHERE id = ? AND deleted_at IS NULL FOR UPDATE", batchID); err != nil {
		if err != sql.ErrNoRows {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to lock batch %d: %v", batchID, err)
		}
		return
	}

	now := time.Now().UTC()

	switch info.Status {
	case "pending":
		// Advance to active only if at least one task is already in terminal state.
		// Historically this function was only called after a task reached terminal state,
		// but it may also be invoked after batch task adjustments (create/delete), so we
		// must guard against incorrectly advancing a never-started batch.
		var terminalCount int
		if err := tx.Get(&terminalCount, `
			SELECT COUNT(*) FROM tasks
			WHERE batch_id = ? AND deleted_at IS NULL
			  AND status IN ('completed', 'failed', 'cancelled')`,
			batchID); err != nil {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to count terminal tasks for batch %d: %v", batchID, err)
			return
		}
		if terminalCount == 0 {
			return
		}

		// Now it's safe to mark started; then re-evaluate completion.
		if _, err := tx.Exec(
			"UPDATE batches SET status = 'active', started_at = ?, updated_at = ? WHERE id = ? AND status = 'pending' AND deleted_at IS NULL",
			now, now, batchID,
		); err != nil {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to advance batch %d to active: %v", batchID, err)
			return
		}
		logger.Printf("[BATCH] Batch %d advanced: pending -> active", batchID)
		info.Status = "active"
		fallthrough

	case "active":
		// Check if ALL non-deleted tasks are in terminal state
		var nonTerminalCount int
		if err := tx.Get(&nonTerminalCount, `
			SELECT COUNT(*) FROM tasks
			WHERE batch_id = ? AND deleted_at IS NULL
			  AND status NOT IN ('completed', 'failed', 'cancelled')`,
			batchID); err != nil {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to count non-terminal tasks for batch %d: %v", batchID, err)
			return
		}

		// Also ensure there's at least one task
		var totalCount int
		if err := tx.Get(&totalCount, "SELECT COUNT(*) FROM tasks WHERE batch_id = ? AND deleted_at IS NULL", batchID); err != nil {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to count tasks for batch %d: %v", batchID, err)
			return
		}

		if totalCount > 0 && nonTerminalCount == 0 {
			if _, err := tx.Exec(
				"UPDATE batches SET status = 'completed', ended_at = ?, updated_at = ? WHERE id = ? AND status = 'active' AND deleted_at IS NULL",
				now, now, batchID,
			); err != nil {
				logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to advance batch %d to completed: %v", batchID, err)
				return
			}
			logger.Printf("[BATCH] Batch %d advanced: active -> completed (all %d tasks in terminal state)", batchID, totalCount)
		}

	default:
		// Terminal or non-advanceable state; do nothing
		return
	}

	var wsID int64
	if err := tx.Get(&wsID, "SELECT workstation_id FROM batches WHERE id = ? AND deleted_at IS NULL", batchID); err != nil {
		if err != sql.ErrNoRows {
			logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to read workstation_id for batch %d: %v", batchID, err)
		}
		return
	}
	if err := syncWorkstationStatusFromBatchesTx(tx, wsID); err != nil {
		logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to sync workstation status for batch %d: %v", batchID, err)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[BATCH] tryAdvanceBatchStatus: failed to commit for batch %d: %v", batchID, err)
	}
}
