// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"bytes"
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

// RobotHandler handles robot related HTTP requests.
type RobotHandler struct {
	db          *sqlx.DB
	recorderHub *services.RecorderHub
	transferHub *services.TransferHub
}

// NewRobotHandler creates a new RobotHandler.
func NewRobotHandler(db *sqlx.DB, recorderHub *services.RecorderHub, transferHub *services.TransferHub) *RobotHandler {
	return &RobotHandler{
		db:          db,
		recorderHub: recorderHub,
		transferHub: transferHub,
	}
}

// RobotResponse represents a robot in the response.
type RobotResponse struct {
	ID          string      `json:"id"`
	RobotTypeID string      `json:"robot_type_id"`
	DeviceID    string      `json:"device_id"`
	FactoryID   string      `json:"factory_id"`
	AssetID     string      `json:"asset_id,omitempty"`
	Status      string      `json:"status"`
	Metadata    interface{} `json:"metadata,omitempty"`
	CreatedAt   string      `json:"created_at,omitempty"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
	Connected   bool        `json:"connected"`
	ConnectedAt string      `json:"connected_at,omitempty"`
}

// RobotListResponse represents the response for listing robots.
type RobotListResponse struct {
	Items   []RobotResponse `json:"items"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	HasNext bool            `json:"hasNext,omitempty"`
	HasPrev bool            `json:"hasPrev,omitempty"`
}

// DeviceConnectionResponse is an in-memory connection snapshot keyed by Axon device_id (no database access).
type DeviceConnectionResponse struct {
	DeviceID          string `json:"device_id"`
	Connected         bool   `json:"connected"`
	ConnectedAt       string `json:"connected_at,omitempty"`
	RecorderConnected bool   `json:"recorder_connected"`
	TransferConnected bool   `json:"transfer_connected"`
}

// CreateRobotRequest represents the request body for creating a robot.
type CreateRobotRequest struct {
	RobotTypeID string      `json:"robot_type_id"`
	DeviceID    string      `json:"device_id"`
	FactoryID   string      `json:"factory_id"`
	AssetID     string      `json:"asset_id,omitempty"`
	Metadata    interface{} `json:"metadata,omitempty"`
}

