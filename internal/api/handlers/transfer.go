// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"
)

// TransferHandler handles WebSocket connections and REST API for Fleet Manager
type TransferHandler struct {
	hub       *services.TransferHub
	cfg       *config.FleetManagerConfig
	db        *sql.DB
	s3        *s3.Client
	bucket    string
	factoryID string
	client    *http.Client
}

// NewTransferHandler creates a new TransferHandler.
// db and s3Client may be nil; Verified ACK will be skipped if either is absent.
func NewTransferHandler(hub *services.TransferHub, cfg *config.FleetManagerConfig, db *sql.DB, s3Client *s3.Client, bucket string, factoryID string) *TransferHandler {
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

// RegisterCallbackRoutes registers callback routes for handling external events.
// It sets up POST /finish endpoint to handle recording completion callbacks.
func (h *TransferHandler) RegisterCallbackRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/finish", h.OnRecordingFinish)
}

// HandleWebSocket handles WebSocket connections using raw http.ResponseWriter
func (h *TransferHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request, deviceID string) {
	log.Printf("[TRANSFER] WebSocket connection attempt for device: %s", deviceID)

	// Validate device exists in robots table (if DB is configured)
	if h.db != nil {
		// Add independent 5s timeout to avoid blocking on slow DB queries
		queryCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var count int
		// #nosec G701 -- Set aside for now
		err := h.db.QueryRowContext(queryCtx,
			"SELECT COUNT(1) FROM robots WHERE device_id = ? AND deleted_at IS NULL", deviceID,
		).Scan(&count)
		if err != nil {
			log.Printf("[TRANSFER] Device %s: DB query error: %v", deviceID, err)
		}
		if count == 0 {
			log.Printf("[TRANSFER] Device %s: robot not found in database", deviceID)
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // allow any origin in dev; tighten in production
	})
	if err != nil {
		log.Printf("[TRANSFER] Device %s: WebSocket accept error: %v", deviceID, err)
		return
	}
	defer func() {
		if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
			log.Printf("[TRANSFER] WebSocket close error for device %s: %v", deviceID, err)
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
	log.Printf("[TRANSFER] Device %s connected from %s", deviceID, remoteIP)

	// Read loop: use ctx directly for infinite wait.
	// context.WithTimeout(ctx, 0) would set deadline=now and cause immediate timeout,
	// so we must NOT wrap ctx with a zero timeout here.
	// Ping keepalive is handled by the goroutine above.
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			log.Printf("[TRANSFER] Device %s disconnected: %v", deviceID, err)
			break
		}

		var msg map[string]interface{}
		if jsonErr := json.Unmarshal(raw, &msg); jsonErr != nil {
			log.Printf("[TRANSFER] Device %s: invalid JSON: %v", deviceID, jsonErr)
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
		log.Printf("[TRANSFER] Device %s: unknown message type %q", dc.DeviceID, msgType)
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
	log.Printf("[TRANSFER] Device %s connected: version=%s pending=%d uploading=%d failed=%d",
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
	log.Printf("[TRANSFER] Device %s: upload started task=%s total_bytes=%d",
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
	log.Printf("[TRANSFER] Device %s: upload progress task=%s %d%%", dc.DeviceID, taskID, percent)
}

// onUploadComplete handles "upload_complete" and runs the Verified ACK flow:
//  1. Verify S3 files exist
//  2. Update episodes table
//  3. Send upload_ack to device
func (h *TransferHandler) onUploadComplete(ctx context.Context, dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: upload complete data is nil", dc.DeviceID)
		return
	}
	taskID := stringVal(data, "task_id")
	if taskID == "" {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: upload complete taskID is empty", dc.DeviceID)
		return
	}
	// #nosec G706 -- Set aside for now
	log.Printf("[TRANSFER] Device %s: upload complete task=%s", dc.DeviceID, taskID)

	if h.s3 == nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: S3 not configured, skipping upload_complete for task=%s", dc.DeviceID, taskID)
		return
	}
	if h.db == nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: DB not configured, skipping upload_complete for task=%s", dc.DeviceID, taskID)
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
		log.Printf("[TRANSFER] Device %s: S3 HeadObject error", dc.DeviceID)
		return
	}

	if !mcapExists || !jsonExists {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: S3 files not found for task=%s, skipping ACK",
			dc.DeviceID, taskID)
		return
	}

	// Step 2: Insert into episodes table
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: DB begin transaction error for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			log.Printf("[TRANSFER] Transaction rollback error: %v", err)
		}
	}()

	// Check if mcap_path and sidecar_path already exist in database
	var count int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(1) FROM episodes WHERE mcap_path = ? OR sidecar_path = ?", mcapKey, jsonKey,
	).Scan(&count)
	if err != nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: DB query error for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	if count > 0 {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: task=%s already exists in DB (by mcap_path or sidecar_path), skipping insert", dc.DeviceID, taskID)
	} else {
		// TODO: query the tasks table based on task_id (string) to get the corresponding id(int)
		episodeID := uuid.New().String()
		_, dbErr := tx.ExecContext(ctx,
			`INSERT INTO episodes (
				episode_id,
				task_id,
				batch_id,
				order_id,
				scene_id,
				mcap_path,
				sidecar_path
			) VALUES (?, 0, 0, 0, 0, ?, ?)`,
			episodeID, mcapKey, jsonKey,
		)
		if dbErr != nil {
			// #nosec G706 -- Set aside for now
			log.Printf("[TRANSFER] Device %s: DB insert failed for task=%s: %v", dc.DeviceID, taskID, dbErr)
			return
		}
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: DB insert success for task=%s episode=%s", dc.DeviceID, taskID, episodeID)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		// #nosec G706 -- Set aside for now
		log.Printf("[TRANSFER] Device %s: DB commit error for task=%s: %v", dc.DeviceID, taskID, err)
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
		log.Printf("[TRANSFER] Device %s: failed to send upload_ack for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	dc.WriteMu.Unlock()
	dc.RecordEvent("outbound", ackMsg)
	// #nosec G706 -- Set aside for now
	log.Printf("[TRANSFER] Device %s: upload_ack sent for task=%s", dc.DeviceID, taskID)
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
	log.Printf("[UPLOAD_FAILED] Received from device %s: full message=%+v", dc.DeviceID, msg)

	// Try to extract bucket info if present
	if bucket, ok := data["bucket"].(string); ok {
		// #nosec G706 -- Set aside for now
		log.Printf("[UPLOAD_FAILED] Device %s: task=%s bucket=%s reason=%q retries=%d",
			dc.DeviceID, taskID, bucket, reason, retryCount)
	} else {
		// #nosec G706 -- Set aside for now
		log.Printf("[UPLOAD_FAILED] Device %s: task=%s reason=%q retries=%d",
			dc.DeviceID, taskID, reason, retryCount)
	}

	// Log configured S3 bucket for comparison
	if h.s3 != nil {
		log.Printf("[UPLOAD_FAILED] Keystone configured bucket: %s", h.s3.Bucket())
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
	log.Printf("[TRANSFER] Device %s: task=%s not found", dc.DeviceID, taskID)
}

// onStatus handles "status" message and updates the device status snapshot
func (h *TransferHandler) onStatus(dc *services.DeviceConn, msg map[string]interface{}) {
	// #nosec G706 -- Set aside for now
	log.Printf("[TRANSFER] Device %s: received status update", dc.DeviceID)
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
	if err := h.sendToDevice(c.Request.Context(), deviceID, msg); err != nil {
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

	log.Printf("[UPLOAD_ALL] Received upload_all request for device: %s", deviceID)

	// Check if device is connected
	dc := h.hub.Get(deviceID)
	if dc == nil {
		log.Printf("[UPLOAD_ALL] Device %s not connected - returning 404", deviceID)
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("device %s not connected", deviceID)})
		return
	}

	log.Printf("[UPLOAD_ALL] Device %s is connected, remote_ip=%s", deviceID, dc.RemoteIP)
	status := dc.GetStatus()
	log.Printf("[UPLOAD_ALL] Device %s current status: pending=%d uploading=%d failed=%d waiting_ack=%d",
		deviceID, status.PendingCount, status.UploadingCount, status.FailedCount, status.WaitingACKCount)

	msg := map[string]interface{}{"type": "upload_all"}
	log.Printf("[UPLOAD_ALL] Sending message to device %s: %+v", deviceID, msg)

	if err := h.sendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		log.Printf("[UPLOAD_ALL] Failed to send message to device %s: %v", deviceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	log.Printf("[UPLOAD_ALL] Message sent successfully to device %s, returning 200 OK", deviceID)
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
	if err := h.sendToDevice(c.Request.Context(), deviceID, msg); err != nil {
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
	if err := h.sendToDevice(c.Request.Context(), deviceID, msg); err != nil {
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
	if err := h.sendToDevice(c.Request.Context(), deviceID, msg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// sendToDevice sends a JSON message to a connected device via WebSocket
func (h *TransferHandler) sendToDevice(ctx context.Context, deviceID string, msg map[string]interface{}) error {
	dc := h.hub.Get(deviceID)
	if dc == nil {
		log.Printf("[sendToDevice] Device %s not found in hub", deviceID)
		return fmt.Errorf("device %s not connected", deviceID)
	}

	log.Printf("[sendToDevice] Acquiring write lock for device %s", deviceID)
	dc.WriteMu.Lock()
	defer dc.WriteMu.Unlock()

	log.Printf("[sendToDevice] Writing message to device %s: %+v", deviceID, msg)
	if err := wsjson.Write(ctx, dc.Conn, msg); err != nil {
		log.Printf("[sendToDevice] wsjson.Write failed for device %s: %v", deviceID, err)
		return fmt.Errorf("failed to send message to device %s: %w", deviceID, err)
	}

	log.Printf("[sendToDevice] Message written successfully, recording event for device %s", deviceID)
	dc.RecordEvent("outbound", msg)
	log.Printf("[sendToDevice] Successfully sent message to device %s", deviceID)
	return nil
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

// RecordingFinishCallback represents the callback payload from axon recorder
type RecordingFinishCallback struct {
	TaskID        string   `json:"task_id"`
	DeviceID      string   `json:"device_id"`
	Status        string   `json:"status"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	DurationSec   float64  `json:"duration_sec"`
	MessageCount  int64    `json:"message_count"`
	FileSizeBytes int64    `json:"file_size_bytes"`
	OutputPath    string   `json:"output_path"`
	SidecarPath   string   `json:"sidecar_path"`
	Topics        []string `json:"topics"`
	Metadata      struct {
		Scene    string   `json:"scene"`
		Subscene string   `json:"subscene"`
		Skills   []string `json:"skills"`
		Factory  string   `json:"factory"`
	} `json:"metadata"`
	Error string `json:"error"`
}

// OnRecordingFinish handles the POST /callbacks/finish callback from axon recorder
// This endpoint is called when a recording finishes, triggering the upload process
// @Router       /callbacks/finish [post]
func (h *TransferHandler) OnRecordingFinish(c *gin.Context) {
	var callback RecordingFinishCallback
	if err := c.ShouldBindJSON(&callback); err != nil {
		log.Printf("[OnRecordingFinish] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "Invalid request body: " + err.Error(),
		})
		return
	}

	log.Printf("[OnRecordingFinish] Received finish callback: task_id=%s, device_id=%s, status=%s, output_path=%s, file_size=%d, message_count=%d, duration_sec=%.2f",
		callback.TaskID, callback.DeviceID, callback.Status, callback.OutputPath, callback.FileSizeBytes, callback.MessageCount, callback.DurationSec)
	log.Printf("[OnRecordingFinish] Topics: %v", callback.Topics)
	log.Printf("[OnRecordingFinish] Metadata: scene=%s, subscene=%s, skills=%v, factory=%s",
		callback.Metadata.Scene, callback.Metadata.Subscene, callback.Metadata.Skills, callback.Metadata.Factory)

	// Validate required fields
	if callback.TaskID == "" {
		log.Printf("[OnRecordingFinish] Missing task_id in callback")
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "Missing required field: task_id",
		})
		return
	}

	if callback.OutputPath == "" {
		log.Printf("[OnRecordingFinish] No output_path provided for task_id=%s, skipping upload", callback.TaskID)
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "No output file to upload",
		})
		return
	}

	deviceID := callback.DeviceID
	if deviceID == "" {
		log.Printf("[OnRecordingFinish] Missing device_id in callback")
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "Missing required field: device_id",
		})
		return
	}

	dc := h.hub.Get(deviceID)
	if dc == nil {
		log.Printf("[OnRecordingFinish] Device %s not found in hub (task_id=%s), cannot trigger upload", deviceID, callback.TaskID)
		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"message":          "Recording finished, device not connected for auto-upload",
			"device_connected": false,
			"task_id":          callback.TaskID,
		})
		return
	}

	log.Printf("[OnRecordingFinish] Found device %s, triggering upload for task_id=%s", deviceID, callback.TaskID)

	uploadRequest := map[string]interface{}{
		"type":     "upload_request",
		"task_id":  callback.TaskID,
		"priority": 1,
	}

	if err := h.sendToDevice(c.Request.Context(), deviceID, uploadRequest); err != nil {
		log.Printf("[OnRecordingFinish] Failed to send upload_request to device %s: %v", deviceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "Failed to trigger upload: " + err.Error(),
		})
		return
	}

	log.Printf("[OnRecordingFinish] Successfully triggered upload for task_id=%s on device %s",
		callback.TaskID, deviceID)

	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"message":         "Upload triggered successfully",
		"task_id":         callback.TaskID,
		"device_id":       deviceID,
		"output_path":     callback.OutputPath,
		"file_size_bytes": callback.FileSizeBytes,
	})
}
