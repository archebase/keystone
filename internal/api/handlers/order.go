// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// OrderHandler handles order-related HTTP requests.
type OrderHandler struct {
	db *sqlx.DB
}

// NewOrderHandler creates a new OrderHandler.
func NewOrderHandler(db *sqlx.DB) *OrderHandler {
	return &OrderHandler{db: db}
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
	Orders []OrderResponse `json:"orders"`
}

// OrderResponse is the response body for a single order.
type OrderResponse struct {
	ID             string `json:"id"`
	SceneID        string `json:"scene_id"`
	Name           string `json:"name"`
	TargetCount    int    `json:"target_count"`
	CompletedCount int    `json:"completed_count"`
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
	Name           string         `db:"name"`
	TargetCount    int            `db:"target_count"`
	CompletedCount int            `db:"completed_count"`
	Status         string         `db:"status"`
	Priority       string         `db:"priority"`
	Deadline       sql.NullTime   `db:"deadline"`
	Metadata       sql.NullString `db:"metadata"`
	CreatedAt      sql.NullString `db:"created_at"`
	UpdatedAt      sql.NullString `db:"updated_at"`
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

// ListOrders returns all non-deleted orders.
func (h *OrderHandler) ListOrders(c *gin.Context) {
	query := `
		SELECT
			o.id,
			o.scene_id,
			o.name,
			o.target_count,
			o.status,
			o.priority,
			o.deadline,
			CAST(o.metadata AS CHAR) AS metadata,
			o.created_at,
			o.updated_at,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'completed' AND t.deleted_at IS NULL) AS completed_count
		FROM orders o
		WHERE o.deleted_at IS NULL
		ORDER BY o.id DESC
	`

	var rows []orderRow
	if err := h.db.Select(&rows, query); err != nil {
		logger.Printf("[ORDER] Failed to query orders: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list orders"})
		return
	}

	orders := make([]OrderResponse, 0, len(rows))
	for _, r := range rows {
		createdAt := ""
		if r.CreatedAt.Valid {
			createdAt = r.CreatedAt.String
		}
		updatedAt := ""
		if r.UpdatedAt.Valid {
			updatedAt = r.UpdatedAt.String
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
		orders = append(orders, OrderResponse{
			ID:             fmt.Sprintf("%d", r.ID),
			SceneID:        fmt.Sprintf("%d", r.SceneID),
			Name:           r.Name,
			TargetCount:    r.TargetCount,
			CompletedCount: r.CompletedCount,
			Status:         r.Status,
			Priority:       r.Priority,
			Deadline:       deadline,
			Metadata:       metadata,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
		})
	}

	c.JSON(http.StatusOK, OrderListResponse{Orders: orders})
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
			o.name,
			o.target_count,
			o.status,
			o.priority,
			o.deadline,
			CAST(o.metadata AS CHAR) AS metadata,
			o.created_at,
			o.updated_at,
			(SELECT COUNT(*) FROM tasks t WHERE t.order_id = o.id AND t.status = 'completed' AND t.deleted_at IS NULL) AS completed_count
		FROM orders o
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
		createdAt = r.CreatedAt.String
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.String
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

	c.JSON(http.StatusOK, OrderResponse{
		ID:             fmt.Sprintf("%d", r.ID),
		SceneID:        fmt.Sprintf("%d", r.SceneID),
		Name:           r.Name,
		TargetCount:    r.TargetCount,
		CompletedCount: r.CompletedCount,
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
	err = h.db.Get(&organizationID, `
		SELECT f.organization_id
		FROM scenes s
		JOIN factories f ON f.id = s.factory_id
		WHERE s.id = ? AND s.deleted_at IS NULL AND f.deleted_at IS NULL
	`, sceneID)
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
		Name:           req.Name,
		TargetCount:    req.TargetCount,
		CompletedCount: 0,
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

	if req.TargetCount != nil {
		if *req.TargetCount <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_count must be > 0"})
			return
		}
		updates = append(updates, "target_count = ?")
		args = append(args, *req.TargetCount)
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

	if req.Status != nil {
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
	if _, err := h.db.Exec(query, args...); err != nil {
		logger.Printf("[ORDER] Failed to update order: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update order"})
		return
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