// CreateRobotResponse represents the response for creating a robot.
type CreateRobotResponse struct {
	ID          string      `json:"id"`
	RobotTypeID string      `json:"robot_type_id"`
	DeviceID    string      `json:"device_id"`
	FactoryID   string      `json:"factory_id"`
	AssetID     string      `json:"asset_id,omitempty"`
	Status      string      `json:"status"`
	Metadata    interface{} `json:"metadata,omitempty"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
}

// RegisterRoutes registers robot related routes.
func (h *RobotHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robots", h.ListRobots)
	apiV1.POST("/robots", h.CreateRobot)
	apiV1.GET("/devices/:device_id/connection", h.GetDeviceConnection)
	apiV1.GET("/robots/:id", h.GetRobot)
	apiV1.PUT("/robots/:id", h.UpdateRobot)
	apiV1.DELETE("/robots/:id", h.DeleteRobot)
}

// robotRow represents a robot in the database
type robotRow struct {
	ID          int64          `db:"id"`
	RobotTypeID int64          `db:"robot_type_id"`
	DeviceID    string         `db:"device_id"`
	FactoryID   int64          `db:"factory_id"`
	AssetID     sql.NullString `db:"asset_id"`
	Status      string         `db:"status"`
	Metadata    sql.NullString `db:"metadata"`
	CreatedAt   sql.NullTime   `db:"created_at"`
	UpdatedAt   sql.NullTime   `db:"updated_at"`
}

func robotMetadataFromDB(ns sql.NullString) interface{} {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	return parseJSONRaw(ns.String)
}

func (h *RobotHandler) connectionState(deviceID string) (connected bool, connectedAt string) {
	connected, connectedAt, _, _ = h.connectionStateDetailed(deviceID)
	return connected, connectedAt
}

// connectionStateDetailed returns hub presence for recorder and transfer (no DB).
func (h *RobotHandler) connectionStateDetailed(deviceID string) (connected bool, connectedAt string, recorderConnected bool, transferConnected bool) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, "", false, false
	}
	if h.recorderHub == nil || h.transferHub == nil {
		return false, "", false, false
	}
	recConn := h.recorderHub.Get(deviceID)
	transConn := h.transferHub.Get(deviceID)
	recorderConnected = recConn != nil
	transferConnected = transConn != nil
	connected = recorderConnected && transferConnected
	if !connected {
		return false, "", recorderConnected, transferConnected
	}
	t := recConn.ConnectedAt
	if transConn.ConnectedAt.After(t) {
		t = transConn.ConnectedAt
	}
	return true, t.UTC().Format(time.RFC3339), recorderConnected, transferConnected
}

func (h *RobotHandler) responseFromRow(r robotRow) RobotResponse {
	createdAt := ""
	if r.CreatedAt.Valid {
		createdAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	assetID := ""
	if r.AssetID.Valid {
		assetID = r.AssetID.String
	}
	connected, connectedAt := h.connectionState(r.DeviceID)
	return RobotResponse{
		ID:          fmt.Sprintf("%d", r.ID),
		RobotTypeID: fmt.Sprintf("%d", r.RobotTypeID),
		DeviceID:    r.DeviceID,
		FactoryID:   fmt.Sprintf("%d", r.FactoryID),
		AssetID:     assetID,
		Status:      r.Status,
		Metadata:    robotMetadataFromDB(r.Metadata),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		Connected:   connected,
		ConnectedAt: connectedAt,
	}
}

// ListRobots handles robot listing requests with filtering.
//
// @Summary      List robots
// @Description  Lists robots with optional filtering by factory_id, status, robot_type_id, and connection status
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        factory_id    query     string  false  "Filter by factory id"
// @Param        status        query     string  false  "Filter by status (active, maintenance, retired)"
// @Param        robot_type_id query     string  false  "Filter by robot type id"
// @Param        connected     query     string  false  "Filter by connection status (true/false)"
// @Param        limit         query     int     false  "Max results (default 50, max 100)"
// @Param        offset        query     int     false  "Pagination offset (default 0)"
// @Success      200           {object}  RobotListResponse
// @Failure      400           {object}  map[string]string
// @Failure      500           {object}  map[string]string
// @Router       /robots [get]
func (h *RobotHandler) ListRobots(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	factoryID := c.Query("factory_id")
	status := c.Query("status")
	robotTypeID := c.Query("robot_type_id")
	connectedParam := c.Query("connected")

	var connectedFilter *bool
	if connectedParam != "" {
		connected, err := strconv.ParseBool(connectedParam)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connected format"})
			return
		}
		connectedFilter = &connected
	}

	whereClause := "WHERE r.deleted_at IS NULL"
	args := []interface{}{}

	if factoryID != "" {
		parsedFactoryID, err := strconv.ParseInt(factoryID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
			return
		}
		whereClause += " AND r.factory_id = ?"
		args = append(args, parsedFactoryID)
	}

	if status != "" {
		whereClause += " AND r.status = ?"
		args = append(args, status)
	}

	if robotTypeID != "" {
		parsedRobotTypeID, err := strconv.ParseInt(robotTypeID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
			return
		}
		whereClause += " AND r.robot_type_id = ?"
		args = append(args, parsedRobotTypeID)
	}

	countQuery := "SELECT COUNT(*) FROM robots r " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[ROBOT] Failed to count robots: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robots"})
		return
	}

	query := `
		SELECT 
			r.id,
			r.robot_type_id,
			r.device_id,
			r.factory_id,
			r.asset_id,
			r.status,
			r.metadata,
			r.created_at,
			r.updated_at
		FROM robots r
		` + whereClause + `
		ORDER BY r.id DESC
		LIMIT ? OFFSET ?
	`

	queryArgs := append(args, pagination.Limit, pagination.Offset)

	var dbRows []robotRow
	if err := h.db.Select(&dbRows, query, queryArgs...); err != nil {
		logger.Printf("[ROBOT] Failed to query robots: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robots"})
		return
	}

	robots := []RobotResponse{}
	for _, r := range dbRows {
		resp := h.responseFromRow(r)
		if connectedFilter != nil && resp.Connected != *connectedFilter {
			continue
		}
		robots = append(robots, resp)
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, RobotListResponse{
		Items:   robots,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// CreateRobot handles robot creation requests.
//
// @Summary      Create robot
// @Description  Registers a new robot
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        body  body      CreateRobotRequest  true  "Robot payload"
// @Success      201   {object}  CreateRobotResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /robots [post]
func (h *RobotHandler) CreateRobot(c *gin.Context) {
	var req CreateRobotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.RobotTypeID = strings.TrimSpace(req.RobotTypeID)
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.FactoryID = strings.TrimSpace(req.FactoryID)

	if req.RobotTypeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type_id is required"})
		return
	}

	if req.DeviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}

	if req.FactoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id is required"})
		return
	}

	// Parse robot_type_id as numeric value
	robotTypeID, err := strconv.ParseInt(req.RobotTypeID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
		return
	}

	// Verify robot_type exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", robotTypeID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type not found"})
		return
	}

	// Parse factory_id as direct number
	factoryID, err := strconv.ParseInt(req.FactoryID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
		return
	}

	// Verify factory exists
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
		return
	}

	now := time.Now().UTC()

	var assetIDStr sql.NullString
	if a := strings.TrimSpace(req.AssetID); a != "" {
		assetIDStr = sql.NullString{String: a, Valid: true}
	}

	metadataStr := sql.NullString{String: "{}", Valid: true}
	if req.Metadata != nil {
		metadataJSON, err := json.Marshal(req.Metadata)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
			return
		}
		metadataStr = sql.NullString{String: string(metadataJSON), Valid: true}
	}

	// Insert the robot
	result, err := h.db.Exec(
		`INSERT INTO robots (
			robot_type_id,
			device_id,
			factory_id,
			asset_id,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		robotTypeID,
		req.DeviceID,
		factoryID,
		assetIDStr,
		"active",
		metadataStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[ROBOT] Failed to insert robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[ROBOT] Failed to get inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
		return
	}

	var row robotRow
	err = h.db.Get(&row, `
		SELECT 
			r.id,
			r.robot_type_id,
			r.device_id,
			r.factory_id,
			r.asset_id,
			r.status,
			r.metadata,
			r.created_at,
			r.updated_at
		FROM robots r
		WHERE r.id = ? AND r.deleted_at IS NULL
	`, id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to load created robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
		return
	}

	resp := h.responseFromRow(row)
	c.JSON(http.StatusCreated, CreateRobotResponse{
		ID:          resp.ID,
		RobotTypeID: resp.RobotTypeID,
		DeviceID:    resp.DeviceID,
		FactoryID:   resp.FactoryID,
		AssetID:     resp.AssetID,
		Status:      resp.Status,
		Metadata:    resp.Metadata,
		CreatedAt:   resp.CreatedAt,
		UpdatedAt:   resp.UpdatedAt,
	})
}

