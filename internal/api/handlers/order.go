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

// OrderHandler handles order-related HTTP requests.
type OrderHandler struct {
	db                 *sqlx.DB
	recorderHub        *services.RecorderHub
	recorderRPCTimeout time.Duration
}

// NewOrderHandler creates a new OrderHandler.
// recorderHub may be nil (skips Axon cancel RPCs after finalizing open batches when an order is completed via target_count).
func NewOrderHandler(db *sqlx.DB, recorderHub *services.RecorderHub, recorderRPCTimeout time.Duration) *OrderHandler {
	return &OrderHandler{db: db, recorderHub: recorderHub, recorderRPCTimeout: recorderRPCTimeout}
}

// RegisterRoutes registers order routes under the provided router group.
func (h *OrderHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/orders", h.ListOrders)
	apiV1.POST("/orders", h.CreateOrder)
	apiV1.GET("/orders/:id", h.GetOrder)
	apiV1.PUT("/orders/:id", h.UpdateOrder)
	apiV1.DELETE("/orders/:id", h.DeleteOrder)
}

// OrderListResponse is the response body for listing orders.
type OrderListResponse struct {
	Items   []OrderResponse `json:"items"`
	Scenes  []SceneRef      `json:"scenes,omitempty"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	HasNext bool            `json:"hasNext,omitempty"`
	HasPrev bool            `json:"hasPrev,omitempty"`
}

// SceneRef is a minimal scene representation used by order pages.
type SceneRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OrderResponse is the response body for a single order.
type OrderResponse struct {
	ID             string `json:"id"`
	SceneID        string `json:"scene_id"`
	SceneName      string `json:"scene_name,omitempty"`
	Name           string `json:"name"`
	TargetCount    int    `json:"target_count"`
	TaskCount      int    `json:"task_count"`
	CompletedCount int    `json:"completed_count"`
	CancelledCount int    `json:"cancelled_count"`
	FailedCount    int    `json:"failed_count"`
	Status         string `json:"status"`
	Priority       string `json:"priority"`
	Deadline       string `json:"deadline,omitempty"`
	Metadata       any    `json:"metadata,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

// CreateOrderRequest is the request body for creating an order.
type CreateOrderRequest struct {
	SceneID     string          `json:"scene_id"`
	Name        string          `json:"name"`
	TargetCount int             `json:"target_count"`
	Priority    string          `json:"priority"`
	Deadline    *string         `json:"deadline,omitempty"` // RFC3339
	Metadata    json.RawMessage `json:"metadata,omitempty"` // JSON object
}

// UpdateOrderRequest is the request body for partially updating an order.
type UpdateOrderRequest struct {
	SceneID     *string `json:"scene_id,omitempty"`
	Name        *string `json:"name,omitempty"`
	TargetCount *int    `json:"target_count,omitempty"`
	Priority    *string `json:"priority,omitempty"`
	Status      *string `json:"status,omitempty"`
	Deadline    *string `json:"deadline,omitempty"` // RFC3339 or empty to clear
	// Metadata uses optionalJSONPatch so JSON null is distinct from omitting the key.
	// For orders, explicit null is normalized to "{}" (empty object).
	Metadata optionalJSONPatch `json:"metadata,omitempty"`
}

type orderRow struct {
	ID             int64          `db:"id"`
	SceneID        int64          `db:"scene_id"`
	SceneName      sql.NullString `db:"scene_name"`
	Name           string         `db:"name"`
	TargetCount    int            `db:"target_count"`
	TaskCount      int            `db:"task_count"`
	CompletedCount int            `db:"completed_count"`
	CancelledCount int            `db:"cancelled_count"`
	FailedCount    int            `db:"failed_count"`
	Status         string         `db:"status"`
	Priority       string         `db:"priority"`
	Deadline       sql.NullTime   `db:"deadline"`
	Metadata       sql.NullString `db:"metadata"`
	CreatedAt      sql.NullTime   `db:"created_at"`
	UpdatedAt      sql.NullTime   `db:"updated_at"`
}

var validOrderPriorities = map[string]struct{}{
	"low":    {},
	"normal": {},
	"high":   {},
	"urgent": {},
}

var validOrderStatuses = map[string]struct{}{
	"created":     {},
	"in_progress": {},
	"paused":      {},
	"completed":   {},
	"cancelled":   {},
}

// ListOrders returns all non-deleted orders with pagination.
func (h *OrderHandler) ListOrders(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	if status != "" {
		if _, ok := validOrderStatuses[status]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
	}

	countQuery := "SELECT COUNT(*) FROM orders WHERE deleted_at IS NULL"
	countArgs := []any{}
	if status != "" {
		countQuery += " AND status = ?"
		countArgs = append(countArgs, status)
	}
	var total int
	if err := h.db.Get(&total, countQuery, countArgs...); err != nil {
		logger.Printf("[ORDER] Failed to count orders: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list orders"})
		return
	}

	query := `
		SELECT
			o.id,
			o.scene_id,
			s.name AS scene_name,
			o.name,
			o.target_count,
			o.status,
			o.priority,
			o.deadline,
			CAST(o.metadata AS CHAR) AS metadata,
			o.created_at,
			o.updated_at,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.deleted_at IS NULL) AS task_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'completed' AND t.deleted_at IS NULL) AS completed_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'cancelled' AND t.deleted_at IS NULL) AS cancelled_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'failed' AND t.deleted_at IS NULL) AS failed_count
		FROM orders o
		LEFT JOIN scenes s ON s.id = o.scene_id AND s.deleted_at IS NULL
		WHERE o.deleted_at IS NULL
	`
	args := []any{}
	if status != "" {
		query += " AND o.status = ?\n"
		args = append(args, status)
	}
	query += `
		ORDER BY o.id DESC
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var rows []orderRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[ORDER] Failed to query orders: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list orders"})
		return
	}

	orders := make([]OrderResponse, 0, len(rows))
	for _, r := range rows {
		createdAt := ""
		if r.CreatedAt.Valid {
			createdAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		updatedAt := ""
		if r.UpdatedAt.Valid {
			updatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}
		deadline := ""
		if r.Deadline.Valid {
			deadline = r.Deadline.Time.UTC().Format(time.RFC3339)
		}
		var metadata any
		if r.Metadata.Valid && strings.TrimSpace(r.Metadata.String) != "" && strings.TrimSpace(r.Metadata.String) != "null" {
			var v any
			if err := json.Unmarshal([]byte(r.Metadata.String), &v); err == nil {
				metadata = v
			}
		}
		sceneName := ""
		if r.SceneName.Valid {
			sceneName = r.SceneName.String
		}
		orders = append(orders, OrderResponse{
			ID:             fmt.Sprintf("%d", r.ID),
			SceneID:        fmt.Sprintf("%d", r.SceneID),
			SceneName:      sceneName,
			Name:           r.Name,
			TargetCount:    r.TargetCount,
			TaskCount:      r.TaskCount,
			CompletedCount: r.CompletedCount,
			CancelledCount: r.CancelledCount,
			FailedCount:    r.FailedCount,
			Status:         r.Status,
			Priority:       r.Priority,
			Deadline:       deadline,
			Metadata:       metadata,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
		})
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	// Provide scenes to allow order pages to avoid a separate GET /scenes call.
	var sceneRefs []SceneRef
	{
		type sceneRefRow struct {
			ID   int64  `db:"id"`
			Name string `db:"name"`
		}
		var srows []sceneRefRow
		// Cap to a reasonable number; UI only needs id/name for select options.
		if err := h.db.Select(&srows, `
			SELECT id, name
			FROM scenes
			WHERE deleted_at IS NULL
			ORDER BY id DESC
			LIMIT 500
		`); err != nil {
			logger.Printf("[ORDER] Failed to query scene refs: %v", err)
		} else {
			sceneRefs = make([]SceneRef, 0, len(srows))
			for _, sr := range srows {
				sceneRefs = append(sceneRefs, SceneRef{
					ID:   fmt.Sprintf("%d", sr.ID),
					Name: sr.Name,
				})
			}
		}
	}

	c.JSON(http.StatusOK, OrderListResponse{
		Items:   orders,
		Scenes:  sceneRefs,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// GetOrder returns a single order by id.
func (h *OrderHandler) GetOrder(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	query := `
		SELECT
			o.id,
			o.scene_id,
			s.name AS scene_name,
			o.name,
			o.target_count,
			o.status,
			o.priority,
			o.deadline,
			CAST(o.metadata AS CHAR) AS metadata,
			o.created_at,
			o.updated_at,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.deleted_at IS NULL) AS task_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'completed' AND t.deleted_at IS NULL) AS completed_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'cancelled' AND t.deleted_at IS NULL) AS cancelled_count,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'failed' AND t.deleted_at IS NULL) AS failed_count
		FROM orders o
		LEFT JOIN scenes s ON s.id = o.scene_id AND s.deleted_at IS NULL
		WHERE o.id = ? AND o.deleted_at IS NULL
	`

	var r orderRow
	if err := h.db.Get(&r, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
			return
		}
		logger.Printf("[ORDER] Failed to query order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get order"})
		return
	}

	createdAt := ""
	if r.CreatedAt.Valid {
		createdAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	deadline := ""
	if r.Deadline.Valid {
		deadline = r.Deadline.Time.UTC().Format(time.RFC3339)
	}
	var metadata any
	if r.Metadata.Valid && strings.TrimSpace(r.Metadata.String) != "" && strings.TrimSpace(r.Metadata.String) != "null" {
		var v any
		if err := json.Unmarshal([]byte(r.Metadata.String), &v); err == nil {
			metadata = v
		}
	}
	sceneName := ""
	if r.SceneName.Valid {
		sceneName = r.SceneName.String
	}

	c.JSON(http.StatusOK, OrderResponse{
		ID:             fmt.Sprintf("%d", r.ID),
		SceneID:        fmt.Sprintf("%d", r.SceneID),
		SceneName:      sceneName,
		Name:           r.Name,
		TargetCount:    r.TargetCount,
		TaskCount:      r.TaskCount,
		CompletedCount: r.CompletedCount,
		CancelledCount: r.CancelledCount,
		FailedCount:    r.FailedCount,
		Status:         r.Status,
		Priority:       r.Priority,
		Deadline:       deadline,
		Metadata:       metadata,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	})
}

// CreateOrder creates a new order.
func (h *OrderHandler) CreateOrder(c *gin.Context) {
	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.SceneID = strings.TrimSpace(req.SceneID)
	req.Name = strings.TrimSpace(req.Name)
	req.Priority = strings.TrimSpace(req.Priority)

	if req.SceneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scene_id is required"})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.TargetCount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_count must be > 0"})
		return
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if _, ok := validOrderPriorities[req.Priority]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid priority"})
		return
	}

	var deadline sql.NullTime
	if req.Deadline != nil {
		dl := strings.TrimSpace(*req.Deadline)
		if dl != "" {
			tm, err := time.Parse(time.RFC3339, dl)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deadline format (RFC3339)"})
				return
			}
			deadline = sql.NullTime{Time: tm.UTC(), Valid: true}
		}
	}

	var metadataStr sql.NullString
	if len(req.Metadata) > 0 {
		raw := strings.TrimSpace(string(req.Metadata))
		if raw == "" || raw == "null" {
			metadataStr = sql.NullString{Valid: false}
		} else {
			var tmp any
			if err := json.Unmarshal(req.Metadata, &tmp); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata (must be valid JSON)"})
				return
			}
			metadataStr = sql.NullString{String: raw, Valid: true}
		}
	}

	sceneID, err := strconv.ParseInt(req.SceneID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene_id format"})
		return
	}

	// Derive organization_id from scene -> factory -> organization
	var organizationID int64
	var sceneName string
	err = h.db.QueryRowx(`
		SELECT f.organization_id, s.name
		FROM scenes s
		JOIN factories f ON f.id = s.factory_id
		WHERE s.id = ? AND s.deleted_at IS NULL AND f.deleted_at IS NULL
	`, sceneID).Scan(&organizationID, &sceneName)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scene not found"})
			return
		}
		logger.Printf("[ORDER] Failed to derive organization_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create order"})
		return
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO orders (
			organization_id,
			scene_id,
			name,
			target_count,
			priority,
			deadline,
			metadata,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'created', ?, ?)`,
		organizationID,
		sceneID,
		req.Name,
		req.TargetCount,
		req.Priority,
		deadline,
		metadataStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[ORDER] Failed to insert order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create order"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[ORDER] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create order"})
		return
	}

	var metadata any
	if metadataStr.Valid && strings.TrimSpace(metadataStr.String) != "" && strings.TrimSpace(metadataStr.String) != "null" {
		var v any
		if err := json.Unmarshal([]byte(metadataStr.String), &v); err == nil {
			metadata = v
		}
	}
	deadlineOut := ""
	if deadline.Valid {
		deadlineOut = deadline.Time.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusCreated, OrderResponse{
		ID:             fmt.Sprintf("%d", id),
		SceneID:        fmt.Sprintf("%d", sceneID),
		SceneName:      sceneName,
		Name:           req.Name,
		TargetCount:    req.TargetCount,
		TaskCount:      0,
		CompletedCount: 0,
		CancelledCount: 0,
		FailedCount:    0,
		Status:         "created",
		Priority:       req.Priority,
		Deadline:       deadlineOut,
		Metadata:       metadata,
		CreatedAt:      now.Format(time.RFC3339),
		UpdatedAt:      now.Format(time.RFC3339),
	})
}

