// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"bytes"
	"crypto/rand"
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

// UpdateStationRequest represents the request body for updating a station.
// Optional fields use pointers / json.RawMessage so callers can omit keys.
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
	RobotTypeID         string      `json:"robot_type_id,omitempty"`
	RobotTypeModel      string      `json:"robot_type_model,omitempty"`
	DataCollectorID     string      `json:"data_collector_id"`
	CollectorName       string      `json:"collector_name,omitempty"`
	CollectorOperatorID string      `json:"collector_operator_id,omitempty"`
	FactoryID           string      `json:"factory_id"`
	FactoryName         string      `json:"factory_name,omitempty"`
	OrganizationID      string      `json:"organization_id"`
	OrganizationName    string      `json:"organization_name,omitempty"`
	Status              string      `json:"status"`
	Name                string      `json:"name"`
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
	FactoryID   int64  `db:"factory_id"`
	Status      string `db:"status"`
	RobotTypeID int64  `db:"robot_type_id"`
}

// robotTypeInfoRow represents robot type info
type robotTypeInfoRow struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
}

// dataCollectorInfoRow represents data collector info retrieved from DB
type dataCollectorInfoRow struct {
	ID             int64  `db:"id"`
	OrganizationID int64  `db:"organization_id"`
	Name           string `db:"name"`
	OperatorID     string `db:"operator_id"`
	Status         string `db:"status"`
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
		SELECT id, device_id, factory_id, status, robot_type_id 
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
		SELECT id, organization_id, name, operator_id, status 
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

	// Validate that the data_collector's organization belongs to the same factory as the robot.
	var dcOrgFactoryID int64
	if err = h.db.Get(&dcOrgFactoryID, "SELECT factory_id FROM organizations WHERE id = ? AND deleted_at IS NULL", dcInfo.OrganizationID); err != nil {
		logger.Printf("[STATION] Failed to query organization for data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}
	if dcOrgFactoryID != robotInfo.FactoryID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector's organization does not belong to the same factory as the robot"})
		return
	}

	// Check if robot is already assigned to an active (not deleted) station
	var existingStationID int64
	err = h.db.Get(&existingStationID, `
		SELECT id FROM workstations 
		WHERE robot_id = ? AND deleted_at IS NULL
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

	// Check if data_collector is already assigned to an active (not deleted) station
	err = h.db.Get(&existingStationID, `
		SELECT id FROM workstations 
		WHERE data_collector_id = ? AND deleted_at IS NULL
	`, dcInfo.ID)
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

	// Get robot type name for denormalization
	var robotType robotTypeInfoRow
	err = h.db.Get(&robotType, "SELECT id, name FROM robot_types WHERE id = ?", robotInfo.RobotTypeID)
	if err != nil {
		logger.Printf("[STATION] Failed to get robot type: %v", err)
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

	stationName, err := h.allocateStationName()
	if err != nil {
		logger.Printf("[STATION] Failed to allocate station name: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	// Insert the workstation (station)
	result, err := h.db.Exec(`
		INSERT INTO workstations (
			robot_id,
			robot_name,
			robot_serial,
			data_collector_id,
			collector_name,
			collector_operator_id,
			factory_id,
			organization_id,
			name,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		robotInfo.ID,
		robotType.Name,     // robot_name from robot_types.name
		robotInfo.DeviceID, // robot_serial from device_id
		dcInfo.ID,
		dcInfo.Name,           // collector_name
		dcInfo.OperatorID,     // collector_operator_id
		robotInfo.FactoryID,   // factory_id from robot
		dcInfo.OrganizationID, // organization_id from data_collector
		stationName,
		"offline",
		metadataStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[STATION] Failed to insert workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	stationID, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[STATION] Failed to get inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	var metaOut interface{}
	if metadataStr.Valid {
		metaOut = stationMetadataFromDB(metadataStr)
	}
	var factoryName string
	_ = h.db.Get(&factoryName, "SELECT name FROM factories WHERE id = ? AND deleted_at IS NULL", robotInfo.FactoryID)
	var orgName string
	_ = h.db.Get(&orgName, "SELECT name FROM organizations WHERE id = ? AND deleted_at IS NULL", dcInfo.OrganizationID)

	c.JSON(http.StatusCreated, StationResponse{
		ID:               fmt.Sprintf("%d", stationID),
		RobotID:          fmt.Sprintf("%d", robotInfo.ID),
		DataCollectorID:  fmt.Sprintf("%d", dcInfo.ID),
		FactoryID:        fmt.Sprintf("%d", robotInfo.FactoryID),
		FactoryName:      factoryName,
		OrganizationID:   fmt.Sprintf("%d", dcInfo.OrganizationID),
		OrganizationName: orgName,
		Status:           "offline",
		Name:             stationName,
		Metadata:         metaOut,
		CreatedAt:        now.Format(time.RFC3339),
		UpdatedAt:        now.Format(time.RFC3339),
	})
}

// stationListRow represents a station row from DB for listing
type stationListRow struct {
	ID                  int64          `db:"id"`
	RobotID             int64          `db:"robot_id"`
	RobotName           string         `db:"robot_name"`
	RobotSerial         string         `db:"robot_serial"`
	RobotTypeID         sql.NullInt64  `db:"robot_type_id"`
	RobotTypeModel      sql.NullString `db:"robot_type_model"`
	DataCollectorID     int64          `db:"data_collector_id"`
	CollectorName       string         `db:"collector_name"`
	CollectorOperatorID string         `db:"collector_operator_id"`
	FactoryID           int64          `db:"factory_id"`
	FactoryName         sql.NullString `db:"factory_name"`
	OrganizationID      int64          `db:"organization_id"`
	OrganizationName    sql.NullString `db:"organization_name"`
	Name                sql.NullString `db:"name"`
	Status              string         `db:"status"`
	Metadata            sql.NullString `db:"metadata"`
	CreatedAt           sql.NullTime   `db:"created_at"`
	UpdatedAt           sql.NullTime   `db:"updated_at"`
}

func stationResponseFromRow(s stationListRow) StationResponse {
	name := ""
	if s.Name.Valid {
		name = s.Name.String
	}
	factoryName := ""
	if s.FactoryName.Valid {
		factoryName = s.FactoryName.String
	}
	orgName := ""
	if s.OrganizationName.Valid {
		orgName = s.OrganizationName.String
	}
	robotTypeID := ""
	if s.RobotTypeID.Valid {
		robotTypeID = fmt.Sprintf("%d", s.RobotTypeID.Int64)
	}
	robotTypeModel := ""
	if s.RobotTypeModel.Valid {
		robotTypeModel = s.RobotTypeModel.String
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
		RobotTypeID:         robotTypeID,
		RobotTypeModel:      robotTypeModel,
		DataCollectorID:     fmt.Sprintf("%d", s.DataCollectorID),
		CollectorName:       s.CollectorName,
		CollectorOperatorID: s.CollectorOperatorID,
		FactoryID:           fmt.Sprintf("%d", s.FactoryID),
		FactoryName:         factoryName,
		OrganizationID:      fmt.Sprintf("%d", s.OrganizationID),
		OrganizationName:    orgName,
		Status:              s.Status,
		Name:                name,
		Metadata:            meta,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
	}
}

// ListStations handles listing all stations.
//
// @Summary      List stations
// @Description  Returns a list of all workstations with pagination
// @Tags         stations
// @Produce      json
// @Param        factory_id      query int false "Filter by factory ID"
// @Param        organization_id query int false "Filter by organization ID"
// @Param        robot_type_id   query int false "Filter by robot type ID"
// @Param        status          query string false "Filter by status (active, inactive, break, offline)"
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

	factoryIDStr := strings.TrimSpace(c.Query("factory_id"))
	orgIDStr := strings.TrimSpace(c.Query("organization_id"))
	robotTypeIDStr := strings.TrimSpace(c.Query("robot_type_id"))
	statusStr := strings.TrimSpace(c.Query("status"))
	var factoryID int64
	var orgID int64
	var robotTypeID int64
	hasFactory := factoryIDStr != ""
	hasOrg := orgIDStr != ""
	hasRobotType := robotTypeIDStr != ""
	hasStatus := statusStr != ""
	if hasFactory {
		var err error
		factoryID, err = strconv.ParseInt(factoryIDStr, 10, 64)
		if err != nil || factoryID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
			return
		}
	}
	if hasOrg {
		var err error
		orgID, err = strconv.ParseInt(orgIDStr, 10, 64)
		if err != nil || orgID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
			return
		}
	}
	if hasRobotType {
		var err error
		robotTypeID, err = strconv.ParseInt(robotTypeIDStr, 10, 64)
		if err != nil || robotTypeID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
			return
		}
	}
	if hasStatus {
		statusStr = strings.ToLower(statusStr)
		if _, ok := validStationStatuses[statusStr]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
	}

	countQuery := `
		SELECT COUNT(*)
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		INNER JOIN factories f ON f.id = ws.factory_id AND f.deleted_at IS NULL
		INNER JOIN organizations o ON o.id = ws.organization_id AND o.deleted_at IS NULL
		WHERE ws.deleted_at IS NULL
	`
	countArgs := []any{}
	if hasFactory {
		countQuery += " AND ws.factory_id = ?"
		countArgs = append(countArgs, factoryID)
	}
	if hasOrg {
		countQuery += " AND ws.organization_id = ?"
		countArgs = append(countArgs, orgID)
	}
	if hasRobotType {
		countQuery += " AND r.robot_type_id = ?"
		countArgs = append(countArgs, robotTypeID)
	}
	if hasStatus {
		countQuery += " AND ws.status = ?"
		countArgs = append(countArgs, statusStr)
	}
	var total int
	if err := h.db.Get(&total, countQuery, countArgs...); err != nil {
		logger.Printf("[STATION] Failed to count stations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list stations"})
		return
	}

	var stations []stationListRow
	query := `
		SELECT 
			ws.id, ws.robot_id, ws.robot_name, ws.robot_serial,
			r.robot_type_id, rt.model AS robot_type_model,
			ws.data_collector_id, ws.collector_name, ws.collector_operator_id,
			ws.factory_id, f.name AS factory_name,
			ws.organization_id, o.name AS organization_name,
			ws.name, ws.status, ws.metadata, ws.created_at, ws.updated_at
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		INNER JOIN factories f ON f.id = ws.factory_id AND f.deleted_at IS NULL
		INNER JOIN organizations o ON o.id = ws.organization_id AND o.deleted_at IS NULL
		WHERE ws.deleted_at IS NULL
	`
	args := []any{}
	if hasFactory {
		query += " AND ws.factory_id = ?\n"
		args = append(args, factoryID)
	}
	if hasOrg {
		query += " AND ws.organization_id = ?\n"
		args = append(args, orgID)
	}
	if hasRobotType {
		query += " AND r.robot_type_id = ?\n"
		args = append(args, robotTypeID)
	}
	if hasStatus {
		query += " AND ws.status = ?\n"
		args = append(args, statusStr)
	}
	query += `
		ORDER BY ws.id DESC
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	err = h.db.Select(&stations, query, args...)
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
	FactoryID           string `json:"factory_id"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	RobotName           string `json:"robot_name,omitempty"`
	RobotSerial         string `json:"robot_serial,omitempty"`
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
			id, robot_id,
			COALESCE(robot_name, '') AS robot_name,
			COALESCE(robot_serial, '') AS robot_serial,
			data_collector_id,
			COALESCE(collector_name, '') AS collector_name,
			COALESCE(collector_operator_id, '') AS collector_operator_id,
			factory_id,
			name, status,
			deleted_at
		FROM workstations
		WHERE id IN (?)
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
		DataCollectorID     int64          `db:"data_collector_id"`
		CollectorName       string         `db:"collector_name"`
		CollectorOperatorID string         `db:"collector_operator_id"`
		FactoryID           int64          `db:"factory_id"`
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
			FactoryID:           fmt.Sprintf("%d", r.FactoryID),
			Name:                name,
			Status:              r.Status,
			RobotName:           strings.TrimSpace(r.RobotName),
			RobotSerial:         strings.TrimSpace(r.RobotSerial),
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

