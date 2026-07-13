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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

const (
	compatRobotTypeModel = "keystone_device_compat"
	compatFactorySlug    = "keystone_workspace_compat"
)

// RobotHandler handles robot related HTTP requests.
type RobotHandler struct {
	db           *sqlx.DB
	recorderHub  *services.RecorderHub
	transferHub  *services.TransferHub
	dpConfigPath string
}

// NewRobotHandler creates a new RobotHandler.
func NewRobotHandler(db *sqlx.DB, recorderHub *services.RecorderHub, transferHub *services.TransferHub, dpConfigPath ...string) *RobotHandler {
	configPath := ""
	if len(dpConfigPath) > 0 {
		configPath = strings.TrimSpace(dpConfigPath[0])
	}
	return &RobotHandler{
		db:           db,
		recorderHub:  recorderHub,
		transferHub:  transferHub,
		dpConfigPath: configPath,
	}
}

// RobotResponse represents a robot in the response.
type RobotResponse struct {
	ID             string      `json:"id"`
	RobotTypeID    string      `json:"robot_type_id,omitempty"`
	RobotTypeName  string      `json:"robot_type_name,omitempty"`
	RobotTypeModel string      `json:"robot_type_model,omitempty"`
	DeviceID       string      `json:"device_id"`
	DeviceName     string      `json:"device_name,omitempty"`
	WorkspaceID    string      `json:"workspace_id"`
	FactoryID      string      `json:"factory_id,omitempty"`
	FactoryName    string      `json:"factory_name,omitempty"`
	FactorySlug    string      `json:"factory_slug,omitempty"`
	AssetID        string      `json:"asset_id,omitempty"`
	Status         string      `json:"status"`
	Metadata       interface{} `json:"metadata,omitempty"`
	CreatedAt      string      `json:"created_at,omitempty"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
	Connected      bool        `json:"connected"`
	ConnectedAt    string      `json:"connected_at,omitempty"`
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

// CloudAssetOption is one bindable cloud asset choice for robot management.
type CloudAssetOption struct {
	DeviceID string `json:"device_id"`
}

// CloudAssetOptionsResponse represents available cloud asset choices.
type CloudAssetOptionsResponse struct {
	Items []CloudAssetOption `json:"items"`
}

// BindCloudAssetRequest represents a robot cloud asset binding request.
type BindCloudAssetRequest struct {
	AssetID string `json:"asset_id"`
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

// CreateRobotRequest represents the request body for creating a robot.
type CreateRobotRequest struct {
	WorkspaceID string      `json:"workspace_id"`
	RobotTypeID string      `json:"robot_type_id"`
	DeviceID    string      `json:"device_id"`
	FactoryID   string      `json:"factory_id"`
	AssetID     string      `json:"asset_id,omitempty"`
	Metadata    interface{} `json:"metadata,omitempty"`
}

// CreateRobotResponse represents the response for creating a robot.
type CreateRobotResponse struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspace_id"`
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
	apiV1.GET("/robots/cloud-asset-options", h.ListCloudAssetOptions)
	apiV1.GET("/devices/:device_id/connection", h.GetDeviceConnection)
	apiV1.GET("/robots/:id", h.GetRobot)
	apiV1.PUT("/robots/:id/cloud-asset", h.BindCloudAsset)
	apiV1.DELETE("/robots/:id/cloud-asset", h.UnbindCloudAsset)
}

// robotRow represents a robot in the database
type robotRow struct {
	ID             int64          `db:"id"`
	RobotTypeID    sql.NullInt64  `db:"robot_type_id"`
	RobotTypeName  sql.NullString `db:"robot_type_name"`
	RobotTypeModel sql.NullString `db:"robot_type_model"`
	DeviceID       string         `db:"device_id"`
	WorkspaceID    int64          `db:"workspace_id"`
	FactoryID      sql.NullInt64  `db:"factory_id"`
	FactoryName    sql.NullString `db:"factory_name"`
	FactorySlug    sql.NullString `db:"factory_slug"`
	AssetID        sql.NullString `db:"asset_id"`
	Status         string         `db:"status"`
	Metadata       sql.NullString `db:"metadata"`
	CreatedAt      sql.NullTime   `db:"created_at"`
	UpdatedAt      sql.NullTime   `db:"updated_at"`
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

func normalizeAssetID(raw string) (sql.NullString, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return sql.NullString{}, nil
	}
	if utf8.RuneCountInString(value) > 100 {
		return sql.NullString{}, fmt.Errorf("asset_id must be at most 100 characters")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return sql.NullString{}, fmt.Errorf("asset_id must not contain control characters")
		}
	}
	return sql.NullString{String: value, Valid: true}, nil
}

