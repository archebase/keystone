// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
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

// RobotTypeHandler handles robot type related HTTP requests.
type RobotTypeHandler struct {
	db *sqlx.DB
}

// NewRobotTypeHandler creates a new RobotTypeHandler.
func NewRobotTypeHandler(db *sqlx.DB) *RobotTypeHandler {
	return &RobotTypeHandler{db: db}
}

// CreateRobotTypeRequest represents the request body for creating a robot type.
type CreateRobotTypeRequest struct {
	Name         string          `json:"name"`
	Model        string          `json:"model"`
	Manufacturer *string         `json:"manufacturer,omitempty"`
	EndEffector  *string         `json:"end_effector,omitempty"`
	SensorSuite  json.RawMessage `json:"sensor_suite,omitempty" swaggertype:"object"`
	ROSTopics    []string        `json:"ros_topics"`
	Capabilities json.RawMessage `json:"capabilities,omitempty" swaggertype:"object"`
}

// RobotTypeResponse represents a robot type in the response.
type RobotTypeResponse struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	Model        string           `json:"model"`
	Manufacturer *string          `json:"manufacturer,omitempty"`
	EndEffector  *string          `json:"end_effector,omitempty"`
	SensorSuite  *json.RawMessage `json:"sensor_suite,omitempty" swaggertype:"object"`
	ROSTopics    []string         `json:"ros_topics"`
	Capabilities *json.RawMessage `json:"capabilities,omitempty" swaggertype:"object"`
	CreatedAt    string           `json:"created_at,omitempty"`
	UpdatedAt    string           `json:"updated_at,omitempty"`
}

// RobotTypeListResponse represents the response for listing robot types.
type RobotTypeListResponse struct {
	Items   []RobotTypeResponse `json:"items"`
	Total   int                 `json:"total"`
	Limit   int                 `json:"limit"`
	Offset  int                 `json:"offset"`
	HasNext bool                `json:"hasNext,omitempty"`
	HasPrev bool                `json:"hasPrev,omitempty"`
}

// RegisterRoutes registers robot type related routes.
func (h *RobotTypeHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robot_types", h.ListRobotTypes)
	apiV1.POST("/robot_types", h.CreateRobotType)
	apiV1.GET("/robot_types/:id", h.GetRobotType)
	apiV1.PUT("/robot_types/:id", h.UpdateRobotType)
	apiV1.DELETE("/robot_types/:id", h.DeleteRobotType)
}

// robotTypeRow represents a robot type in the database
type robotTypeRow struct {
	ID           int64          `db:"id"`
	Name         string         `db:"name"`
	Model        string         `db:"model"`
	Manufacturer sql.NullString `db:"manufacturer"`
	EndEffector  sql.NullString `db:"end_effector"`
	SensorSuite  sql.NullString `db:"sensor_suite"`
	ROSTopics    sql.NullString `db:"ros_topics"`
	Capabilities sql.NullString `db:"capabilities"`
	CreatedAt    sql.NullTime   `db:"created_at"`
	UpdatedAt    sql.NullTime   `db:"updated_at"`
}

func sqlNullStringFromOptionalPtr(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{Valid: false}
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: v, Valid: true}
}

// jsonColumnForCreate stores sensor_suite/capabilities on INSERT.
// If the client omits the field or sends empty/null, defaults to {}.
func jsonColumnForCreate(raw json.RawMessage) sql.NullString {
	return sql.NullString{String: jsonStringOrEmptyObject(raw), Valid: true}
}