// UpdateStation handles updating a station's status.
//
// @Summary      Update station
// @Description  Updates a station's status by ID
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        id      path      string               true  "Station ID (numeric, e.g. 1)"
// @Param        body    body      UpdateStationRequest true  "Status update payload"
// @Success      200     {object}  StationResponse
// @Failure      400     {object}  map[string]string
// @Failure      404     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /stations/{id} [put]
func (h *StationHandler) UpdateStation(c *gin.Context) {
	stationIDStr := c.Param("id")

	stationID, err := parseStationPathID(stationIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected numeric id"})
		return
	}

	var req UpdateStationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	hasRobot := req.RobotID != nil
	hasDC := req.DataCollectorID != nil
	hasStatus := req.Status != nil
	hasMeta := len(req.Metadata) > 0

	if hasRobot && strings.TrimSpace(*req.RobotID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_id cannot be empty"})
		return
	}
	if hasDC && strings.TrimSpace(*req.DataCollectorID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector_id cannot be empty"})
		return
	}

	if !hasRobot && !hasDC && !hasStatus && !hasMeta {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	// Station must exist before pairing validations
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM workstations WHERE id = ? AND deleted_at IS NULL)", stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to query station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
		return
	}

	var robotInfo robotInfoRow
	var robotType robotTypeInfoRow
	if hasRobot {
		ridStr := strings.TrimSpace(*req.RobotID)
		ridStr = strings.TrimPrefix(ridStr, "robot_")
		newRobotID, err := strconv.ParseInt(ridStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_id format"})
			return
		}
		err = h.db.Get(&robotInfo, `
			SELECT id, device_id, factory_id, status, robot_type_id
			FROM robots
			WHERE id = ? AND deleted_at IS NULL
		`, newRobotID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot not found"})
			return
		}
		if err != nil {
			logger.Printf("[STATION] Failed to query robot: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		if robotInfo.Status != "active" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot status must be active to be paired"})
			return
		}
		var otherWS int64
		err = h.db.Get(&otherWS, `
			SELECT id FROM workstations
			WHERE robot_id = ? AND deleted_at IS NULL AND id != ?
		`, robotInfo.ID, stationID)
		if err == nil {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "ROBOT_ALREADY_ASSIGNED",
				"message": fmt.Sprintf("Robot robot_%d is already assigned to station ws_%d", robotInfo.ID, otherWS),
			})
			return
		}
		if err != sql.ErrNoRows {
			logger.Printf("[STATION] Failed to check robot assignment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		err = h.db.Get(&robotType, "SELECT id, name FROM robot_types WHERE id = ?", robotInfo.RobotTypeID)
		if err != nil {
			logger.Printf("[STATION] Failed to get robot type: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
	}

	var dcInfo dataCollectorInfoRow
	if hasDC {
		dcStr := strings.TrimSpace(*req.DataCollectorID)
		dcStr = strings.TrimPrefix(dcStr, "dc_")
		newDCID, err := strconv.ParseInt(dcStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data_collector_id format"})
			return
		}
		err = h.db.Get(&dcInfo, `
			SELECT id, organization_id, name, operator_id, status
			FROM data_collectors
			WHERE id = ? AND deleted_at IS NULL
		`, newDCID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector not found"})
			return
		}
		if err != nil {
			logger.Printf("[STATION] Failed to query data collector: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		if dcInfo.Status != "active" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector status must be active to be paired"})
			return
		}
		var otherWS int64
		err = h.db.Get(&otherWS, `
			SELECT id FROM workstations
			WHERE data_collector_id = ? AND deleted_at IS NULL AND id != ?
		`, dcInfo.ID, stationID)
		if err == nil {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "DATA_COLLECTOR_ALREADY_ASSIGNED",
				"message": fmt.Sprintf("Data collector dc_%d is already assigned to station ws_%d", dcInfo.ID, otherWS),
			})
			return
		}
		if err != sql.ErrNoRows {
			logger.Printf("[STATION] Failed to check data collector assignment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
	}

	updates := []string{}
	args := []interface{}{}

	if hasRobot {
		updates = append(updates, "robot_id = ?", "robot_name = ?", "robot_serial = ?", "factory_id = ?")
		args = append(args, robotInfo.ID, robotType.Name, robotInfo.DeviceID, robotInfo.FactoryID)
	}

	if hasDC {
		// Validate that the new DC's organization belongs to the same factory as the current (or incoming) robot.
		effectiveFactoryID := int64(0)
		if hasRobot {
			effectiveFactoryID = robotInfo.FactoryID
		} else {
			// Read factory_id from the existing workstation row.
			if err := h.db.Get(&effectiveFactoryID, "SELECT factory_id FROM workstations WHERE id = ? AND deleted_at IS NULL", stationID); err != nil {
				logger.Printf("[STATION] Failed to read workstation factory_id: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
				return
			}
		}
		var dcOrgFactoryID int64
		if err := h.db.Get(&dcOrgFactoryID, "SELECT factory_id FROM organizations WHERE id = ? AND deleted_at IS NULL", dcInfo.OrganizationID); err != nil {
			logger.Printf("[STATION] Failed to query organization for data collector: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
			return
		}
		if dcOrgFactoryID != effectiveFactoryID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector's organization does not belong to the same factory as the robot"})
			return
		}
		updates = append(updates, "data_collector_id = ?", "collector_name = ?", "collector_operator_id = ?", "organization_id = ?")
		args = append(args, dcInfo.ID, dcInfo.Name, dcInfo.OperatorID, dcInfo.OrganizationID)
	}

	if hasStatus {
		status := strings.TrimSpace(*req.Status)
		if status == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status cannot be empty"})
			return
		}
		if !validStationStatuses[status] {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid status value",
				"valid":  []string{"active", "inactive", "break", "offline"},
				"actual": status,
			})
			return
		}
		updates = append(updates, "status = ?")
		args = append(args, status)
	}

	if hasMeta {
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

	updates = append(updates, "updated_at = NOW()")
	args = append(args, stationID)

	query := fmt.Sprintf(`
		UPDATE workstations 
		SET %s
		WHERE id = ? AND deleted_at IS NULL
	`, strings.Join(updates, ", "))
	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[STATION] Failed to update station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	// Fetch the updated station for response
	var station stationListRow
	err = h.db.Get(&station, `
		SELECT
			ws.id, ws.robot_id, ws.robot_name, ws.robot_serial,
			r.robot_type_id, rt.model AS robot_type_model,
			ws.data_collector_id, ws.collector_name, ws.collector_operator_id,
			ws.factory_id, f.name AS factory_name,
			ws.organization_id, o.name AS organization_name,
			ws.name, ws.status, ws.metadata, ws.created_at, ws.updated_at
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		INNER JOIN factories f ON f.id = ws.factory_id AND f.deleted_at IS NULL
		INNER JOIN organizations o ON o.id = ws.organization_id AND o.deleted_at IS NULL
		WHERE ws.id = ? AND ws.deleted_at IS NULL
	`, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to fetch updated station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	c.JSON(http.StatusOK, stationResponseFromRow(station))
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

	var station stationListRow
	err = h.db.Get(&station, `
		SELECT
			ws.id, ws.robot_id, ws.robot_name, ws.robot_serial,
			r.robot_type_id, rt.model AS robot_type_model,
			ws.data_collector_id, ws.collector_name, ws.collector_operator_id,
			ws.factory_id, f.name AS factory_name,
			ws.organization_id, o.name AS organization_name,
			ws.name, ws.status, ws.metadata, ws.created_at, ws.updated_at
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		INNER JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		INNER JOIN factories f ON f.id = ws.factory_id AND f.deleted_at IS NULL
		INNER JOIN organizations o ON o.id = ws.organization_id AND o.deleted_at IS NULL
		WHERE ws.id = ? AND ws.deleted_at IS NULL
	`, stationID)
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

// DeleteStation handles station deletion requests (soft delete).
//
// @Summary      Delete station
// @Description  Soft deletes a station by ID
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
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM workstations WHERE id = ? AND deleted_at IS NULL)", stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to check station existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
		return
	}

	var hasBlockingBatch bool
	err = h.db.Get(&hasBlockingBatch, `
		SELECT EXISTS(
			SELECT 1 FROM batches
			WHERE workstation_id = ? AND deleted_at IS NULL
			  AND status IN ('pending', 'active')
		)
	`, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to check batches for station %d: %v", stationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}
	if hasBlockingBatch {
		c.JSON(http.StatusConflict, gin.H{
			"error": "cannot delete station while batches are pending or active",
		})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE workstations SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to delete station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete station"})
		return
	}

	c.Status(http.StatusNoContent)
}
