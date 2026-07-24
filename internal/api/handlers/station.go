// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"crypto/rand"
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

// StationHandler handles station (workstation) related HTTP requests.
type StationHandler struct {
	db *sqlx.DB
}

// NewStationHandler creates a new StationHandler.
func NewStationHandler(db *sqlx.DB) *StationHandler {
	return &StationHandler{db: db}
}

// CreateStationRequest represents the request body for creating a station.
type CreateStationRequest struct {
	RobotID         string      `json:"robot_id"`
	DataCollectorID string      `json:"data_collector_id"`
	Metadata        interface{} `json:"metadata,omitempty"`
}

// UpdateStationRequest represents mutable station fields.
type UpdateStationRequest struct {
	RobotID         *string         `json:"robot_id,omitempty"`
	DataCollectorID *string         `json:"data_collector_id,omitempty"`
	Status          *string         `json:"status,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// StationResponse represents a station in the response.
type StationResponse struct {
	ID                  string      `json:"id"`
	RobotID             string      `json:"robot_id"`
	RobotName           string      `json:"robot_name,omitempty"`
	RobotSerial         string      `json:"robot_serial,omitempty"`
	RobotDeviceName     string      `json:"robot_device_name,omitempty"`
	DataCollectorID     string      `json:"data_collector_id"`
	CollectorName       string      `json:"collector_name,omitempty"`
	CollectorOperatorID string      `json:"collector_operator_id,omitempty"`
	WorkspaceID         string      `json:"workspace_id"`
	WorkspaceName       string      `json:"workspace_name,omitempty"`
	Status              string      `json:"status"`
	Name                string      `json:"name"`
	IsCurrent           bool        `json:"is_current"`
	SupersededBy        string      `json:"superseded_by,omitempty"`
	Metadata            interface{} `json:"metadata,omitempty"`
	CreatedAt           string      `json:"created_at"`
	UpdatedAt           string      `json:"updated_at"`
}

func stationMetadataFromDB(ns sql.NullString) interface{} {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	return parseJSONRaw(ns.String)
}

const (
	stationNamePrefix   = "ws-"
	stationNameRandLen  = 8
	stationNameRetries  = 20
	stationNameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func randomStationNameSuffix() (string, error) {
	raw := make([]byte, stationNameRandLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, stationNameRandLen)
	for i := range out {
		out[i] = stationNameAlphabet[int(raw[i])%len(stationNameAlphabet)]
	}
	return string(out), nil
}

func (h *StationHandler) allocateStationName() (string, error) {
	for i := 0; i < stationNameRetries; i++ {
		suffix, err := randomStationNameSuffix()
		if err != nil {
			return "", err
		}
		name := stationNamePrefix + suffix
		var exists bool
		if err := h.db.Get(&exists,
			"SELECT EXISTS(SELECT 1 FROM workstations WHERE name = ? AND deleted_at IS NULL)", name); err != nil {
			return "", err
		}
		if !exists {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not allocate unique station name")
}

// RegisterRoutes registers station related routes.
func (h *StationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/stations/lookup", h.LookupStations)
	apiV1.POST("/stations", h.CreateStation)
	apiV1.GET("/stations", h.ListStations)
	apiV1.GET("/stations/:id", h.GetStation)
	apiV1.PUT("/stations/:id", h.UpdateStation)
	apiV1.DELETE("/stations/:id", h.DeleteStation)
}

// robotInfoRow represents robot info retrieved from DB
type robotInfoRow struct {
	ID          int64  `db:"id"`
	DeviceID    string `db:"device_id"`
	WorkspaceID int64  `db:"workspace_id"`
	Status      string `db:"status"`
}

// dataCollectorInfoRow represents data collector info retrieved from DB
type dataCollectorInfoRow struct {
	ID         int64  `db:"id"`
	Name       string `db:"name"`
	OperatorID string `db:"operator_id"`
	Status     string `db:"status"`
}

// CreateStation handles station creation requests.
//
// @Summary      Create station
// @Description  Creates a new station by pairing a robot with a data collector
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        body  body      CreateStationRequest  true  "Station payload"
// @Success      201   {object}  StationResponse
// @Failure      400   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /stations [post]
func (h *StationHandler) CreateStation(c *gin.Context) {
	var req CreateStationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.RobotID = strings.TrimSpace(req.RobotID)
	req.DataCollectorID = strings.TrimSpace(req.DataCollectorID)

	// Validate required fields
	if req.RobotID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_id is required"})
		return
	}

	if req.DataCollectorID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector_id is required"})
		return
	}

	// Parse robot_id (robots.id)
	robotIDStr := strings.TrimPrefix(req.RobotID, "robot_")
	robotID, err := strconv.ParseInt(robotIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_id format"})
		return
	}

	var robotInfo robotInfoRow
	err = h.db.Get(&robotInfo, `
		SELECT id, device_id, workspace_id, status
		FROM robots 
		WHERE id = ? AND deleted_at IS NULL
	`, robotID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot not found"})
		return
	}
	if err != nil {
		logger.Printf("[STATION] Failed to query robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	// Validate robot status allows pairing (only 'active' status can be paired)
	if robotInfo.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot status must be active to be paired"})
		return
	}

	// Parse data_collector_id (data_collectors.id)
	dcIDStr := strings.TrimPrefix(req.DataCollectorID, "dc_")
	dcID, err := strconv.ParseInt(dcIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data_collector_id format"})
		return
	}

	var dcInfo dataCollectorInfoRow
	err = h.db.Get(&dcInfo, `
		SELECT id, name, operator_id, status
		FROM data_collectors 
		WHERE id = ? AND deleted_at IS NULL
	`, dcID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector not found"})
		return
	}
	if err != nil {
		logger.Printf("[STATION] Failed to query data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	// Validate data_collector status allows pairing (only 'active' status can be paired)
	if dcInfo.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector status must be active to be paired"})
		return
	}
	allowed, err := services.OperatorHasWorkspaceAccess(c.Request.Context(), h.db, dcInfo.OperatorID, robotInfo.WorkspaceID)
	if err != nil {
		logger.Printf("[STATION] Failed to check collector Workspace access: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}
	if !allowed {
		c.JSON(http.StatusConflict, gin.H{"error": "cross-workspace station binding is not allowed"})
		return
	}

	// Check if robot is already assigned to a current station.
	var existingStationID int64
	err = h.db.Get(&existingStationID, `
		SELECT id FROM workstations 
		WHERE robot_id = ? AND is_current = TRUE AND deleted_at IS NULL
	`, robotInfo.ID)
	if err == nil {
		// Robot is already assigned
		c.JSON(http.StatusConflict, gin.H{
			"error":   "ROBOT_ALREADY_ASSIGNED",
			"message": fmt.Sprintf("Robot robot_%d is already assigned to station ws_%d", robotInfo.ID, existingStationID),
		})
		return
	}
	if err != sql.ErrNoRows {
		logger.Printf("[STATION] Failed to check existing station for robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	// Check if data_collector is already assigned to a current station.
	err = h.db.Get(&existingStationID, `
		SELECT id FROM workstations 
		WHERE data_collector_id = ? AND workspace_id = ? AND is_current = TRUE AND deleted_at IS NULL
	`, dcInfo.ID, robotInfo.WorkspaceID)
	if err == nil {
		// Data collector is already assigned
		c.JSON(http.StatusConflict, gin.H{
			"error":   "DATA_COLLECTOR_ALREADY_ASSIGNED",
			"message": fmt.Sprintf("Data collector dc_%d is already assigned to station ws_%d", dcInfo.ID, existingStationID),
		})
		return
	}
	if err != sql.ErrNoRows {
		logger.Printf("[STATION] Failed to check existing station for data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	now := time.Now().UTC()

	metadataStr := sql.NullString{String: "{}", Valid: true}
	if req.Metadata != nil {
		metadataJSON, err := json.Marshal(req.Metadata)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
			return
		}
		metadataStr = sql.NullString{String: string(metadataJSON), Valid: true}
	}

	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[STATION] Failed to begin create transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			logger.Printf("[STATION] Transaction rollback error: %v", err)
		}
	}()

	var stationID int64
	err = tx.Get(&stationID, `
		SELECT id
		FROM workstations
		WHERE robot_id = ?
		  AND data_collector_id = ?
		  AND is_current = FALSE
		  AND deleted_at IS NULL
		ORDER BY updated_at DESC, id DESC
		LIMIT 1
	`, robotInfo.ID, dcInfo.ID)
	switch err {
	case nil:
		if _, err := tx.Exec(`
			UPDATE workstations
			SET
				robot_name = ?,
				robot_serial = ?,
				collector_name = ?,
				collector_operator_id = ?,
				workspace_id = ?,
				status = ?,
				is_current = TRUE,
				superseded_at = NULL,
				superseded_by = NULL,
				metadata = ?,
				updated_at = ?
			WHERE id = ? AND is_current = FALSE AND deleted_at IS NULL
		`,
			robotInfo.DeviceID,
			robotInfo.DeviceID,
			dcInfo.Name,
			dcInfo.OperatorID,
			robotInfo.WorkspaceID,
			"offline",
			metadataStr,
			now,
			stationID,
		); err != nil {
			logger.Printf("[STATION] Failed to reactivate workstation: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
			return
		}
	case sql.ErrNoRows:
		stationName, err := h.allocateStationName()
		if err != nil {
			logger.Printf("[STATION] Failed to allocate station name: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
			return
		}
		result, err := tx.Exec(`
			INSERT INTO workstations (
				robot_id,
				robot_name,
				robot_serial,
				data_collector_id,
				collector_name,
				collector_operator_id,
				workspace_id,
				name,
				status,
				is_current,
				metadata,
				created_at,
				updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			robotInfo.ID,
			robotInfo.DeviceID,
			robotInfo.DeviceID, // robot_serial from device_id
			dcInfo.ID,
			dcInfo.Name,       // collector_name
			dcInfo.OperatorID, // collector_operator_id
			robotInfo.WorkspaceID,
			stationName,
			"offline",
			true,
			metadataStr,
			now,
			now,
		)
		if err != nil {
			logger.Printf("[STATION] Failed to insert workstation: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
			return
		}
		stationID, err = result.LastInsertId()
		if err != nil {
			logger.Printf("[STATION] Failed to get inserted id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
			return
		}
	default:
		logger.Printf("[STATION] Failed to find reusable workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[STATION] Failed to commit station create: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	station, err := h.getStationResponseRow(stationID, true)
	if err != nil {
		logger.Printf("[STATION] Failed to fetch created station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}
	c.JSON(http.StatusCreated, stationResponseFromRow(station))
}

// stationListRow represents a station row from DB for listing
type stationListRow struct {
	ID                  int64          `db:"id"`
	RobotID             int64          `db:"robot_id"`
	RobotName           string         `db:"robot_name"`
	RobotSerial         string         `db:"robot_serial"`
	RobotMetadata       sql.NullString `db:"robot_metadata"`
	DataCollectorID     int64          `db:"data_collector_id"`
	CollectorName       string         `db:"collector_name"`
	CollectorOperatorID string         `db:"collector_operator_id"`
	WorkspaceID         int64          `db:"workspace_id"`
	WorkspaceName       sql.NullString `db:"workspace_name"`
	Name                sql.NullString `db:"name"`
	Status              string         `db:"status"`
	IsCurrent           bool           `db:"is_current"`
	SupersededBy        sql.NullInt64  `db:"superseded_by"`
	Metadata            sql.NullString `db:"metadata"`
	CreatedAt           sql.NullTime   `db:"created_at"`
	UpdatedAt           sql.NullTime   `db:"updated_at"`
}

func stationResponseFromRow(s stationListRow) StationResponse {
	name := ""
	if s.Name.Valid {
		name = s.Name.String
	}
	workspaceName := ""
	if s.WorkspaceName.Valid {
		workspaceName = s.WorkspaceName.String
	}
	supersededBy := ""
	if s.SupersededBy.Valid {
		supersededBy = fmt.Sprintf("%d", s.SupersededBy.Int64)
	}
	meta := stationMetadataFromDB(s.Metadata)
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	return StationResponse{
		ID:                  fmt.Sprintf("%d", s.ID),
		RobotID:             fmt.Sprintf("%d", s.RobotID),
		RobotName:           s.RobotName,
		RobotSerial:         s.RobotSerial,
		RobotDeviceName:     robotDeviceNameFromMetadata(s.RobotMetadata),
		DataCollectorID:     fmt.Sprintf("%d", s.DataCollectorID),
		CollectorName:       s.CollectorName,
		CollectorOperatorID: s.CollectorOperatorID,
		WorkspaceID:         fmt.Sprintf("%d", s.WorkspaceID),
		WorkspaceName:       workspaceName,
		Status:              s.Status,
		Name:                name,
		IsCurrent:           s.IsCurrent,
		SupersededBy:        supersededBy,
		Metadata:            meta,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
	}
}

func (h *StationHandler) getStationResponseRow(stationID int64, currentOnly bool) (stationListRow, error) {
	where := "WHERE ws.id = ? AND ws.deleted_at IS NULL"
	if currentOnly {
		where += " AND ws.is_current = TRUE"
	}

	var station stationListRow
	err := h.db.Get(&station, `
		SELECT
			ws.id, ws.robot_id, ws.robot_name, ws.robot_serial,
			r.metadata AS robot_metadata,
			ws.data_collector_id, ws.collector_name, ws.collector_operator_id,
			ws.workspace_id AS workspace_id, o.name AS workspace_name,
			ws.name, ws.status, ws.is_current, ws.superseded_by, ws.metadata, ws.created_at, ws.updated_at
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN workspaces o ON o.id = ws.workspace_id AND o.deleted_at IS NULL
		`+where, stationID)
	return station, err
}

// ListStations handles listing all stations.
//
// @Summary      List stations
// @Description  Returns a list of all workstations with pagination
// @Tags         stations
// @Produce      json
// @Param        workspace_id          query string false "Filter by Workspace ID(s), comma-separated"
// @Param        device_id             query string false "Filter by robot device ID(s), comma-separated"
// @Param        collector_name        query string false "Filter by collector name(s), comma-separated"
// @Param        collector_operator_id query string false "Filter by collector operator ID(s), comma-separated"
// @Param        status                query string false "Filter by status(es), comma-separated (active, inactive, break, offline)"
// @Param        is_current            query bool   false "Filter by current login binding state"
// @Param        keyword         query string false "Search by name, robot serial, robot name, collector operator ID, or collector name"
// @Param        q               query string false "Alias of keyword"
// @Param        search          query string false "Alias of keyword"
// @Param        limit  query int false "Max results (default 50, max 100)"
// @Param        offset query int false "Pagination offset (default 0)"
// @Success      200 {object} ListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /stations [get]
func (h *StationHandler) ListStations(c *gin.Context) {
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
	statuses, err := parseNonEmptyStringList(c.Query("status"), "status")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for i, status := range statuses {
		status = strings.ToLower(status)
		if _, ok := validStationStatuses[status]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		statuses[i] = status
	}
	deviceIDs, err := parseNonEmptyStringList(firstNonEmptyQuery(c, "device_id", "robot_serial"), "device_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	collectorNames, err := parseNonEmptyStringList(c.Query("collector_name"), "collector_name")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	collectorOperatorIDs, err := parseNonEmptyStringList(c.Query("collector_operator_id"), "collector_operator_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var isCurrent *bool
	if raw := strings.TrimSpace(c.Query("is_current")); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid is_current format"})
			return
		}
		isCurrent = &parsed
	}
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")
	stationSearchFields := []string{"ws.name", "ws.robot_serial", "ws.collector_operator_id", "ws.robot_name", "ws.collector_name", "r.metadata"}

	whereClause := "WHERE ws.deleted_at IS NULL AND ws.superseded_at IS NULL"
	args := []any{}
	if isCurrent != nil {
		whereClause += " AND ws.is_current = ?"
		args = append(args, *isCurrent)
	}
	whereClause, args = appendInt64InFilter(whereClause, args, "ws.workspace_id", workspaceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "ws.status", statuses)
	whereClause, args = appendStringInFilter(whereClause, args, "ws.robot_serial", deviceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "ws.collector_name", collectorNames)
	whereClause, args = appendStringInFilter(whereClause, args, "ws.collector_operator_id", collectorOperatorIDs)
	whereClause, args = appendKeywordSearch(whereClause, args, keyword, stationSearchFields...)

	countQuery := `
		SELECT COUNT(*)
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN workspaces o ON o.id = ws.workspace_id AND o.deleted_at IS NULL
		` + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[STATION] Failed to count stations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list stations"})
		return
	}

	var stations []stationListRow
	query := `
		SELECT 
			ws.id, ws.robot_id, ws.robot_name, ws.robot_serial,
			r.metadata AS robot_metadata,
			ws.data_collector_id, ws.collector_name, ws.collector_operator_id,
			ws.workspace_id AS workspace_id, o.name AS workspace_name,
			ws.name, ws.status, ws.is_current, ws.superseded_by, ws.metadata, ws.created_at, ws.updated_at
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN workspaces o ON o.id = ws.workspace_id AND o.deleted_at IS NULL
		` + whereClause + `
	`
	orderClause, orderArgs := keywordOrderBy(keyword, "ws.id DESC", stationSearchFields...)
	query += `
		` + orderClause + `
		LIMIT ? OFFSET ?
	`
	queryArgs := append(args, orderArgs...)
	queryArgs = append(queryArgs, pagination.Limit, pagination.Offset)

	err = h.db.Select(&stations, query, queryArgs...)
	if err != nil && err != sql.ErrNoRows {
		logger.Printf("[STATION] Failed to query stations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list stations"})
		return
	}

	if stations == nil {
		stations = []stationListRow{}
	}

	response := make([]StationResponse, 0, len(stations))
	for _, s := range stations {
		response = append(response, stationResponseFromRow(s))
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, ListResponse{
		Items:   response,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

const maxStationLookupIDs = 500

// LookupStationsRequest is the body for POST /stations/lookup.
type LookupStationsRequest struct {
	WorkstationIDs []any `json:"workstation_ids"`
}

// StationLookupItem is a workstation snapshot for admin/history views (includes soft-deleted rows).
type StationLookupItem struct {
	ID                  string `json:"id"`
	RobotID             string `json:"robot_id"`
	DataCollectorID     string `json:"data_collector_id"`
	WorkspaceID         string `json:"workspace_id"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	RobotName           string `json:"robot_name,omitempty"`
	RobotSerial         string `json:"robot_serial,omitempty"`
	RobotDeviceName     string `json:"robot_device_name,omitempty"`
	CollectorName       string `json:"collector_name,omitempty"`
	CollectorOperatorID string `json:"collector_operator_id,omitempty"`
	Deleted             bool   `json:"deleted"`
}

func parseWorkstationIDFromLookupAny(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		if x < 1 || x != float64(int64(x)) {
			return 0, false
		}
		return int64(x), true
	case string:
		s := strings.TrimSpace(x)
		s = strings.TrimPrefix(strings.TrimPrefix(s, "ws_"), "WS_")
		if s == "" {
			return 0, false
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	case json.Number:
		id, err := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	default:
		return 0, false
	}
}

// LookupStations returns workstation snapshots by id, including soft-deleted rows.
func (h *StationHandler) LookupStations(c *gin.Context) {
	var req LookupStationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(req.WorkstationIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"stations": []StationLookupItem{}})
		return
	}

	seen := make(map[int64]struct{})
	ids := make([]int64, 0, len(req.WorkstationIDs))
	for _, raw := range req.WorkstationIDs {
		id, ok := parseWorkstationIDFromLookupAny(raw)
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) >= maxStationLookupIDs {
			break
		}
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"stations": []StationLookupItem{}})
		return
	}

	query, args, err := sqlx.In(`
		SELECT
			ws.id, ws.robot_id,
			COALESCE(ws.robot_name, '') AS robot_name,
			COALESCE(ws.robot_serial, '') AS robot_serial,
			r.metadata AS robot_metadata,
			ws.data_collector_id,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(ws.collector_operator_id, '') AS collector_operator_id,
			ws.workspace_id AS workspace_id,
			ws.name, ws.status,
			ws.deleted_at
		FROM workstations ws
		LEFT JOIN robots r ON r.id = ws.robot_id
		WHERE ws.id IN (?)
	`, ids)
	if err != nil {
		logger.Printf("[STATION] Failed to build lookup query: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to lookup stations"})
		return
	}
	query = h.db.Rebind(query)

	type lookupRow struct {
		ID                  int64          `db:"id"`
		RobotID             int64          `db:"robot_id"`
		RobotName           string         `db:"robot_name"`
		RobotSerial         string         `db:"robot_serial"`
		RobotMetadata       sql.NullString `db:"robot_metadata"`
		DataCollectorID     int64          `db:"data_collector_id"`
		CollectorName       string         `db:"collector_name"`
		CollectorOperatorID string         `db:"collector_operator_id"`
		WorkspaceID         int64          `db:"workspace_id"`
		Name                sql.NullString `db:"name"`
		Status              string         `db:"status"`
		DeletedAt           sql.NullTime   `db:"deleted_at"`
	}

	var rows []lookupRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[STATION] Failed to lookup stations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to lookup stations"})
		return
	}

	out := make([]StationLookupItem, 0, len(rows))
	for _, r := range rows {
		name := ""
		if r.Name.Valid {
			name = strings.TrimSpace(r.Name.String)
		}
		out = append(out, StationLookupItem{
			ID:                  fmt.Sprintf("%d", r.ID),
			RobotID:             fmt.Sprintf("%d", r.RobotID),
			DataCollectorID:     fmt.Sprintf("%d", r.DataCollectorID),
			WorkspaceID:         fmt.Sprintf("%d", r.WorkspaceID),
			Name:                name,
			Status:              r.Status,
			RobotName:           strings.TrimSpace(r.RobotName),
			RobotSerial:         strings.TrimSpace(r.RobotSerial),
			RobotDeviceName:     robotDeviceNameFromMetadata(r.RobotMetadata),
			CollectorName:       strings.TrimSpace(r.CollectorName),
			CollectorOperatorID: strings.TrimSpace(r.CollectorOperatorID),
			Deleted:             r.DeletedAt.Valid,
		})
	}

	c.JSON(http.StatusOK, gin.H{"stations": out})
}

// validStationStatuses contains all valid station status values
var validStationStatuses = map[string]bool{
	"active":   true,
	"inactive": true,
	"break":    true,
	"offline":  true,
}

// parseStationPathID parses a station id from the URL path (decimal string, e.g. "12").
func parseStationPathID(stationIDStr string) (int64, error) {
	s := strings.TrimSpace(stationIDStr)
	if s == "" {
		return 0, fmt.Errorf("empty station id")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, fmt.Errorf("station id must be positive")
	}
	return id, nil
}

// GetStation handles getting a single station by ID.
//
// @Summary      Get station
// @Description  Gets a station by ID
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Station ID (numeric, e.g., 1)"
// @Success      200  {object}  StationResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /stations/{id} [get]
func (h *StationHandler) GetStation(c *gin.Context) {
	stationIDStr := c.Param("id")

	stationID, err := parseStationPathID(stationIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected numeric id"})
		return
	}

	station, err := h.getStationResponseRow(stationID, false)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
			return
		}
		logger.Printf("[STATION] Failed to query station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get station"})
		return
	}

	c.JSON(http.StatusOK, stationResponseFromRow(station))
}

// UpdateStation updates a current station without legacy production metadata.
//
// @Summary      Update station
// @Description  Updates station bindings, status, or metadata by ID
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        id   path string               true "Station ID"
// @Param        body body UpdateStationRequest true "Station update payload"
// @Success      200  {object} StationResponse
// @Failure      400  {object} map[string]string
// @Failure      404  {object} map[string]string
// @Failure      409  {object} map[string]string
// @Failure      500  {object} map[string]string
// @Router       /stations/{id} [put]
func (h *StationHandler) UpdateStation(c *gin.Context) {
	stationID, err := parseStationPathID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected numeric id"})
		return
	}

	var req UpdateStationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.RobotID == nil && req.DataCollectorID == nil && req.Status == nil && len(req.Metadata) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	ctx := c.Request.Context()
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		logger.Printf("[STATION] Failed to begin update transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	lockClause := " FOR UPDATE"
	if tx.DriverName() == "sqlite" {
		lockClause = ""
	}

	var current struct {
		RobotID         int64  `db:"robot_id"`
		DataCollectorID int64  `db:"data_collector_id"`
		WorkspaceID     int64  `db:"workspace_id"`
		Status          string `db:"status"`
	}
	if err := tx.GetContext(ctx, &current, `
		SELECT robot_id, data_collector_id, workspace_id, status
		FROM workstations
		WHERE id = ? AND is_current = TRUE AND deleted_at IS NULL
	`+lockClause, stationID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
			return
		}
		logger.Printf("[STATION] Failed to query station for update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	robotID := current.RobotID
	if req.RobotID != nil {
		robotID, err = parseStationBindingID(*req.RobotID, "robot_")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_id format"})
			return
		}
	}
	collectorID := current.DataCollectorID
	if req.DataCollectorID != nil {
		collectorID, err = parseStationBindingID(*req.DataCollectorID, "dc_")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data_collector_id format"})
			return
		}
	}

	var robot robotInfoRow
	if err := tx.GetContext(ctx, &robot, `
		SELECT id, device_id, workspace_id, status
		FROM robots WHERE id = ? AND deleted_at IS NULL
	`, robotID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot not found"})
			return
		}
		logger.Printf("[STATION] Failed to query update robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	if robot.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot status must be active to be paired"})
		return
	}

	var collector dataCollectorInfoRow
	if err := tx.GetContext(ctx, &collector, `
		SELECT id, name, operator_id, status
		FROM data_collectors WHERE id = ? AND deleted_at IS NULL
	`, collectorID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector not found"})
			return
		}
		logger.Printf("[STATION] Failed to query update collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	if collector.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector status must be active to be paired"})
		return
	}
	allowed, err := services.OperatorHasWorkspaceAccess(ctx, tx, collector.OperatorID, robot.WorkspaceID)
	if err != nil {
		logger.Printf("[STATION] Failed to check update collector Workspace access: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	if !allowed {
		c.JSON(http.StatusConflict, gin.H{"error": "cross-workspace station binding is not allowed"})
		return
	}

	if robotID != current.RobotID || collectorID != current.DataCollectorID {
		hasBlockingTask, taskErr := stationHasBlockingTasks(ctx, tx, stationID)
		if taskErr != nil {
			logger.Printf("[STATION] Failed to check tasks before rebinding station %d: %v", stationID, taskErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		if hasBlockingTask {
			c.JSON(http.StatusConflict, gin.H{
				"error": "cannot change station binding while tasks are pending or active",
			})
			return
		}

		var conflict bool
		if err := tx.GetContext(ctx, &conflict, `
			SELECT EXISTS(
				SELECT 1 FROM workstations
				WHERE id != ? AND is_current = TRUE AND deleted_at IS NULL
				  AND (robot_id = ? OR (data_collector_id = ? AND workspace_id = ?))
			)
		`, stationID, robotID, collectorID, robot.WorkspaceID); err != nil {
			logger.Printf("[STATION] Failed to check update binding conflict: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		if conflict {
			c.JSON(http.StatusConflict, gin.H{"error": "robot or data collector is already assigned"})
			return
		}
	}

	status := current.Status
	if req.Status != nil {
		status = strings.TrimSpace(*req.Status)
		if !validStationStatuses[status] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
	}

	updateMetadata := false
	var metadata any
	if len(req.Metadata) > 0 {
		updateMetadata = true
		metadataJSON := strings.TrimSpace(string(req.Metadata))
		if metadataJSON != "null" && !json.Valid(req.Metadata) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
			return
		}
		if metadataJSON != "null" {
			metadata = metadataJSON
		}
	}
	//nolint:gosec // G701 false positive: the SQL is static and all request values use placeholders.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workstations
		SET robot_id = ?, robot_name = ?, robot_serial = ?,
			data_collector_id = ?, collector_name = ?, collector_operator_id = ?,
			workspace_id = ?, status = ?, updated_at = ?,
			metadata = CASE WHEN ? THEN ? ELSE metadata END
		WHERE id = ? AND is_current = TRUE AND deleted_at IS NULL
	`,
		robot.ID, robot.DeviceID, robot.DeviceID,
		collector.ID, collector.Name, collector.OperatorID,
		robot.WorkspaceID, status, time.Now().UTC(),
		updateMetadata, metadata, stationID,
	); err != nil {
		logger.Printf("[STATION] Failed to update station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	if err := tx.Commit(); err != nil {
		logger.Printf("[STATION] Failed to commit station update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	row, err := h.getStationResponseRow(stationID, true)
	if err != nil {
		logger.Printf("[STATION] Failed to fetch updated station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	c.JSON(http.StatusOK, stationResponseFromRow(row))
}

func parseStationBindingID(raw string, prefix string) (int64, error) {
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), prefix))
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid binding id")
	}
	return id, nil
}

func stationHasBlockingTasks(ctx context.Context, q sqlx.QueryerContext, stationID int64) (bool, error) {
	var hasBlockingTask bool
	err := sqlx.GetContext(ctx, q, &hasBlockingTask, `
		SELECT EXISTS(
			SELECT 1 FROM tasks
			WHERE workstation_id = ? AND deleted_at IS NULL
			  AND status IN ('pending', 'ready', 'in_progress', 'uploading')
		)
	`, stationID)
	if err != nil {
		return false, fmt.Errorf("query blocking station tasks: %w", err)
	}
	return hasBlockingTask, nil
}

// DeleteStation handles station deletion requests by unbinding the current station.
//
// @Summary      Delete station
// @Description  Unbinds a current station by ID
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Station ID (numeric, e.g. 1)"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /stations/{id} [delete]
func (h *StationHandler) DeleteStation(c *gin.Context) {
	stationIDStr := c.Param("id")

	stationID, err := parseStationPathID(stationIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected numeric id"})
		return
	}

	// Check if station exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM workstations WHERE id = ? AND is_current = TRUE AND deleted_at IS NULL)", stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to check station existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
		return
	}

	hasBlockingTask, err := stationHasBlockingTasks(c.Request.Context(), h.db, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to check tasks for station %d: %v", stationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}
	if hasBlockingTask {
		c.JSON(http.StatusConflict, gin.H{
			"error": "cannot unbind station while tasks are pending or active",
		})
		return
	}

	now := time.Now().UTC()

	_, err = h.db.Exec(`
		UPDATE workstations
		SET is_current = FALSE, superseded_at = ?, superseded_by = NULL, updated_at = ?
		WHERE id = ? AND is_current = TRUE AND deleted_at IS NULL
	`, now, now, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to unbind station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}

	c.Status(http.StatusNoContent)
}
