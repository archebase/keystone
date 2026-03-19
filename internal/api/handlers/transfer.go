// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"
)

// TransferHandler handles WebSocket connections and REST API for Transfer Service
type TransferHandler struct {
	hub       *services.TransferHub
	cfg       *config.TransferConfig
	db        *sqlx.DB
	s3        *s3.Client
	bucket    string
	factoryID string
	client    *http.Client
}

// NewTransferHandler creates a new TransferHandler.
// db and s3Client may be nil; Verified ACK will be skipped if either is absent.
func NewTransferHandler(hub *services.TransferHub, cfg *config.TransferConfig, db *sqlx.DB, s3Client *s3.Client, bucket string, factoryID string) *TransferHandler {
	return &TransferHandler{
		hub:       hub,
		cfg:       cfg,
		db:        db,
		s3:        s3Client,
		bucket:    bucket,
		factoryID: factoryID,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// RegisterRoutes registers all transfer-related REST routes
func (h *TransferHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	// Note: apiV1 is already /transfer (set by server.go)
	// Devices list endpoint
	apiV1.GET("/devices", h.ListDevices)

	transfer := apiV1.Group(":device_id")
	{
		transfer.POST("/upload_request", h.UploadRequest)
		transfer.POST("/upload_all", h.UploadAll)
		transfer.POST("/status_query", h.StatusQuery)
		transfer.POST("/cancel", h.CancelUpload)
		transfer.POST("/upload_ack", h.ManualACK)
	}
}

// HandleWebSocket handles WebSocket connections using raw http.ResponseWriter
func (h *TransferHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request, deviceID string) {

	// Validate device exists in robots table (if DB is configured)
	if h.db != nil {
		// Add independent 5s timeout to avoid blocking on slow DB queries
		queryCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var count int
		// #nosec G701 -- Set aside for now
		if err := h.db.GetContext(queryCtx, &count,
			"SELECT COUNT(1) FROM robots WHERE device_id = ? AND deleted_at IS NULL", deviceID,
		); err != nil {
			logger.Printf("[TRANSFER] Device %s: DB query error: %v", deviceID, err)
		}
		// Check count regardless of DB error (count defaults to 0 on error)
		if count == 0 {
			logger.Printf("[TRANSFER] Device %s: robot not found in database", deviceID)
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // allow any origin in dev; tighten in production
	})
	if err != nil {
		logger.Printf("[TRANSFER] Device %s: WebSocket accept error: %v", deviceID, err)
		return
	}
	defer func() {
		if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
			logger.Printf("[TRANSFER] WebSocket close error for device %s: %v", deviceID, err)
		}
	}()

	// Create context for this connection
	ctx := r.Context()

	// Start ping handler to automatically respond to client pings
	// This prevents connection timeout due to idle connections
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.Ping(ctx); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	remoteIP := extractIP(r.RemoteAddr)

	dc := h.hub.NewDeviceConn(conn, deviceID, remoteIP)
	h.hub.Connect(deviceID, dc)
	defer h.hub.Disconnect(deviceID)

	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Transfer %s connected from %s", deviceID, remoteIP)

	// Read loop: use ctx directly for infinite wait.
	// context.WithTimeout(ctx, 0) would set deadline=now and cause immediate timeout,
	// so we must NOT wrap ctx with a zero timeout here.
	// Ping keepalive is handled by the goroutine above.
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			logger.Printf("[TRANSFER] Device %s disconnected: %v", deviceID, err)
			break
		}

		var msg map[string]interface{}
		if jsonErr := json.Unmarshal(raw, &msg); jsonErr != nil {
			logger.Printf("[TRANSFER] Device %s: invalid JSON: %v", deviceID, jsonErr)
			continue
		}

		dc.LastSeenAt = time.Now()
		dc.RecordEvent("inbound", msg)

		// Route message by type
		h.handleMessage(ctx, dc, msg)
	}
}

// handleMessage dispatches an inbound WebSocket message to the appropriate handler
func (h *TransferHandler) handleMessage(ctx context.Context, dc *services.DeviceConn, msg map[string]interface{}) {
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "connected":
		h.onConnected(dc, msg)
	case "upload_started":
		h.onUploadStarted(dc, msg)
	case "upload_progress":
		h.onUploadProgress(dc, msg)
	case "upload_complete":
		h.onUploadComplete(ctx, dc, msg)
	case "upload_failed":
		h.onUploadFailed(dc, msg)
	case "upload_not_found":
		h.onUploadNotFound(dc, msg)
	case "status":
		h.onStatus(dc, msg)
	default:
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: unknown message type %q", dc.DeviceID, msgType)
	}
}

