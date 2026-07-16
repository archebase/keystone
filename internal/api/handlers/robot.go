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
	"sort"
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
	ID           string      `json:"id"`
	DeviceID     string      `json:"device_id"`
	DeviceName   string      `json:"device_name,omitempty"`
	DeviceTypeID string      `json:"device_type_id,omitempty"`
	DeviceType   string      `json:"device_type,omitempty"`
	WorkspaceID  string      `json:"workspace_id"`
	Status       string      `json:"status"`
	Metadata     interface{} `json:"metadata,omitempty"`
	CreatedAt    string      `json:"created_at,omitempty"`
	UpdatedAt    string      `json:"updated_at,omitempty"`
	Connected    bool        `json:"connected"`
	ConnectedAt  string      `json:"connected_at,omitempty"`
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

type robotConnectionSnapshot map[string]string

// RegisterRoutes registers robot related routes.
func (h *RobotHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robots", h.ListRobots)
	apiV1.GET("/devices/:device_id/connection", h.GetDeviceConnection)
	apiV1.GET("/robots/:id", h.GetRobot)
}

// robotRow represents a robot in the database
type robotRow struct {
	ID           int64          `db:"id"`
	DeviceID     string         `db:"device_id"`
	DeviceTypeID sql.NullInt64  `db:"device_type_id"`
	DeviceType   sql.NullString `db:"device_type"`
	WorkspaceID  int64          `db:"workspace_id"`
	Status       string         `db:"status"`
	Metadata     sql.NullString `db:"metadata"`
	CreatedAt    sql.NullTime   `db:"created_at"`
	UpdatedAt    sql.NullTime   `db:"updated_at"`
}

func robotMetadataFromDB(ns sql.NullString) interface{} {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	return parseJSONRaw(ns.String)
}

