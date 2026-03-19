// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
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
	RobotID         string `json:"robot_id"`
	DataCollectorID string `json:"data_collector_id"`
	Name            string `json:"name"`
}

// UpdateStationRequest represents the request body for updating a station.
type UpdateStationRequest struct {
	Status string `json:"status"`
}

// StationResponse represents a station in the response.
type StationResponse struct {
	ID                  string `json:"id"`
	RobotID             string `json:"robot_id"`
	RobotName           string `json:"robot_name"`
	RobotSerial         string `json:"robot_serial"`
	DataCollectorID     string `json:"data_collector_id"`
	CollectorName       string `json:"collector_name"`
	CollectorOperatorID string `json:"collector_operator_id"`
	FactoryID           string `json:"factory_id"`
	Status              string `json:"status"`
	Name                string `json:"name"`
	CreatedAt           string `json:"created_at"`
}

// RegisterRoutes registers station related routes.
func (h *StationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/stations", h.CreateStation)
	apiV1.GET("/stations", h.ListStations)
	apiV1.PATCH("/stations/:id", h.UpdateStation)
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
	ID         int64  `db:"id"`
	Name       string `db:"name"`
	OperatorID string `db:"operator_id"`
	Status     string `db:"status"`
}

// factoryRow represents factory info
type factoryInfoRow struct {
	ID   int64  `db:"id"`
	Slug string `db:"slug"`
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
	req.Name = strings.TrimSpace(req.Name)

	// Validate required fields
	if req.RobotID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_id is required"})
		return
	}

	if req.DataCollectorID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data_collector_id is required"})
		return
	}

	// Parse robot_id (device_id from robots table)
	var robotInfo robotInfoRow
	err := h.db.Get(&robotInfo, `
		SELECT id, device_id, factory_id, status, robot_type_id 
		FROM robots 
		WHERE device_id = ? AND deleted_at IS NULL
	`, req.RobotID)
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

	// Parse data_collector_id (operator_id from data_collectors table)
	var dcInfo dataCollectorInfoRow
	err = h.db.Get(&dcInfo, `
		SELECT id, name, operator_id, status 
		FROM data_collectors 
		WHERE operator_id = ? AND deleted_at IS NULL
	`, req.DataCollectorID)
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

	// Note: The data_collectors table doesn't have a factory_id column in the schema.
	// Therefore, we cannot validate that robot and data_collector are in the same factory.
	// This validation would require schema modification.

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
			"message": fmt.Sprintf("Robot %s is already assigned to station ws_%d", req.RobotID, existingStationID),
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
			"message": fmt.Sprintf("Data collector %s is already assigned to station ws_%d", req.DataCollectorID, existingStationID),
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

	// Get factory slug for response
	var factory factoryInfoRow
	err = h.db.Get(&factory, "SELECT id, slug FROM factories WHERE id = ?", robotInfo.FactoryID)
	if err != nil {
		logger.Printf("[STATION] Failed to get factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create station"})
		return
	}

	// Generate created_at timestamp
	createdAt := time.Now().UTC().Format("2006-01-02 15:04:05")

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
			name,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		robotInfo.ID,
		robotType.Name,     // robot_name from robot_types.name
		robotInfo.DeviceID, // robot_serial from device_id
		dcInfo.ID,
		dcInfo.Name,       // collector_name
		dcInfo.OperatorID, // collector_operator_id
		robotInfo.FactoryID,
		req.Name,
		"active",
		createdAt,
		createdAt,
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

	// Format created_at for response in ISO 8601
	createdAtISO, _ := time.Parse("2006-01-02 15:04:05", createdAt)

	c.JSON(http.StatusCreated, StationResponse{
		ID:                  fmt.Sprintf("ws_%d", stationID),
		RobotID:             req.RobotID,
		RobotName:           robotType.Name,
		RobotSerial:         robotInfo.DeviceID,
		DataCollectorID:     req.DataCollectorID,
		CollectorName:       dcInfo.Name,
		CollectorOperatorID: dcInfo.OperatorID,
		FactoryID:           factory.Slug,
		Status:              "active",
		Name:                req.Name,
		CreatedAt:           createdAtISO.Format(time.RFC3339),
	})
}

// stationListRow represents a station row from DB for listing
type stationListRow struct {
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
	CreatedAt           sql.NullString `db:"created_at"`
}