// GetDeviceConnection returns recorder+transfer WebSocket presence for a device_id without touching the database.
//
// @Summary      Device connection status
// @Description  In-memory connection snapshot (RecorderHub + TransferHub). Same rules as GET /robots/:id field `connected`.
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Axon device id"
// @Success      200  {object}  DeviceConnectionResponse
// @Failure      400  {object}  map[string]string
// @Router       /devices/{device_id}/connection [get]
func (h *RobotHandler) GetDeviceConnection(c *gin.Context) {
	raw := c.Param("device_id")
	deviceID := strings.TrimSpace(raw)
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}
	connected, connectedAt, rec, trans := h.connectionStateDetailed(deviceID)
	c.JSON(http.StatusOK, DeviceConnectionResponse{
		DeviceID:          deviceID,
		Connected:         connected,
		ConnectedAt:       connectedAt,
		RecorderConnected: rec,
		TransferConnected: trans,
	})
}

// GetRobot handles getting a single robot by ID.
//
// @Summary      Get robot
// @Description  Gets a robot by ID
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Robot ID"
// @Success      200  {object}  RobotResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robots/{id} [get]
func (h *RobotHandler) GetRobot(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	query := `
		SELECT 
			r.id,
			r.robot_type_id,
			r.device_id,
			r.factory_id,
			r.asset_id,
			r.status,
			r.metadata,
			r.created_at,
			r.updated_at
		FROM robots r
		WHERE r.id = ? AND r.deleted_at IS NULL
	`

	var r robotRow
	if err := h.db.Get(&r, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
			return
		}
		logger.Printf("[ROBOT] Failed to query robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get robot"})
		return
	}

	c.JSON(http.StatusOK, h.responseFromRow(r))
}