// UpdateOrder partially updates mutable order fields.
func (h *OrderHandler) UpdateOrder(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	var req UpdateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Ensure exists
	var exists bool
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM orders WHERE id = ? AND deleted_at IS NULL)", id); err != nil {
		logger.Printf("[ORDER] Failed to check order existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}

	updates := []string{}
	args := []interface{}{}

	if req.SceneID != nil {
		sceneIDStr := strings.TrimSpace(*req.SceneID)
		if sceneIDStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scene_id cannot be empty"})
			return
		}
		sceneID, err := strconv.ParseInt(sceneIDStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene_id format"})
			return
		}
		var sceneExists bool
		if err := h.db.Get(&sceneExists, "SELECT EXISTS(SELECT 1 FROM scenes WHERE id = ? AND deleted_at IS NULL)", sceneID); err != nil {
			logger.Printf("[ORDER] Failed to verify scene: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		if !sceneExists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scene not found"})
			return
		}
		updates = append(updates, "scene_id = ?")
		args = append(args, sceneID)
	}

	var autoStatusFromTarget *string
	if req.TargetCount != nil {
		if *req.TargetCount <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_count must be > 0"})
			return
		}
		type orderTargetCtx struct {
			Status         string `db:"status"`
			CompletedCount int    `db:"completed_count"`
		}
		var octx orderTargetCtx
		if err := h.db.Get(&octx, `
			SELECT o.status,
				(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.deleted_at IS NULL AND t.status = 'completed') AS completed_count
			FROM orders o WHERE o.id = ? AND o.deleted_at IS NULL`, id); err != nil {
			logger.Printf("[ORDER] Failed to load order for target_count update: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		if *req.TargetCount < octx.CompletedCount {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_count cannot be less than completed_count"})
			return
		}
		updates = append(updates, "target_count = ?")
		args = append(args, *req.TargetCount)
		switch {
		case *req.TargetCount == octx.CompletedCount && octx.Status != "cancelled" && octx.Status != "completed":
			s := "completed"
			autoStatusFromTarget = &s
		case octx.Status == "completed" && *req.TargetCount > octx.CompletedCount:
			s := "in_progress"
			autoStatusFromTarget = &s
		}
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
			return
		}
		updates = append(updates, "name = ?")
		args = append(args, name)
	}

	if req.Deadline != nil {
		dl := strings.TrimSpace(*req.Deadline)
		if dl == "" {
			updates = append(updates, "deadline = NULL")
		} else {
			tm, err := time.Parse(time.RFC3339, dl)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deadline format (RFC3339)"})
				return
			}
			updates = append(updates, "deadline = ?")
			args = append(args, tm.UTC())
		}
	}

	if req.Metadata.Present {
		if req.Metadata.Value == nil {
			updates = append(updates, "metadata = ?")
			args = append(args, "{}")
		} else {
			// Keep order metadata as a JSON object for consistency.
			if _, ok := req.Metadata.Value.(map[string]interface{}); !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata (must be a JSON object)"})
				return
			}
			b, err := json.Marshal(req.Metadata.Value)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata (must be valid JSON)"})
				return
			}
			updates = append(updates, "metadata = ?")
			args = append(args, strings.TrimSpace(string(b)))
		}
	}

	if req.Priority != nil {
		priority := strings.TrimSpace(*req.Priority)
		if priority == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "priority cannot be empty"})
			return
		}
		if _, ok := validOrderPriorities[priority]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid priority"})
			return
		}
		updates = append(updates, "priority = ?")
		args = append(args, priority)
	}

	if autoStatusFromTarget != nil {
		updates = append(updates, "status = ?")
		args = append(args, *autoStatusFromTarget)
	} else if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status cannot be empty"})
			return
		}
		if _, ok := validOrderStatuses[status]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		updates = append(updates, "status = ?")
		args = append(args, status)
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now, id)

	query := fmt.Sprintf("UPDATE orders SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))
	if autoStatusFromTarget != nil && *autoStatusFromTarget == "completed" {
		tx, err := h.db.Beginx()
		if err != nil {
			logger.Printf("[ORDER] Failed to begin tx for order update: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(query, args...); err != nil {
			logger.Printf("[ORDER] Failed to update order: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		orderFinalizeRecorderNotifies, finErr := finalizeOpenBatchesAfterOrderCompletedTx(tx, id, now)
		if finErr != nil {
			logger.Printf("[ORDER] Failed to finalize open batches after order completed via target_count: %v", finErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		if err := tx.Commit(); err != nil {
			logger.Printf("[ORDER] Failed to commit order update: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
		if len(orderFinalizeRecorderNotifies) > 0 && h.recorderHub != nil {
			notifies := orderFinalizeRecorderNotifies
			go func() {
				ctx := context.Background()
				for _, n := range notifies {
					notifyRecorderCancelTasksWithHub(ctx, h.recorderHub, h.recorderRPCTimeout, n.BatchID, n.Rows)
				}
			}()
		}
	} else {
		if _, err := h.db.Exec(query, args...); err != nil {
			logger.Printf("[ORDER] Failed to update order: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
			return
		}
	}

	h.GetOrder(c)
}

// DeleteOrder soft-deletes an order if it is not referenced by other production units.
func (h *OrderHandler) DeleteOrder(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	var exists bool
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM orders WHERE id = ? AND deleted_at IS NULL)", id); err != nil {
		logger.Printf("[ORDER] Failed to check order existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete order"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}

	// Reject deletion when the order has related batches/tasks/episodes.
	var batchCount int
	if err := h.db.Get(&batchCount, "SELECT COUNT(*) FROM batches WHERE order_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORDER] Failed to check batch references: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete order"})
		return
	}
	if batchCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete order referenced by %d batches", batchCount)})
		return
	}

	var taskCount int
	if err := h.db.Get(&taskCount, "SELECT COUNT(*) FROM tasks WHERE order_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORDER] Failed to check task references: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete order"})
		return
	}
	if taskCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete order referenced by %d tasks", taskCount)})
		return
	}

	var episodeCount int
	if err := h.db.Get(&episodeCount, "SELECT COUNT(*) FROM episodes WHERE order_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORDER] Failed to check episode references: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete order"})
		return
	}
	if episodeCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete order referenced by %d episodes", episodeCount)})
		return
	}

	now := time.Now().UTC()
	if _, err := h.db.Exec("UPDATE orders SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id); err != nil {
		logger.Printf("[ORDER] Failed to delete order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete order"})
		return
	}

	c.Status(http.StatusNoContent)
}