// onConnected handles the initial "connected" message from the device
func (h *TransferHandler) onConnected(dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	s := services.DeviceStatus{
		Version:         stringVal(data, "version"),
		PendingCount:    intVal(data, "pending_count"),
		UploadingCount:  intVal(data, "uploading_count"),
		WaitingACKCount: intVal(data, "waiting_ack_count"),
		FailedCount:     intVal(data, "failed_count"),
	}
	dc.UpdateStatus(s)
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Transfer %s connected: version=%s pending=%d uploading=%d failed=%d",
		dc.DeviceID, s.Version, s.PendingCount, s.UploadingCount, s.FailedCount)
}

// onUploadStarted handles "upload_started" message
func (h *TransferHandler) onUploadStarted(dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	taskID := stringVal(data, "task_id")
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: upload started task=%s total_bytes=%d",
		dc.DeviceID, taskID, int64Val(data, "total_bytes"))
}

// onUploadProgress handles "upload_progress" message
func (h *TransferHandler) onUploadProgress(dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	taskID := stringVal(data, "task_id")
	percent := intVal(data, "percent")
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: upload progress task=%s %d%%", dc.DeviceID, taskID, percent)
}

// onUploadComplete handles "upload_complete" and runs the Verified ACK flow:
//  1. Verify S3 files exist
//  2. Update episodes table
//  3. Send upload_ack to device
func (h *TransferHandler) onUploadComplete(ctx context.Context, dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: upload complete data is nil", dc.DeviceID)
		return
	}
	taskID := stringVal(data, "task_id")
	if taskID == "" {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: upload complete taskID is empty", dc.DeviceID)
		return
	}
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: upload complete for task=%s", dc.DeviceID, taskID)

	if h.s3 == nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: S3 not configured, skipping upload_complete for task=%s", dc.DeviceID, taskID)
		return
	}
	if h.db == nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: DB not configured, skipping upload_complete for task=%s", dc.DeviceID, taskID)
		return
	}

	// Step 1: Verify S3 files exist (parallel execution)
	today := time.Now().Format("2006-01-02")
	mcapKey := fmt.Sprintf("%s/%s/%s/%s.mcap", h.factoryID, dc.DeviceID, today, taskID)
	jsonKey := fmt.Sprintf("%s/%s/%s/%s.json", h.factoryID, dc.DeviceID, today, taskID)

	var mcapExists, jsonExists bool
	var mcapErr, jsonErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		mcapExists, mcapErr = h.s3.HeadObject(ctx, mcapKey)
	}()

	go func() {
		defer wg.Done()
		jsonExists, jsonErr = h.s3.HeadObject(ctx, jsonKey)
	}()

	wg.Wait()

	if mcapErr != nil || jsonErr != nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: S3 HeadObject error", dc.DeviceID)
		return
	}

	if !mcapExists || !jsonExists {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: S3 files not found for task=%s, skipping ACK",
			dc.DeviceID, taskID)
		return
	}

	// Step 2: Insert into episodes table
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: DB begin transaction error for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			logger.Printf("[TRANSFER] Transaction rollback error: %v", err)
		}
	}()

	// Check if mcap_path and sidecar_path already exist in database
	var count int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(1) FROM episodes WHERE mcap_path = ? OR sidecar_path = ?", mcapKey, jsonKey,
	).Scan(&count)
	if err != nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: DB query error for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	if count > 0 {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: task=%s already exists in DB (by mcap_path or sidecar_path), skipping insert", dc.DeviceID, taskID)
	} else {
		var taskRow struct {
			ID             int64         `db:"id"`
			BatchID        int64         `db:"batch_id"`
			OrderID        int64         `db:"order_id"`
			SceneID        int64         `db:"scene_id"`
			SceneName      string        `db:"scene_name"`
			WorkstationID  sql.NullInt64 `db:"workstation_id"`
			FactoryID      sql.NullInt64 `db:"factory_id"`
			OrganizationID sql.NullInt64 `db:"organization_id"`
			SOPID          int64         `db:"sop_id"`
		}

		err = tx.QueryRowContext(ctx, `SELECT
			id,
			batch_id,
			order_id,
			scene_id,
			COALESCE(scene_name, '') AS scene_name,
			workstation_id,
			factory_id,
			organization_id,
			sop_id
		FROM tasks
		WHERE task_id = ? AND deleted_at IS NULL`, taskID).Scan(
			&taskRow.ID,
			&taskRow.BatchID,
			&taskRow.OrderID,
			&taskRow.SceneID,
			&taskRow.SceneName,
			&taskRow.WorkstationID,
			&taskRow.FactoryID,
			&taskRow.OrganizationID,
			&taskRow.SOPID,
		)
		if err != nil {
			return
		}

		episodeID := uuid.New().String()
		_, dbErr := tx.ExecContext(ctx,
			`INSERT INTO episodes (
				episode_id,
				task_id,
				batch_id,
				order_id,
				scene_id,
				scene_name,
				workstation_id,
				factory_id,
				organization_id,
				sop_id,
				mcap_path,
				sidecar_path
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			episodeID,
			taskRow.ID,
			taskRow.BatchID,
			taskRow.OrderID,
			taskRow.SceneID,
			taskRow.SceneName,
			taskRow.WorkstationID,
			taskRow.FactoryID,
			taskRow.OrganizationID,
			taskRow.SOPID,
			mcapKey,
			jsonKey,
		)
		if dbErr != nil {
			// #nosec G706 -- Set aside for now
			logger.Printf("[TRANSFER] Device %s: DB insert failed for task=%s: %v", dc.DeviceID, taskID, dbErr)
			return
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: DB commit error for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}

	// Step 3: Send upload_ack
	ackMsg := map[string]interface{}{
		"type":    "upload_ack",
		"task_id": taskID,
	}
	dc.WriteMu.Lock()
	if err := wsjson.Write(ctx, dc.Conn, ackMsg); err != nil {
		dc.WriteMu.Unlock()
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: failed to send upload_ack for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	dc.WriteMu.Unlock()
	dc.RecordEvent("outbound", ackMsg)
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: upload_ack sent for task=%s", dc.DeviceID, taskID)
}

// onUploadFailed handles "upload_failed" message
func (h *TransferHandler) onUploadFailed(dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	taskID := stringVal(data, "task_id")
	reason := stringVal(data, "reason")
	retryCount := intVal(data, "retry_count")

	// Log full message for debugging
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Received from device %s: full message=%+v", dc.DeviceID, msg)

	// Try to extract bucket info if present
	if bucket, ok := data["bucket"].(string); ok {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: task=%s bucket=%s reason=%q retries=%d",
			dc.DeviceID, taskID, bucket, reason, retryCount)
	} else {
		// #nosec G706 -- Set aside for now
		logger.Printf("[TRANSFER] Device %s: task=%s reason=%q retries=%d",
			dc.DeviceID, taskID, reason, retryCount)
	}

	// Log configured S3 bucket for comparison
	if h.s3 != nil {
		logger.Printf("[TRANSFER] Keystone configured bucket: %s", h.s3.Bucket())
	}
}

// onUploadNotFound handles "upload_not_found" message
func (h *TransferHandler) onUploadNotFound(dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	taskID := stringVal(data, "task_id")

	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: task=%s not found", dc.DeviceID, taskID)
}

// onStatus handles "status" message and updates the device status snapshot
func (h *TransferHandler) onStatus(dc *services.DeviceConn, msg map[string]interface{}) {
	// #nosec G706 -- Set aside for now
	logger.Printf("[TRANSFER] Device %s: received status update", dc.DeviceID)
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}

	// Parse waiting_ack_task_ids
	var waitingIDs []string
	if raw, ok := data["waiting_ack_task_ids"].([]interface{}); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				waitingIDs = append(waitingIDs, s)
			}
		}
	}

	s := services.DeviceStatus{
		PendingCount:      intVal(data, "pending_count"),
		UploadingCount:    intVal(data, "uploading_count"),
		WaitingACKCount:   intVal(data, "waiting_ack_count"),
		WaitingACKTaskIDs: waitingIDs,
		CompletedCount:    intVal(data, "completed_count"),
		FailedCount:       intVal(data, "failed_count"),
		PendingBytes:      int64Val(data, "pending_bytes"),
		BytesPerSec:       int64Val(data, "bytes_per_sec"),
	}
	dc.UpdateStatus(s)
}

// ListDevices returns all currently connected devices.
//
// @Summary      List connected devices
// @Description  Returns metadata for all devices currently connected via WebSocket
// @Tags         transfer
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /transfer/devices [get]
func (h *TransferHandler) ListDevices(c *gin.Context) {
	devices := h.hub.ListDevices()
	c.JSON(http.StatusOK, gin.H{"devices": devices})
}

// UploadRequest sends an upload_request message to the device.
//
// @Summary      Request upload for a specific task
// @Tags         transfer
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Device ID"
// @Param        body       body  object  true  "task_id and priority"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /transfer/{device_id}/upload_request [post]
func (h *TransferHandler) UploadRequest(c *gin.Context) {
	deviceID := c.Param("device_id")

	var body struct {
		TaskID   string `json:"task_id" binding:"required"`
		Priority int    `json:"priority"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg := map[string]interface{}{
		"type":     "upload_request",
		"task_id":  body.TaskID,
		"priority": body.Priority,
	}
	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// UploadAll sends an upload_all message to the device.
//
// @Summary      Request upload of all pending tasks
// @Tags         transfer
// @Produce      json
// @Param        device_id  path  string  true  "Device ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /transfer/{device_id}/upload_all [post]
func (h *TransferHandler) UploadAll(c *gin.Context) {
	deviceID := c.Param("device_id")

	logger.Printf("[TRANSFER] Device %s: received upload_all request", deviceID)

	// Check if device is connected
	dc := h.hub.Get(deviceID)
	if dc == nil {
		logger.Printf("[TRANSFER] Device %s: not connected", deviceID)
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("device %s not connected", deviceID)})
		return
	}

	logger.Printf("[TRANSFER] Device %s: connected, remote_ip=%s", deviceID, dc.RemoteIP)
	status := dc.GetStatus()
	logger.Printf("[TRANSFER] Device %s: current status is pending=%d uploading=%d failed=%d waiting_ack=%d",
		deviceID, status.PendingCount, status.UploadingCount, status.FailedCount, status.WaitingACKCount)

	msg := map[string]interface{}{"type": "upload_all"}
	logger.Printf("[TRANSFER] Sending message to device %s: %+v", deviceID, msg)

	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		logger.Printf("[TRANSFER] Failed to send message to device %s: %v", deviceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	logger.Printf("[TRANSFER] Message sent successfully to device %s", deviceID)
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// CancelUpload sends a cancel message to the device.
//
// @Summary      Cancel an upload task
// @Tags         transfer
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Device ID"
// @Param        body       body  object  true  "task_id to cancel"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /transfer/{device_id}/cancel [post]
func (h *TransferHandler) CancelUpload(c *gin.Context) {
	deviceID := c.Param("device_id")

	var body struct {
		TaskID string `json:"task_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg := map[string]interface{}{
		"type":    "cancel",
		"task_id": body.TaskID,
	}
	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// StatusQuery sends a status_query message to the device to request current status.
//
// @Summary      Query device status
// @Tags         transfer
// @Produce      json
// @Param        device_id  path  string  true  "Device ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /transfer/{device_id}/status_query [post]
func (h *TransferHandler) StatusQuery(c *gin.Context) {
	deviceID := c.Param("device_id")

	// Check if device is connected
	dc := h.hub.Get(deviceID)
	if dc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("device %s not connected", deviceID)})
		return
	}

	msg := map[string]interface{}{"type": "status_query"}
	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// ManualACK sends an upload_ack message to the device.
//
// @Summary      Manually acknowledge an upload
// @Tags         transfer
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Device ID"
// @Param        body       body  object  true  "task_id to acknowledge"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /transfer/{device_id}/upload_ack [post]
func (h *TransferHandler) ManualACK(c *gin.Context) {
	deviceID := c.Param("device_id")

	var body struct {
		TaskID string `json:"task_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg := map[string]interface{}{
		"type":    "upload_ack",
		"task_id": body.TaskID,
	}
	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// extractIP extracts the IP address from a RemoteAddr string (host:port)
func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// --- JSON helper utilities ---

func stringVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func intVal(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		i, _ := strconv.Atoi(v)
		return i
	}
	return 0
}

func int64Val(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
