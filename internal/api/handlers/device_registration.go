// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

var (
	errRegistrationFactoryNotFound   = errors.New("factory not found")
	errRegistrationRobotTypeNotFound = errors.New("robot_type not found")
)

// DeviceRegistrationHandler handles install-time device registration requests.
type DeviceRegistrationHandler struct {
	db *sqlx.DB
}

// NewDeviceRegistrationHandler creates a new DeviceRegistrationHandler.
func NewDeviceRegistrationHandler(db *sqlx.DB) *DeviceRegistrationHandler {
	return &DeviceRegistrationHandler{db: db}
}

// DeviceRegistrationRequest represents the request body for device registration.
type DeviceRegistrationRequest struct {
	Factory   string `json:"factory"`
	RobotType string `json:"robot_type"`
}

// DeviceRegistrationResponse represents a successful device registration.
type DeviceRegistrationResponse struct {
	DeviceID    string `json:"device_id"`
	Factory     string `json:"factory"`
	FactoryID   string `json:"factory_id"`
	RobotType   string `json:"robot_type"`
	RobotTypeID string `json:"robot_type_id"`
	RobotID     string `json:"robot_id"`
}

type deviceRegistrationFactoryRow struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
}

type deviceRegistrationRobotTypeRow struct {
	ID    int64  `db:"id"`
	Model string `db:"model"`
}

// RegisterRoutes registers device registration routes.
func (h *DeviceRegistrationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/devices/register", h.RegisterDevice)
}

// RegisterDevice handles install-time robot device registration.
//
// @Summary      Register device
// @Description  Registers one robot device by existing factory name and robot type model
// @Tags         devices
// @Accept       json
// @Produce      json
// @Param        body  body      DeviceRegistrationRequest  true  "Device registration payload"
// @Success      201   {object}  DeviceRegistrationResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /devices/register [post]
func (h *DeviceRegistrationHandler) RegisterDevice(c *gin.Context) {
	var req DeviceRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	factory := strings.TrimSpace(req.Factory)
	if factory == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory is required"})
		return
	}

	robotType := strings.TrimSpace(req.RobotType)
	if robotType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type is required"})
		return
	}

	resp, err := h.registerDevice(factory, robotType)
	if err != nil {
		switch {
		case errors.Is(err, errRegistrationFactoryNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
		case errors.Is(err, errRegistrationRobotTypeNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "robot_type not found"})
		default:
			logger.Printf("[DEVICE] Failed to register device: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register device"})
		}
		return
	}

	c.JSON(http.StatusCreated, resp)
}