func assetIDValue(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return strings.TrimSpace(ns.String)
}

func isDPConfigSystemError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "KEYSTONE_SYNC_DP_CONFIG") ||
		strings.Contains(msg, "read DP config") ||
		strings.Contains(msg, "parse DP config") ||
		strings.Contains(msg, "unsupported version") ||
		strings.Contains(msg, "endpoints.") ||
		strings.Contains(msg, "deviceId is empty") ||
		strings.Contains(msg, "duplicate deviceId")
}

func (h *RobotHandler) respondCloudAssetValidationError(c *gin.Context, err error, action string) {
	if isDPConfigSystemError(err) {
		logger.Printf("[ROBOT] Failed to %s cloud asset using DP config: %v", action, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate cloud asset"})
		return
	}
	logger.Printf("[ROBOT] Cloud asset is not bindable while %s: %v", action, err)
	c.JSON(http.StatusBadRequest, gin.H{"error": "asset_id is not bindable"})
}

func (h *RobotHandler) assetIDInUse(assetID string, excludeRobotID int64) (bool, error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return false, nil
	}
	var exists bool
	query := "SELECT EXISTS(SELECT 1 FROM robots WHERE asset_id = ? AND deleted_at IS NULL"
	args := []interface{}{assetID}
	if excludeRobotID > 0 {
		query += " AND id <> ?"
		args = append(args, excludeRobotID)
	}
	query += ")"
	if err := h.db.Get(&exists, query, args...); err != nil {
		return false, err
	}
	return exists, nil
}

func (h *RobotHandler) resolveCompatRobotTypeID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		var id int64
		err := h.db.Get(&id, "SELECT id FROM robot_types WHERE deleted_at IS NULL ORDER BY id LIMIT 1")
		if err == nil {
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, fmt.Errorf("no compatible robot type is available")
		}
		now := time.Now().UTC()
		if h.db.DriverName() == "sqlite" {
			_, err = h.db.Exec(`
				INSERT INTO robot_types (name, model, ros_topics, created_at, updated_at, deleted_at)
				VALUES (?, ?, ?, ?, ?, NULL)
				ON CONFLICT(model) DO UPDATE SET
					updated_at = excluded.updated_at,
					deleted_at = NULL
			`, "Keystone Device Compatibility", compatRobotTypeModel, "{}", now, now)
		} else {
			_, err = h.db.Exec(`
				INSERT INTO robot_types (name, model, ros_topics, created_at, updated_at, deleted_at)
				VALUES (?, ?, ?, ?, ?, NULL)
				ON DUPLICATE KEY UPDATE
					updated_at = VALUES(updated_at),
					deleted_at = NULL
			`, "Keystone Device Compatibility", compatRobotTypeModel, "{}", now, now)
		}
		if err != nil {
			return 0, fmt.Errorf("create compatible robot type: %w", err)
		}
		if err := h.db.Get(&id, "SELECT id FROM robot_types WHERE model = ? AND deleted_at IS NULL LIMIT 1", compatRobotTypeModel); err != nil {
			return 0, fmt.Errorf("load compatible robot type: %w", err)
		}
		return id, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid robot_type_id format")
	}
	var exists bool
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", id); err != nil || !exists {
		return 0, fmt.Errorf("robot_type not found")
	}
	return id, nil
}