// ListStations handles listing all stations.
//
// @Summary      List stations
// @Description  Returns a list of all workstations
// @Tags         stations
// @Produce      json
// @Success      200  {object}  map[string][]StationResponse
// @Failure      500  {object}  map[string]string
// @Router       /stations [get]
func (h *StationHandler) ListStations(c *gin.Context) {
	var stations []stationListRow
	err := h.db.Select(&stations, `
		SELECT 
			id, robot_id, robot_name, robot_serial,
			data_collector_id, collector_name, collector_operator_id,
			factory_id, name, status, created_at
		FROM workstations 
		WHERE deleted_at IS NULL
		ORDER BY id DESC
	`)
	if err != nil && err != sql.ErrNoRows {
		logger.Printf("[STATION] Failed to query stations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list stations"})
		return
	}

	if stations == nil {
		stations = []stationListRow{}
	}

	// Get factory slugs for all unique factory IDs
	factoryIDs := make([]int64, 0, len(stations))
	existingFactoryIDs := make(map[int64]bool)
	for _, s := range stations {
		if !existingFactoryIDs[s.FactoryID] {
			factoryIDs = append(factoryIDs, s.FactoryID)
			existingFactoryIDs[s.FactoryID] = true
		}
	}

	factorySlugs := make(map[int64]string)
	if len(factoryIDs) > 0 {
		// Use placeholder query for MySQL
		query := "SELECT id, slug FROM factories WHERE id IN (" + strings.Repeat("?,", len(factoryIDs)-1) + "?)"
		// Convert to []interface{} for sqlx
		args := make([]interface{}, len(factoryIDs))
		for i, id := range factoryIDs {
			args[i] = id
		}
		var factoryRows []factoryInfoRow
		err = h.db.Select(&factoryRows, query, args...)
		if err != nil && err != sql.ErrNoRows {
			logger.Printf("[STATION] Failed to query factories: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list stations"})
			return
		}
		for _, f := range factoryRows {
			factorySlugs[f.ID] = f.Slug
		}
	}

	// Build response
	response := make([]StationResponse, 0, len(stations))
	for _, s := range stations {
		var createdAtStr string
		if s.CreatedAt.Valid {
			createdAt, _ := time.Parse("2006-01-02 15:04:05", s.CreatedAt.String)
			createdAtStr = createdAt.Format(time.RFC3339)
		}

		response = append(response, StationResponse{
			ID:                  fmt.Sprintf("ws_%d", s.ID),
			RobotID:             fmt.Sprintf("robot_%d", s.RobotID),
			RobotName:           s.RobotName,
			RobotSerial:         s.RobotSerial,
			DataCollectorID:     fmt.Sprintf("dc_%d", s.DataCollectorID),
			CollectorName:       s.CollectorName,
			CollectorOperatorID: s.CollectorOperatorID,
			FactoryID:           factorySlugs[s.FactoryID],
			Status:              s.Status,
			Name:                s.Name.String,
			CreatedAt:           createdAtStr,
		})
	}

	c.JSON(http.StatusOK, gin.H{"stations": response})
}

// validStationStatuses contains all valid station status values
var validStationStatuses = map[string]bool{
	"active":   true,
	"inactive": true,
	"break":    true,
	"offline":  true,
}

// UpdateStation handles updating a station's status.
//
// @Summary      Update station
// @Description  Updates a station's status by ID
// @Tags         stations
// @Accept       json
// @Produce      json
// @Param        id      path      string               true  "Station ID (e.g., ws_001)"
// @Param        body    body      UpdateStationRequest true  "Status update payload"
// @Success      200     {object}  StationResponse
// @Failure      400     {object}  map[string]string
// @Failure      404     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /stations/{id} [patch]
func (h *StationHandler) UpdateStation(c *gin.Context) {
	stationIDStr := c.Param("id")

	// Parse station ID (format: ws_XXX)
	if !strings.HasPrefix(stationIDStr, "ws_") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected ws_XXX"})
		return
	}

	idStr := strings.TrimPrefix(stationIDStr, "ws_")
	var stationID int64
	_, err := fmt.Sscanf(idStr, "%d", &stationID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid station ID format, expected ws_XXX"})
		return
	}

	var req UpdateStationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate status
	req.Status = strings.TrimSpace(req.Status)
	if req.Status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status is required"})
		return
	}

	if !validStationStatuses[req.Status] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "invalid status value",
			"valid":  []string{"active", "inactive", "break", "offline"},
			"actual": req.Status,
		})
		return
	}

	// Check if station exists
	var existingStatus string
	err = h.db.Get(&existingStatus, `
		SELECT status FROM workstations 
		WHERE id = ? AND deleted_at IS NULL
	`, stationID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "station not found"})
		return
	}
	if err != nil {
		logger.Printf("[STATION] Failed to query station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	// Update the station status
	_, err = h.db.Exec(`
		UPDATE workstations 
		SET status = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, req.Status, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to update station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	// Fetch the updated station for response
	var station stationListRow
	err = h.db.Get(&station, `
		SELECT 
			id, robot_id, robot_name, robot_serial,
			data_collector_id, collector_name, collector_operator_id,
			factory_id, name, status, created_at
		FROM workstations 
		WHERE id = ? AND deleted_at IS NULL
	`, stationID)
	if err != nil {
		logger.Printf("[STATION] Failed to fetch updated station: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update station"})
		return
	}

	// Get factory slug
	var factory factoryInfoRow
	var factorySlug string
	err = h.db.Get(&factory, "SELECT id, slug FROM factories WHERE id = ?", station.FactoryID)
	if err == nil {
		factorySlug = factory.Slug
	}

	// Format response
	var createdAtStr string
	if station.CreatedAt.Valid {
		createdAt, _ := time.Parse("2006-01-02 15:04:05", station.CreatedAt.String)
		createdAtStr = createdAt.Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, StationResponse{
		ID:                  fmt.Sprintf("ws_%d", station.ID),
		RobotID:             fmt.Sprintf("robot_%d", station.RobotID),
		RobotName:           station.RobotName,
		RobotSerial:         station.RobotSerial,
		DataCollectorID:     fmt.Sprintf("dc_%d", station.DataCollectorID),
		CollectorName:       station.CollectorName,
		CollectorOperatorID: station.CollectorOperatorID,
		FactoryID:           factorySlug,
		Status:              station.Status,
		Name:                station.Name.String,
		CreatedAt:           createdAtStr,
	})
}