func stringPtrFromNull(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func ptrRawJSONFromNull(ns sql.NullString) *json.RawMessage {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	r := json.RawMessage(ns.String)
	return &r
}

func robotTypeRowToResponse(rt robotTypeRow) RobotTypeResponse {
	var topics []string
	if rt.ROSTopics.Valid && rt.ROSTopics.String != "" {
		topics = parseJSONArray(rt.ROSTopics.String)
	}

	createdAt := ""
	if rt.CreatedAt.Valid {
		createdAt = rt.CreatedAt.Time.UTC().Format(time.RFC3339)
	}

	updatedAt := ""
	if rt.UpdatedAt.Valid {
		updatedAt = rt.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	return RobotTypeResponse{
		ID:           rt.ID,
		Name:         rt.Name,
		Model:        rt.Model,
		Manufacturer: stringPtrFromNull(rt.Manufacturer),
		EndEffector:  stringPtrFromNull(rt.EndEffector),
		SensorSuite:  ptrRawJSONFromNull(rt.SensorSuite),
		ROSTopics:    topics,
		Capabilities: ptrRawJSONFromNull(rt.Capabilities),
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
	}
}

const robotTypeSelectColumns = `
			id,
			name,
			model,
			manufacturer,
			end_effector,
			sensor_suite,
			ros_topics,
			capabilities,
			created_at,
			updated_at`

// CreateRobotType handles robot type creation requests.
//
// @Summary      Create robot type
// @Description  Creates a new robot type with only required fields
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        body  body      CreateRobotTypeRequest   true  "Robot type payload"
// @Success      201   {object}  RobotTypeResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /robot_types [post]
func (h *RobotTypeHandler) CreateRobotType(c *gin.Context) {
	var req CreateRobotTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Model = strings.TrimSpace(req.Model)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO robot_types (
			name,
			model,
			manufacturer,
			end_effector,
			sensor_suite,
			ros_topics,
			capabilities,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name,
		req.Model,
		sqlNullStringFromOptionalPtr(req.Manufacturer),
		sqlNullStringFromOptionalPtr(req.EndEffector),
		jsonColumnForCreate(req.SensorSuite),
		toNullableJSONArray(req.ROSTopics),
		jsonColumnForCreate(req.Capabilities),
		now,
		now,
	)
	if err != nil {
		logger.Printf("[ROBOT] Failed to insert robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot type"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot type"})
		return
	}

	var rt robotTypeRow
	err = h.db.Get(&rt, `SELECT `+robotTypeSelectColumns+` FROM robot_types WHERE id = ?`, id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch created robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot type"})
		return
	}

	c.JSON(http.StatusCreated, robotTypeRowToResponse(rt))
}

// ListRobotTypes handles robot type listing requests.
//
// @Summary      List robot types
// @Description  Lists all robot types with pagination
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        limit  query     int  false  "Max results (default 50, max 100)"
// @Param        offset query     int  false  "Pagination offset (default 0)"
// @Success      200 {object} RobotTypeListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types [get]
func (h *RobotTypeHandler) ListRobotTypes(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	countQuery := "SELECT COUNT(*) FROM robot_types WHERE deleted_at IS NULL"
	var total int
	if err := h.db.Get(&total, countQuery); err != nil {
		logger.Printf("[ROBOT] Failed to count robot types: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot types"})
		return
	}

	query := `
		SELECT ` + robotTypeSelectColumns + `
		FROM robot_types
		WHERE deleted_at IS NULL
		ORDER BY id DESC
		LIMIT ? OFFSET ?
	`

	var dbRows []robotTypeRow
	if err := h.db.Select(&dbRows, query, pagination.Limit, pagination.Offset); err != nil {
		logger.Printf("[ROBOT] Failed to query robot types: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot types"})
		return
	}

	robotTypes := make([]RobotTypeResponse, 0, len(dbRows))
	for _, rt := range dbRows {
		robotTypes = append(robotTypes, robotTypeRowToResponse(rt))
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, RobotTypeListResponse{
		Items:   robotTypes,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

func toNullableJSONArray(values []string) sql.NullString {
	if len(values) == 0 {
		return sql.NullString{String: "[]", Valid: true}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

// GetRobotType handles getting a single robot type by ID.
//
// @Summary      Get robot type
// @Description  Gets a robot type by ID
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Robot Type ID"
// @Success      200  {object}  RobotTypeResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robot_types/{id} [get]
func (h *RobotTypeHandler) GetRobotType(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot type id"})
		return
	}

	query := `
		SELECT ` + robotTypeSelectColumns + `
		FROM robot_types
		WHERE id = ? AND deleted_at IS NULL
	`

	var rt robotTypeRow
	if err := h.db.Get(&rt, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "robot type not found"})
			return
		}
		logger.Printf("[ROBOT] Failed to query robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get robot type"})
		return
	}

	c.JSON(http.StatusOK, robotTypeRowToResponse(rt))
}

// UpdateRobotTypeRequest represents the request body for updating a robot type.
type UpdateRobotTypeRequest struct {
	Name         *string          `json:"name,omitempty"`
	Model        *string          `json:"model,omitempty"`
	Manufacturer *string          `json:"manufacturer,omitempty"`
	EndEffector  *string          `json:"end_effector,omitempty"`
	SensorSuite  *json.RawMessage `json:"sensor_suite,omitempty" swaggertype:"object"`
	ROSTopics    *[]string        `json:"ros_topics,omitempty"`
	Capabilities *json.RawMessage `json:"capabilities,omitempty" swaggertype:"object"`
}

// UpdateRobotType handles updating a robot type.
//
// @Summary      Update robot type
// @Description  Updates an existing robot type
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        id   path      string                    true  "Robot Type ID"
// @Param        body body      UpdateRobotTypeRequest    true  "Robot Type payload"
// @Success      200  {object}  RobotTypeResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robot_types/{id} [put]
func (h *RobotTypeHandler) UpdateRobotType(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot type id"})
		return
	}

	var req UpdateRobotTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name != "" {
			updates = append(updates, "name = ?")
			args = append(args, name)
		}
	}

	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		if model != "" {
			updates = append(updates, "model = ?")
			args = append(args, model)
		}
	}

	if req.Manufacturer != nil {
		v := strings.TrimSpace(*req.Manufacturer)
		if v == "" {
			updates = append(updates, "manufacturer = NULL")
		} else {
			updates = append(updates, "manufacturer = ?")
			args = append(args, v)
		}
	}

	if req.EndEffector != nil {
		v := strings.TrimSpace(*req.EndEffector)
		if v == "" {
			updates = append(updates, "end_effector = NULL")
		} else {
			updates = append(updates, "end_effector = ?")
			args = append(args, v)
		}
	}

	if req.SensorSuite != nil {
		raw := *req.SensorSuite
		updates = append(updates, "sensor_suite = ?")
		args = append(args, jsonStringOrEmptyObject(raw))
	}

	if req.ROSTopics != nil {
		updates = append(updates, "ros_topics = ?")
		args = append(args, toNullableJSONArray(*req.ROSTopics))
	}

	if req.Capabilities != nil {
		raw := *req.Capabilities
		updates = append(updates, "capabilities = ?")
		args = append(args, jsonStringOrEmptyObject(raw))
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE robot_types SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	result, err := h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[ROBOT] Failed to update robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot type"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot type not found"})
		return
	}

	var rt robotTypeRow
	err = h.db.Get(&rt, "SELECT "+robotTypeSelectColumns+" FROM robot_types WHERE id = ?", id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch updated robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated robot type"})
		return
	}

	c.JSON(http.StatusOK, robotTypeRowToResponse(rt))
}

// DeleteRobotType handles robot type deletion requests (soft delete).
//
// @Summary      Delete robot type
// @Description  Soft deletes a robot type by ID
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Robot Type ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{id} [delete]
func (h *RobotTypeHandler) DeleteRobotType(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot type id"})
		return
	}

	// Check if robot type exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to check robot type existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot type"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot type not found"})
		return
	}

	var robotsUseType bool
	err = h.db.Get(&robotsUseType, "SELECT EXISTS(SELECT 1 FROM robots WHERE robot_type_id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to check robots referencing robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot type"})
		return
	}
	if robotsUseType {
		c.JSON(http.StatusConflict, gin.H{"error": "robot type is in used by one or more robots"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE robot_types SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to delete robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot type"})
		return
	}

	c.Status(http.StatusNoContent)
}