// tryAdvanceOrderStatus advances order status based on completed tasks count.
//
// Rules (completed-only):
// - created -> in_progress when there is at least one completed task
// - in_progress -> completed when completed_count == target_count
//
// This helper uses its own transaction and is safe to call after task updates commit.
// recorderHub may be nil (skips Axon clear/cancel RPCs after finalizing open batches).
func tryAdvanceOrderStatus(db *sqlx.DB, orderID int64, recorderHub *services.RecorderHub, recorderRPCTimeout time.Duration) {
	tx, err := db.Beginx()
	if err != nil {
		logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to begin tx for order %d: %v", orderID, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	var orderFinalizeRecorderNotifies []orderCompletionRecorderNotify

	type orderInfo struct {
		Status      string `db:"status"`
		TargetCount int    `db:"target_count"`
	}
	var info orderInfo
	if err := tx.Get(&info, "SELECT status, target_count FROM orders WHERE id = ? AND deleted_at IS NULL FOR UPDATE", orderID); err != nil {
		if err != sql.ErrNoRows {
			logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to lock order %d: %v", orderID, err)
		}
		return
	}

	// Only auto-advance non-terminal statuses.
	if info.Status != "created" && info.Status != "in_progress" {
		return
	}
	if info.TargetCount <= 0 {
		return
	}

	var completedCount int
	if err := tx.Get(&completedCount, `
		SELECT COUNT(*) FROM tasks
		WHERE order_id = ? AND deleted_at IS NULL AND status = 'completed'
	`, orderID); err != nil {
		logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to count completed tasks for order %d: %v", orderID, err)
		return
	}

	now := time.Now().UTC()

	if info.Status == "created" && completedCount > 0 {
		if _, err := tx.Exec(
			"UPDATE orders SET status = 'in_progress', updated_at = ? WHERE id = ? AND status = 'created' AND deleted_at IS NULL",
			now, orderID,
		); err != nil {
			logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to advance order %d created->in_progress: %v", orderID, err)
			return
		}
		info.Status = "in_progress"
	}

	if info.Status == "in_progress" && completedCount == info.TargetCount {
		if _, err := tx.Exec(
			"UPDATE orders SET status = 'completed', updated_at = ? WHERE id = ? AND status = 'in_progress' AND deleted_at IS NULL",
			now, orderID,
		); err != nil {
			logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to advance order %d in_progress->completed: %v", orderID, err)
			return
		}
		// Close any still-open batches for this order: cancel non-terminal tasks, then mark batches completed.
		var finErr error
		orderFinalizeRecorderNotifies, finErr = finalizeOpenBatchesAfterOrderCompletedTx(tx, orderID, now)
		if finErr != nil {
			logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to finalize open batches for completed order %d: %v", orderID, finErr)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[ORDER] tryAdvanceOrderStatus: failed to commit for order %d: %v", orderID, err)
		return
	}

	// Best-effort: after commit, notify Axon recorder for ready/in_progress tasks we cancelled (same as PATCH batch cancel).
	if len(orderFinalizeRecorderNotifies) > 0 && recorderHub != nil {
		notifies := orderFinalizeRecorderNotifies
		go func() {
			ctx := context.Background()
			for _, n := range notifies {
				notifyRecorderCancelTasksWithHub(ctx, recorderHub, recorderRPCTimeout, n.BatchID, n.Rows)
			}
		}()
	}
}