func (h *DeviceRegistrationHandler) registerDevice(factoryName, robotTypeModel string) (DeviceRegistrationResponse, error) {
	tx, err := h.db.Beginx()
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Safe after successful Commit.

	var factory deviceRegistrationFactoryRow
	if err := tx.Get(&factory, `
		SELECT id, name
		FROM factories
		WHERE name = ? AND deleted_at IS NULL
	`, factoryName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceRegistrationResponse{}, errRegistrationFactoryNotFound
		}
		return DeviceRegistrationResponse{}, fmt.Errorf("query factory: %w", err)
	}

	var robotType deviceRegistrationRobotTypeRow
	if err := tx.Get(&robotType, `
		SELECT id, model
		FROM robot_types
		WHERE model = ? AND deleted_at IS NULL
	`, robotTypeModel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceRegistrationResponse{}, errRegistrationRobotTypeNotFound
		}
		return DeviceRegistrationResponse{}, fmt.Errorf("query robot type: %w", err)
	}

	sequence, err := allocateDeviceIDSequence(tx, factory.ID, robotType.ID)
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("allocate device id sequence: %w", err)
	}

	now := time.Now().UTC()
	deviceID := formatRegisteredDeviceID(factory.ID, robotType.ID, sequence)
	result, err := tx.Exec(`
		INSERT INTO robots (
			robot_type_id,
			device_id,
			factory_id,
			asset_id,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		robotType.ID,
		deviceID,
		factory.ID,
		sql.NullString{},
		"active",
		sql.NullString{String: "{}", Valid: true},
		now,
		now,
	)
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("insert robot: %w", err)
	}

	robotID, err := result.LastInsertId()
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("get inserted robot id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("commit transaction: %w", err)
	}

	return DeviceRegistrationResponse{
		DeviceID:    deviceID,
		Factory:     factory.Name,
		FactoryID:   strconv.FormatInt(factory.ID, 10),
		RobotType:   robotType.Model,
		RobotTypeID: strconv.FormatInt(robotType.ID, 10),
		RobotID:     strconv.FormatInt(robotID, 10),
	}, nil
}

func allocateDeviceIDSequence(tx *sqlx.Tx, factoryID, robotTypeID int64) (int64, error) {
	if tx.DriverName() == "sqlite" {
		return allocateDeviceIDSequenceSQLite(tx, factoryID, robotTypeID)
	}
	return allocateDeviceIDSequenceMySQL(tx, factoryID, robotTypeID)
}

func allocateDeviceIDSequenceMySQL(tx *sqlx.Tx, factoryID, robotTypeID int64) (int64, error) {
	if _, err := tx.Exec(`
		INSERT INTO device_id_sequences (factory_id, robot_type_id, next_sequence)
		VALUES (?, ?, 1)
		ON DUPLICATE KEY UPDATE updated_at = updated_at
	`, factoryID, robotTypeID); err != nil {
		return 0, fmt.Errorf("initialize sequence row: %w", err)
	}

	var sequence int64
	if err := tx.Get(&sequence, `
		SELECT next_sequence
		FROM device_id_sequences
		WHERE factory_id = ? AND robot_type_id = ?
		FOR UPDATE
	`, factoryID, robotTypeID); err != nil {
		return 0, fmt.Errorf("lock sequence row: %w", err)
	}
	if sequence < 1 {
		return 0, fmt.Errorf("invalid next_sequence %d", sequence)
	}

	if _, err := tx.Exec(`
		UPDATE device_id_sequences
		SET next_sequence = next_sequence + 1
		WHERE factory_id = ? AND robot_type_id = ?
	`, factoryID, robotTypeID); err != nil {
		return 0, fmt.Errorf("increment sequence row: %w", err)
	}

	return sequence, nil
}

func allocateDeviceIDSequenceSQLite(tx *sqlx.Tx, factoryID, robotTypeID int64) (int64, error) {
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO device_id_sequences (
			factory_id,
			robot_type_id,
			next_sequence,
			created_at,
			updated_at
		) VALUES (?, ?, 1, ?, ?)
	`, factoryID, robotTypeID, time.Now().UTC(), time.Now().UTC()); err != nil {
		return 0, fmt.Errorf("initialize sequence row: %w", err)
	}

	var sequence int64
	if err := tx.Get(&sequence, `
		SELECT next_sequence
		FROM device_id_sequences
		WHERE factory_id = ? AND robot_type_id = ?
	`, factoryID, robotTypeID); err != nil {
		return 0, fmt.Errorf("select sequence row: %w", err)
	}
	if sequence < 1 {
		return 0, fmt.Errorf("invalid next_sequence %d", sequence)
	}

	if _, err := tx.Exec(`
		UPDATE device_id_sequences
		SET next_sequence = next_sequence + 1,
			updated_at = ?
		WHERE factory_id = ? AND robot_type_id = ?
	`, time.Now().UTC(), factoryID, robotTypeID); err != nil {
		return 0, fmt.Errorf("increment sequence row: %w", err)
	}

	return sequence, nil
}

func formatRegisteredDeviceID(factoryID, robotTypeID, sequence int64) string {
	return fmt.Sprintf("AB-F%04d-T%04d-%06d", factoryID, robotTypeID, sequence)
}