func (h *RobotHandler) resolveCompatFactoryID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		var id int64
		err := h.db.Get(&id, "SELECT id FROM factories WHERE deleted_at IS NULL ORDER BY id LIMIT 1")
		if err == nil {
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, fmt.Errorf("no compatible factory is available")
		}
		now := time.Now().UTC()
		if h.db.DriverName() == "sqlite" {
			_, err = h.db.Exec(`
				INSERT INTO factories (name, slug, created_at, updated_at, deleted_at)
				VALUES (?, ?, ?, ?, NULL)
				ON CONFLICT(slug) DO UPDATE SET
					updated_at = excluded.updated_at,
					deleted_at = NULL
			`, "Keystone Workspace Compatibility", compatFactorySlug, now, now)
		} else {
			_, err = h.db.Exec(`
				INSERT INTO factories (name, slug, created_at, updated_at, deleted_at)
				VALUES (?, ?, ?, ?, NULL)
				ON DUPLICATE KEY UPDATE
					updated_at = VALUES(updated_at),
					deleted_at = NULL
			`, "Keystone Workspace Compatibility", compatFactorySlug, now, now)
		}
		if err != nil {
			return 0, fmt.Errorf("create compatible factory: %w", err)
		}
		if err := h.db.Get(&id, "SELECT id FROM factories WHERE slug = ? AND deleted_at IS NULL LIMIT 1", compatFactorySlug); err != nil {
			return 0, fmt.Errorf("load compatible factory: %w", err)
		}
		return id, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid factory_id format")
	}
	var exists bool
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", id); err != nil || !exists {
		return 0, fmt.Errorf("factory not found")
	}
	return id, nil
}