// UpdateRobotRequest represents the request body for updating a robot.
// Metadata uses json.RawMessage so we can tell: key omitted (no change) vs explicit JSON null (store {}).
type UpdateRobotRequest struct {
	RobotTypeID *string         `json:"robot_type_id,omitempty"`
	DeviceID    *string         `json:"device_id,omitempty"`
	FactoryID   *string         `json:"factory_id,omitempty"`
	AssetID     *string         `json:"asset_id,omitempty"`
	Status      *string         `json:"status,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// UpdateRobot handles updating a robot.
//
// @Summary      Update robot
// @Description  Updates an existing robot
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        id   path      string           true  "Robot ID"
// @Param        body body      UpdateRobotRequest  true  "Robot payload"
// @Success      200  {object}  RobotResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robots/{id} [put]
func (h *RobotHandler) UpdateRobot(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	var req UpdateRobotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check if robot exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robots WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}

	// Validate status if provided
	validStatuses := map[string]bool{
		"active":      true,
		"maintenance": true,
		"retired":     true,
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	if req.RobotTypeID != nil {
		if *req.RobotTypeID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type_id cannot be empty"})
			return
		}
		parsedRobotTypeID, err := strconv.ParseInt(*req.RobotTypeID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
			return
		}
		// Verify robot_type exists
		var rtExists bool
		err = h.db.Get(&rtExists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", parsedRobotTypeID)
		if err != nil || !rtExists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type not found"})
			return
		}
		updates = append(updates, "robot_type_id = ?")
		args = append(args, parsedRobotTypeID)
	}

	if req.DeviceID != nil {
		deviceID := strings.TrimSpace(*req.DeviceID)
		if deviceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "device_id cannot be empty"})
			return
		}
		updates = append(updates, "device_id = ?")
		args = append(args, deviceID)
	}

	if req.FactoryID != nil {
		if *req.FactoryID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id cannot be empty"})
			return
		}
		parsedFactoryID, err := strconv.ParseInt(*req.FactoryID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
			return
		}
		// Verify factory exists
		var fExists bool
		err = h.db.Get(&fExists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", parsedFactoryID)
		if err != nil || !fExists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
			return
		}
		updates = append(updates, "factory_id = ?")
		args = append(args, parsedFactoryID)
	}

	if req.AssetID != nil {
		trimmed := strings.TrimSpace(*req.AssetID)
		var a sql.NullString
		if trimmed != "" {
			a = sql.NullString{String: trimmed, Valid: true}
		}
		updates = append(updates, "asset_id = ?")
		args = append(args, a)
	}

	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if !validStatuses[status] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status, must be one of: active, maintenance, retired"})
			return
		}
		updates = append(updates, "status = ?")
		args = append(args, status)
	}

	if len(req.Metadata) > 0 {
		meta := bytes.TrimSpace(req.Metadata)
		if bytes.Equal(meta, []byte("null")) {
			updates = append(updates, "metadata = ?")
			args = append(args, sql.NullString{String: "{}", Valid: true})
		} else {
			var probe interface{}
			if err := json.Unmarshal(req.Metadata, &probe); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
				return
			}
			updates = append(updates, "metadata = ?")
			args = append(args, sql.NullString{String: string(req.Metadata), Valid: true})
		}
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	// Determine which workstation denormalized columns need syncing.
	needsDeviceIDSync := req.DeviceID != nil && strings.TrimSpace(*req.DeviceID) != ""
	needsRobotTypeSync := req.RobotTypeID != nil && *req.RobotTypeID != ""
	needsFactorySync := req.FactoryID != nil && *req.FactoryID != ""

	updates = append(updates, "updated_at = ?")
	args = append(args, time.Now().UTC())
	args = append(args, id)

	query := fmt.Sprintf("UPDATE robots SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	tx, err := h.db.Begin()
	if err != nil {
		logger.Printf("[ROBOT] Failed to begin transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.Exec(query, args...); err != nil {
		logger.Printf("[ROBOT] Failed to update robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}

	// Sync denormalized columns in workstations if device_id, robot_type_id, or factory_id changed.
	if needsDeviceIDSync || needsRobotTypeSync || needsFactorySync {
		wsUpdates := []string{}
		wsArgs := []interface{}{}

		if needsDeviceIDSync {
			wsUpdates = append(wsUpdates, "robot_serial = ?")
			wsArgs = append(wsArgs, strings.TrimSpace(*req.DeviceID))
		}
		if needsRobotTypeSync {
			// Look up the robot_type name to keep robot_name in sync.
			parsedRobotTypeID, _ := strconv.ParseInt(*req.RobotTypeID, 10, 64)
			var rtName string
			if err := tx.QueryRow("SELECT name FROM robot_types WHERE id = ? AND deleted_at IS NULL", parsedRobotTypeID).Scan(&rtName); err != nil {
				logger.Printf("[ROBOT] Failed to fetch robot_type name for workstation sync: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
				return
			}
			wsUpdates = append(wsUpdates, "robot_name = ?")
			wsArgs = append(wsArgs, rtName)
		}
		if needsFactorySync {
			parsedFactoryID, _ := strconv.ParseInt(*req.FactoryID, 10, 64)
			wsUpdates = append(wsUpdates, "factory_id = ?")
			wsArgs = append(wsArgs, parsedFactoryID)
		}

		wsArgs = append(wsArgs, id)
		wsQuery := fmt.Sprintf(
			"UPDATE workstations SET %s WHERE robot_id = ? AND deleted_at IS NULL",
			strings.Join(wsUpdates, ", "),
		)
		if _, err := tx.Exec(wsQuery, wsArgs...); err != nil {
			logger.Printf("[ROBOT] Failed to sync workstation denormalized columns: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[ROBOT] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}

	// Fetch the updated robot
	var r robotRow
	err = h.db.Get(&r, `
		SELECT 
			r.id,
			r.robot_type_id,
			r.device_id,
			r.factory_id,
			r.asset_id,
			r.status,
			r.metadata,
			r.created_at,
			r.updated_at
		FROM robots r
		WHERE r.id = ? AND r.deleted_at IS NULL
	`, id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch updated robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated robot"})
		return
	}

	c.JSON(http.StatusOK, h.responseFromRow(r))
}

// DeleteRobot handles robot deletion requests (soft delete).
//
// @Summary      Delete robot
// @Description  Soft deletes a robot by ID
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Robot ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robots/{id} [delete]
func (h *RobotHandler) DeleteRobot(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	// Check if robot exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robots WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to check robot existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}

	var usedByStation bool
	err = h.db.Get(&usedByStation, "SELECT EXISTS(SELECT 1 FROM workstations WHERE robot_id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to check workstations referencing robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot"})
		return
	}
	if usedByStation {
		c.JSON(http.StatusConflict, gin.H{"error": "robot is assigned to one or more workstations"})
		return
	}

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE robots SET deleted_at = NOW(), updated_at = ? WHERE id = ? AND deleted_at IS NULL", time.Now().UTC(), id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to delete robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete robot"})
		return
	}

	c.Status(http.StatusNoContent)
}