func robotDeviceNameFromMetadata(ns sql.NullString) string {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(ns.String), &payload); err != nil {
		return ""
	}
	name, ok := payload["hilbert_dc_device_name"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

func (h *RobotHandler) loadRobotRow(id int64) (robotRow, error) { //nolint:unused // Kept for detail endpoints that share robot row loading.
	var r robotRow
	err := h.db.Get(&r, `
			SELECT
				r.id,
				r.device_id,
				r.device_type_id,
				r.device_type,
				r.workspace_id,
				r.status,
				r.metadata,
				r.created_at,
				r.updated_at
		FROM robots r
		WHERE r.id = ? AND r.deleted_at IS NULL
	`, id)
	return r, err
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

func (h *RobotHandler) connectionSnapshot() robotConnectionSnapshot {
	snapshot := robotConnectionSnapshot{}
	if h.recorderHub == nil || h.transferHub == nil {
		return snapshot
	}

	transferByDeviceID := make(map[string]time.Time)
	for _, device := range h.transferHub.ListDevices() {
		deviceID := strings.TrimSpace(device.DeviceID)
		if deviceID == "" {
			continue
		}
		transferByDeviceID[deviceID] = device.ConnectedAt
	}
	if len(transferByDeviceID) == 0 {
		return snapshot
	}

	for _, recorder := range h.recorderHub.ListDevices() {
		deviceID := strings.TrimSpace(recorder.DeviceID)
		if deviceID == "" {
			continue
		}
		transferConnectedAt, ok := transferByDeviceID[deviceID]
		if !ok {
			continue
		}
		connectedAt := recorder.ConnectedAt
		if transferConnectedAt.After(connectedAt) {
			connectedAt = transferConnectedAt
		}
		snapshot[deviceID] = connectedAt.UTC().Format(time.RFC3339)
	}
	return snapshot
}

func (s robotConnectionSnapshot) deviceIDs() []string {
	ids := make([]string, 0, len(s))
	for deviceID := range s {
		ids = append(ids, deviceID)
	}
	sort.Strings(ids)
	return ids
}

func appendRobotDeviceConnectionFilter(whereClause string, args []interface{}, connected bool, connectedDeviceIDs []string) (string, []interface{}) {
	if len(connectedDeviceIDs) == 0 {
		return whereClause, args
	}

	placeholders := make([]string, 0, len(connectedDeviceIDs))
	for _, deviceID := range connectedDeviceIDs {
		placeholders = append(placeholders, "?")
		args = append(args, deviceID)
	}

	operator := "NOT IN"
	if connected {
		operator = "IN"
	}
	return whereClause + " AND r.device_id " + operator + " (" + strings.Join(placeholders, ",") + ")", args
}

func robotResponseFromRow(r robotRow, connected bool, connectedAt string) RobotResponse {
	createdAt := ""
	if r.CreatedAt.Valid {
		createdAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if r.UpdatedAt.Valid {
		updatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	return RobotResponse{
		ID:           fmt.Sprintf("%d", r.ID),
		DeviceID:     r.DeviceID,
		DeviceName:   robotDeviceNameFromMetadata(r.Metadata),
		DeviceTypeID: robotDeviceTypeIDFromDB(r.DeviceTypeID),
		DeviceType:   strings.TrimSpace(r.DeviceType.String),
		WorkspaceID:  fmt.Sprintf("%d", r.WorkspaceID),
		Status:       r.Status,
		Metadata:     robotMetadataFromDB(r.Metadata),
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		Connected:    connected,
		ConnectedAt:  connectedAt,
	}
}

func robotDeviceTypeIDFromDB(ns sql.NullInt64) string {
	if !ns.Valid || ns.Int64 <= 0 {
		return ""
	}
	return strconv.FormatInt(ns.Int64, 10)
}

func (h *RobotHandler) responseFromRow(r robotRow) RobotResponse {
	connected, connectedAt := h.connectionState(r.DeviceID)
	return robotResponseFromRow(r, connected, connectedAt)
}

func responseFromRowWithConnectionSnapshot(r robotRow, snapshot robotConnectionSnapshot) RobotResponse {
	connectedAt, connected := snapshot[strings.TrimSpace(r.DeviceID)]
	return robotResponseFromRow(r, connected, connectedAt)
}

func parseConnectedFilter(raw string) (*bool, error) {
	values, err := parseNonEmptyStringList(raw, "connected")
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, nil
	}

	seen := make(map[bool]struct{})
	for _, value := range values {
		connected, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("invalid connected format")
		}
		seen[connected] = struct{}{}
	}
	if len(seen) != 1 {
		return nil, nil
	}

	result := false
	for connected := range seen {
		result = connected
	}
	return &result, nil
}

// ListRobots handles robot listing requests with filtering.
//
// @Summary      List robots
// @Description  Lists devices with optional Workspace, status, connection, device ID, and keyword filters
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        workspace_id  query     string  false  "Filter by Workspace ID(s), comma-separated"
// @Param        status        query     string  false  "Filter by status(es), comma-separated (active, maintenance, retired)"
// @Param        connected     query     string  false  "Filter by connection status(es), comma-separated (true/false)"
// @Param        device_id     query     string  false  "Filter by device ID(s), comma-separated"
// @Param        device_name   query     string  false  "Search by projected device name"
// @Param        keyword       query     string  false  "Search by device ID"
// @Param        q             query     string  false  "Alias of keyword"
// @Param        search        query     string  false  "Alias of keyword"
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

	workspaceIDs, err := parseNonNegativeInt64List(c.Query("workspace_id"), "workspace_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	connectedFilter, err := parseConnectedFilter(c.Query("connected"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	deviceIDs, err := parseNonEmptyStringList(c.Query("device_id"), "device_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	statuses, err := parseNonEmptyStringList(c.Query("status"), "status")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	deviceName := strings.TrimSpace(c.Query("device_name"))
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")

	whereClause := "WHERE r.deleted_at IS NULL"
	args := []interface{}{}
	whereClause, args = appendInt64InFilter(whereClause, args, "r.workspace_id", workspaceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "r.device_id", deviceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "r.status", statuses)

	if deviceName != "" {
		whereClause += " AND " + robotDeviceNameSQLExpr(h.db.DriverName()) + " LIKE ?"
		args = append(args, "%"+deviceName+"%")
	}
	if keyword != "" {
		likeKeyword := "%" + keyword + "%"
		whereClause += " AND r.device_id LIKE ?"
		args = append(args, likeKeyword)
	}

	connectionSnapshot := h.connectionSnapshot()
	if connectedFilter != nil {
		connectedDeviceIDs := connectionSnapshot.deviceIDs()
		if *connectedFilter && len(connectedDeviceIDs) == 0 {
			c.JSON(http.StatusOK, RobotListResponse{
				Items:   []RobotResponse{},
				Total:   0,
				Limit:   pagination.Limit,
				Offset:  pagination.Offset,
				HasNext: false,
				HasPrev: pagination.Offset > 0,
			})
			return
		}
		whereClause, args = appendRobotDeviceConnectionFilter(whereClause, args, *connectedFilter, connectedDeviceIDs)
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM robots r " + whereClause
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[ROBOT] Failed to count robots: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robots"})
		return
	}

	orderClause, orderArgs := keywordOrderBy(keyword, "r.id DESC", "r.device_id")
	query := `
		SELECT 
			r.id,
			r.device_id,
			r.device_type_id,
			r.device_type,
			r.workspace_id,
			r.status,
			r.metadata,
			r.created_at,
			r.updated_at
		FROM robots r
				` + whereClause + `
				` + orderClause + `
				LIMIT ? OFFSET ?
	`
	queryArgs := append(args, orderArgs...)
	queryArgs = append(queryArgs, pagination.Limit, pagination.Offset)

	var dbRows []robotRow
	if err := h.db.Select(&dbRows, query, queryArgs...); err != nil {
		logger.Printf("[ROBOT] Failed to query robots: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robots"})
		return
	}

	robots := make([]RobotResponse, 0, len(dbRows))
	for _, r := range dbRows {
		robots = append(robots, responseFromRowWithConnectionSnapshot(r, connectionSnapshot))
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

func robotDeviceNameSQLExpr(driverName string) string {
	switch driverName {
	case "mysql":
		return "COALESCE(CASE WHEN JSON_VALID(r.metadata) THEN JSON_UNQUOTE(JSON_EXTRACT(r.metadata, '$.hilbert_dc_device_name')) ELSE '' END, '')"
	default:
		return "COALESCE(CASE WHEN json_valid(r.metadata) THEN json_extract(r.metadata, '$.hilbert_dc_device_name') ELSE '' END, '')"
	}
}

// GetDeviceConnection returns recorder and transfer connection state for a device.
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
			r.device_id,
			r.device_type_id,
			r.device_type,
			r.workspace_id,
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