func (h *RobotHandler) loadRobotRow(id int64) (robotRow, error) {
	var r robotRow
	err := h.db.Get(&r, `
		SELECT
			r.id,
			r.robot_type_id,
			r.device_id,
			r.workspace_id,
			r.factory_id,
			r.asset_id,
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
	assetID := ""
	if r.AssetID.Valid {
		assetID = r.AssetID.String
	}
	return RobotResponse{
		ID:             fmt.Sprintf("%d", r.ID),
		RobotTypeID:    nullableInt64String(r.RobotTypeID),
		RobotTypeName:  nullString(r.RobotTypeName),
		RobotTypeModel: nullString(r.RobotTypeModel),
		DeviceID:       r.DeviceID,
		DeviceName:     robotDeviceNameFromMetadata(r.Metadata),
		WorkspaceID:    fmt.Sprintf("%d", r.WorkspaceID),
		FactoryID:      nullableInt64String(r.FactoryID),
		FactoryName:    nullString(r.FactoryName),
		FactorySlug:    nullString(r.FactorySlug),
		AssetID:        assetID,
		Status:         r.Status,
		Metadata:       robotMetadataFromDB(r.Metadata),
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
		Connected:      connected,
		ConnectedAt:    connectedAt,
	}
}

func nullableInt64String(value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	return strconv.FormatInt(value.Int64, 10)
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
// @Param        keyword       query     string  false  "Search by device ID or asset ID"
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
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")

	whereClause := "WHERE r.deleted_at IS NULL"
	args := []interface{}{}
	whereClause, args = appendInt64InFilter(whereClause, args, "r.workspace_id", workspaceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "r.device_id", deviceIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "r.status", statuses)

	if keyword != "" {
		likeKeyword := "%" + keyword + "%"
		whereClause += " AND (r.device_id LIKE ? OR r.asset_id LIKE ?)"
		args = append(args, likeKeyword, likeKeyword)
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

	orderClause, orderArgs := keywordOrderBy(keyword, "r.id DESC", "r.device_id", "r.asset_id")
	query := `
		SELECT 
				r.id,
				r.robot_type_id,
				NULL AS robot_type_name,
				NULL AS robot_type_model,
				r.device_id,
				r.workspace_id,
				r.factory_id,
				NULL AS factory_name,
				NULL AS factory_slug,
				r.asset_id,
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

// ListCloudAssetOptions lists bindable cloud asset IDs from the DP config.
//
// @Summary      List cloud asset options
// @Description  Lists cloud asset IDs that can be bound to a robot
// @Tags         robots
// @Produce      json
// @Param        robot_id query string false "Current robot ID to exclude from occupancy checks"
// @Param        q        query string false "Case-insensitive device ID search"
// @Success      200      {object} CloudAssetOptionsResponse
// @Failure      400      {object} map[string]string
// @Failure      500      {object} map[string]string
// @Router       /robots/cloud-asset-options [get]
func (h *RobotHandler) ListCloudAssetOptions(c *gin.Context) {
	excludeRobotID := int64(0)
	robotIDRaw := strings.TrimSpace(c.Query("robot_id"))
	if robotIDRaw != "" {
		id, err := strconv.ParseInt(robotIDRaw, 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_id format"})
			return
		}
		excludeRobotID = id
	}
	query := strings.ToLower(strings.TrimSpace(c.Query("q")))

	currentAssetID := ""
	if excludeRobotID > 0 {
		current, err := h.loadRobotRow(excludeRobotID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
			return
		}
		if err != nil {
			logger.Printf("[ROBOT] Failed to query robot for cloud asset options: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list cloud asset options"})
			return
		}
		currentAssetID = assetIDValue(current.AssetID)
	}

	profiles, err := services.ListDPDeviceProfiles(h.dpConfigPath)
	if err != nil {
		logger.Printf("[ROBOT] Failed to list cloud asset options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list cloud asset options"})
		return
	}

	var occupied []string
	occupiedQuery := "SELECT asset_id FROM robots WHERE asset_id IS NOT NULL AND asset_id <> '' AND deleted_at IS NULL"
	args := []interface{}{}
	if excludeRobotID > 0 {
		occupiedQuery += " AND id <> ?"
		args = append(args, excludeRobotID)
	}
	if err := h.db.Select(&occupied, occupiedQuery, args...); err != nil {
		logger.Printf("[ROBOT] Failed to query occupied cloud assets: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list cloud asset options"})
		return
	}
	occupiedSet := make(map[string]struct{}, len(occupied))
	for _, assetID := range occupied {
		if value := strings.TrimSpace(assetID); value != "" {
			occupiedSet[value] = struct{}{}
		}
	}

	options := make([]CloudAssetOption, 0, len(profiles))
	for _, profile := range profiles {
		deviceID := strings.TrimSpace(profile.DeviceID)
		assetID, err := normalizeAssetID(deviceID)
		if err != nil || !assetID.Valid {
			continue
		}
		if _, used := occupiedSet[assetID.String]; used {
			continue
		}
		if currentAssetID != "" && assetID.String == currentAssetID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(assetID.String), query) {
			continue
		}
		options = append(options, CloudAssetOption{DeviceID: assetID.String})
	}

	c.JSON(http.StatusOK, CloudAssetOptionsResponse{Items: options})
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
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	req.FactoryID = strings.TrimSpace(req.FactoryID)
	assetID, err := normalizeAssetID(req.AssetID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.WorkspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id is required"})
		return
	}

	if req.DeviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}

	workspaceID, err := strconv.ParseInt(req.WorkspaceID, 10, 64)
	if err != nil || workspaceID < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace_id format"})
		return
	}
	var workspaceExists bool
	if err := h.db.Get(&workspaceExists, "SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = ? AND deleted_at IS NULL)", workspaceID); err != nil || !workspaceExists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace not found"})
		return
	}
	if assetID.Valid {
		if err := services.ValidateDPDeviceProfile(h.dpConfigPath, assetID.String); err != nil {
			h.respondCloudAssetValidationError(c, err, "create robot with")
			return
		}
		inUse, err := h.assetIDInUse(assetID.String, 0)
		if err != nil {
			logger.Printf("[ROBOT] Failed to check asset_id uniqueness: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
			return
		}
		if inUse {
			c.JSON(http.StatusConflict, gin.H{"error": "asset_id is already assigned to another robot"})
			return
		}
	}

	robotTypeID, err := h.resolveCompatRobotTypeID(req.RobotTypeID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	factoryID, err := h.resolveCompatFactoryID(req.FactoryID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	// Insert the robot
	result, err := h.db.Exec(
		`INSERT INTO robots (
			robot_type_id,
			device_id,
			workspace_id,
			factory_id,
			asset_id,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		robotTypeID,
		req.DeviceID,
		workspaceID,
		factoryID,
		assetID,
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
			r.workspace_id,
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
		WorkspaceID: resp.WorkspaceID,
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
			r.workspace_id,
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
	AssetID     json.RawMessage `json:"asset_id,omitempty" swaggertype:"string"`
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

	if len(req.AssetID) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "asset_id must be managed through cloud asset binding API"})
		return
	}

	var current struct {
		AssetID sql.NullString `db:"asset_id"`
	}
	err = h.db.Get(&current, "SELECT asset_id FROM robots WHERE id = ? AND deleted_at IS NULL", id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}
	if err != nil {
		logger.Printf("[ROBOT] Failed to query robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
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

	query := fmt.Sprintf("UPDATE robots SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", ")) //nolint:gosec // columns are hardcoded literals, not user input

	tx, err := h.db.Begin()
	if err != nil {
		logger.Printf("[ROBOT] Failed to begin transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	defer tx.Rollback() //nolint:errcheck

	result, err := tx.Exec(query, args...)
	if err != nil {
		logger.Printf("[ROBOT] Failed to update robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[ROBOT] Failed to get rows affected: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
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
		wsQuery := fmt.Sprintf( //nolint:gosec // columns are hardcoded literals, not user input
			"UPDATE workstations SET %s WHERE robot_id = ? AND is_current = TRUE AND deleted_at IS NULL",
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
			r.workspace_id,
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

// BindCloudAsset binds or changes a robot cloud asset ID.
//
// @Summary      Bind robot cloud asset
// @Description  Binds or changes the Data Platform device ID used for future uploads
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        id   path      string                 true  "Robot ID"
// @Param        body body      BindCloudAssetRequest  true  "Cloud asset payload"
// @Success      200  {object}  RobotResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robots/{id}/cloud-asset [put]
func (h *RobotHandler) BindCloudAsset(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	var req BindCloudAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	assetID, err := normalizeAssetID(req.AssetID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !assetID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "asset_id is required"})
		return
	}

	current, err := h.loadRobotRow(id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}
	if err != nil {
		logger.Printf("[ROBOT] Failed to query robot for cloud asset bind: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}

	currentAssetID := assetIDValue(current.AssetID)
	if assetID.String == currentAssetID {
		c.JSON(http.StatusOK, h.responseFromRow(current))
		return
	}

	if err := services.ValidateDPDeviceProfile(h.dpConfigPath, assetID.String); err != nil {
		h.respondCloudAssetValidationError(c, err, "bind")
		return
	}
	inUse, err := h.assetIDInUse(assetID.String, id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to check asset_id uniqueness: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	if inUse {
		c.JSON(http.StatusConflict, gin.H{"error": "asset_id is already assigned to another robot"})
		return
	}

	now := time.Now().UTC()
	result, err := h.db.Exec(
		"UPDATE robots SET asset_id = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		assetID,
		now,
		id,
	)
	if err != nil {
		logger.Printf("[ROBOT] Failed to bind cloud asset: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[ROBOT] Failed to get rows affected for cloud asset bind: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}

	updated, err := h.loadRobotRow(id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch cloud asset bound robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated robot"})
		return
	}
	if currentAssetID == "" {
		logger.Printf("[ROBOT] Cloud asset bound: robot_id=%d asset_id=%s", id, assetID.String)
	} else {
		logger.Printf("[ROBOT] Cloud asset changed: robot_id=%d old_asset_id=%s new_asset_id=%s", id, currentAssetID, assetID.String)
	}
	c.JSON(http.StatusOK, h.responseFromRow(updated))
}

// UnbindCloudAsset clears a robot cloud asset ID.
//
// @Summary      Unbind robot cloud asset
// @Description  Clears the Data Platform device ID used for future uploads
// @Tags         robots
// @Produce      json
// @Param        id   path      string  true  "Robot ID"
// @Success      200  {object}  RobotResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /robots/{id}/cloud-asset [delete]
func (h *RobotHandler) UnbindCloudAsset(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	current, err := h.loadRobotRow(id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}
	if err != nil {
		logger.Printf("[ROBOT] Failed to query robot for cloud asset unbind: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}

	currentAssetID := assetIDValue(current.AssetID)
	if currentAssetID == "" {
		c.JSON(http.StatusOK, h.responseFromRow(current))
		return
	}

	result, err := h.db.Exec(
		"UPDATE robots SET asset_id = NULL, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		time.Now().UTC(),
		id,
	)
	if err != nil {
		logger.Printf("[ROBOT] Failed to unbind cloud asset: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[ROBOT] Failed to get rows affected for cloud asset unbind: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update robot"})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		return
	}

	updated, err := h.loadRobotRow(id)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch cloud asset unbound robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated robot"})
		return
	}
	logger.Printf("[ROBOT] Cloud asset unbound: robot_id=%d old_asset_id=%s", id, currentAssetID)
	c.JSON(http.StatusOK, h.responseFromRow(updated))
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
