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

// BatchHandler handles batch-related HTTP requests.
type BatchHandler struct {
	db *sqlx.DB
}

// NewBatchHandler creates a new BatchHandler.
func NewBatchHandler(db *sqlx.DB) *BatchHandler {
	return &BatchHandler{db: db}
}

// RegisterRoutes registers batch routes under the provided router group.
func (h *BatchHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/batches", h.ListBatches)
	apiV1.GET("/batches/:id", h.GetBatch)
	apiV1.DELETE("/batches/:id", h.DeleteBatch)
	apiV1.PATCH("/batches/:id", h.PatchBatch)
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
	ID            string `json:"id" db:"id"`
	BatchID       string `json:"batch_id" db:"batch_id"`
	OrderID       string `json:"order_id" db:"order_id"`
	WorkstationID string `json:"workstation_id" db:"workstation_id"`
	Name          string `json:"name" db:"name"`
	Notes         string `json:"notes,omitempty" db:"notes"`
	Status        string `json:"status" db:"status"`
	EpisodeCount  int    `json:"episode_count" db:"episode_count"`
	StartedAt     string `json:"started_at,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
	Metadata      any    `json:"metadata,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// ListBatchesResponse represents the response body for listing batches.
type ListBatchesResponse struct {
	Batches []BatchListItem `json:"batches"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
}

type batchRow struct {
	ID            int64          `db:"id"`
	BatchID       string         `db:"batch_id"`
	OrderID       int64          `db:"order_id"`
	WorkstationID int64          `db:"workstation_id"`
	Name          string         `db:"name"`
	Notes         sql.NullString `db:"notes"`
	Status        string         `db:"status"`
	EpisodeCount  int            `db:"episode_count"`
	StartedAt     sql.NullTime   `db:"started_at"`
	EndedAt       sql.NullTime   `db:"ended_at"`
	Metadata      sql.NullString `db:"metadata"`
	CreatedAt     sql.NullString `db:"created_at"`
	UpdatedAt     sql.NullString `db:"updated_at"`
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
		createdAt = r.CreatedAt.String
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.String
	}

	return BatchListItem{
		ID:            fmt.Sprintf("%d", r.ID),
		BatchID:       r.BatchID,
		OrderID:       fmt.Sprintf("%d", r.OrderID),
		WorkstationID: fmt.Sprintf("%d", r.WorkstationID),
		Name:          r.Name,
		Notes:         notes,
		Status:        r.Status,
		EpisodeCount:  r.EpisodeCount,
		StartedAt:     startedAt,
		EndedAt:       endedAt,
		Metadata:      parseNullableJSON(r.Metadata),
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
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

	conditions := []string{"deleted_at IS NULL"}
	args := make([]interface{}, 0, 6)

	if orderID != "" {
		conditions = append(conditions, "CAST(order_id AS CHAR) = ?")
		args = append(args, orderID)
	}
	if workstationID != "" {
		conditions = append(conditions, "CAST(workstation_id AS CHAR) = ?")
		args = append(args, workstationID)
	}
	if status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, status)
	}

	whereClause := strings.Join(conditions, " AND ")

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM batches WHERE %s", whereClause)
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[BATCH] Failed to count batches: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list batches"})
		return
	}

	query := fmt.Sprintf(`
		SELECT
			id,
			batch_id,
			order_id,
			workstation_id,
			name,
			notes,
			status,
			episode_count,
			started_at,
			ended_at,
			CAST(metadata AS CHAR) AS metadata,
			created_at,
			updated_at
		FROM batches
		WHERE %s
		ORDER BY id DESC
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

	c.JSON(http.StatusOK, ListBatchesResponse{
		Batches: batches,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
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
			id,
			batch_id,
			order_id,
			workstation_id,
			name,
			notes,
			status,
			episode_count,
			started_at,
			ended_at,
			CAST(metadata AS CHAR) AS metadata,
			created_at,
			updated_at
		FROM batches
		WHERE id = ? AND deleted_at IS NULL
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
// Only batches with status "cancelled" can be deleted.
//
// @Summary      Delete batch
// @Description  Soft deletes a batch by ID. Only allowed when status is cancelled.
// @Tags         batches
// @Accept       json
// @Produce      json
// @Param        id path     int  true  "Batch ID"
// @Success      204
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

	if status != "cancelled" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "batch can only be deleted when status is cancelled"})
		return
	}

	now := time.Now().UTC()
	if _, err := h.db.Exec("UPDATE batches SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id); err != nil {
		logger.Printf("[BATCH] Failed to delete batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete batch"})
		return
	}

	c.Status(http.StatusNoContent)
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

// PatchBatch handles batch status updates. Only supports status transitions:
// - pending -> active | cancelled
// - active  -> completed | cancelled
//
// @Summary      Patch batch
// @Description  Updates batch status. Only specific state transitions are allowed.
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

	allowed := false
	switch cur.Status {
	case "pending":
		allowed = req.Status == "active" || req.Status == "cancelled"
	case "active":
		allowed = req.Status == "completed" || req.Status == "cancelled"
	default:
		allowed = false
	}
	if !allowed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status transition"})
		return
	}

	now := time.Now().UTC()
	startedAt := cur.StartedAt
	updates := []string{"status = ?", "updated_at = ?"}
	args := []interface{}{req.Status, now}

	// pending -> active sets started_at when not set yet
	if cur.Status == "pending" && req.Status == "active" && !startedAt.Valid {
		startedAt = sql.NullTime{Time: now, Valid: true}
		updates = append(updates, "started_at = ?")
		args = append(args, now)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE batches SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))
	if _, err := h.db.Exec(query, args...); err != nil {
		logger.Printf("[BATCH] Failed to patch batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to patch batch"})
		return
	}

	startedAtOut := ""
	if startedAt.Valid {
		startedAtOut = startedAt.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, PatchBatchResponse{
		ID:        fmt.Sprintf("%d", id),
		Status:    req.Status,
		StartedAt: startedAtOut,
		UpdatedAt: now.Format(time.RFC3339),
	})
}
