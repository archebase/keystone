// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

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
		transfer.GET("/events", h.GetDeviceEvents)
		transfer.POST("/upload_request", h.UploadRequest)
		transfer.POST("/upload_all", h.UploadAll)
		transfer.POST("/cancel", h.CancelUpload)
		transfer.POST("/upload_ack", h.ManualACK)

		// Recorder RPC forwarding
		transfer.GET("/recorder/:rpc_path", h.ForwardRecorderRPC)
		transfer.POST("/recorder/:rpc_path", h.ForwardRecorderRPC)
	}
}

// RegisterWebSocket registers the WebSocket endpoint
func (h *TransferHandler) RegisterWebSocket(engine *gin.Engine) {
	engine.GET("/transfer/:device_id", h.HandleWebSocket)
}

// HandleWebSocket upgrades the HTTP connection to WebSocket and manages the device session.
//
// @Summary      WebSocket endpoint for axon_transfer devices
// @Description  Upgrades to WebSocket; device_id identifies the connecting robot
// @Tags         transfer
// @Param        device_id  path  string  true  "Device ID"
// @Router       /transfer/{device_id} [get]
func (h *TransferHandler) HandleWebSocket(c *gin.Context) {
	h.HandleWebSocketRaw(c.Writer, c.Request, c.Param("device_id"))
}

// HandleWebSocketRaw handles WebSocket connections using raw http.ResponseWriter
// to avoid gin.ResponseWriter compatibility issues with nhooyr.io/websocket
func (h *TransferHandler) HandleWebSocketRaw(w http.ResponseWriter, r *http.Request, deviceID string) {
	log.Printf("[TRANSFER] WebSocket connection attempt for device: %s", deviceID)

	// Validate device exists in robots table (if DB is configured)
	if h.db != nil {
		// Add independent 5s timeout to avoid blocking on slow DB queries
		queryCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var count int
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
	defer conn.Close(websocket.StatusNormalClosure, "")

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
				if err := conn.Ping(context.Background()); err != nil {
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
	case "status":
		h.onStatus(dc, msg)
	default:
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
	log.Printf("[TRANSFER] Device %s: upload progress task=%s %d%%", dc.DeviceID, taskID, percent)
}

// onUploadComplete handles "upload_complete" and runs the Verified ACK flow:
//  1. Verify S3 files exist
//  2. Update episodes table
//  3. Send upload_ack to device
func (h *TransferHandler) onUploadComplete(ctx context.Context, dc *services.DeviceConn, msg map[string]interface{}) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	taskID := stringVal(data, "task_id")
	log.Printf("[TRANSFER] Device %s: upload complete task=%s", dc.DeviceID, taskID)

	// Step 1: Verify S3 files exist (skip if S3 client not configured)
	if h.s3 != nil {
		today := time.Now().Format("2006-01-02")
		mcapKey := fmt.Sprintf("%s/%s/%s/%s.mcap", h.factoryID, dc.DeviceID, today, taskID)
		jsonKey := fmt.Sprintf("%s/%s/%s/%s.json", h.factoryID, dc.DeviceID, today, taskID)

		mcapExists, err := h.s3.HeadObject(ctx, mcapKey)
		if err != nil {
			log.Printf("[TRANSFER] Device %s: S3 HeadObject error for key=%s: %v", dc.DeviceID, mcapKey, err)
			return
		}
		log.Printf("[TRANSFER] Device %s: S3 HeadObject result for key=%s: exists=%v", dc.DeviceID, mcapKey, mcapExists)
		jsonExists, err := h.s3.HeadObject(ctx, jsonKey)
		if err != nil {
			log.Printf("[TRANSFER] Device %s: S3 HeadObject error for key=%s: %v", dc.DeviceID, jsonKey, err)
			return
		}
		log.Printf("[TRANSFER] Device %s: S3 HeadObject result for key=%s: exists=%v", dc.DeviceID, jsonKey, jsonExists)

		if !mcapExists || !jsonExists {
			log.Printf("[TRANSFER] Device %s: S3 files not found for task=%s (mcapKey=%s mcap=%v jsonKey=%s json=%v), skipping ACK",
				dc.DeviceID, taskID, mcapKey, mcapExists, jsonKey, jsonExists)
			return
		}

		// Step 2: Insert into episodes table (skip if DB not configured)
		if h.db != nil {
			today := time.Now().Format("2006-01-02")
			mcapKey := fmt.Sprintf("%s/%s/%s/%s.mcap", h.factoryID, dc.DeviceID, today, taskID)
			jsonKey := fmt.Sprintf("%s/%s/%s/%s.json", h.factoryID, dc.DeviceID, today, taskID)
			episodeID := uuid.New().String()

			_, dbErr := h.db.ExecContext(ctx,
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
				log.Printf("[TRANSFER] Device %s: DB insert failed for task=%s: %v", dc.DeviceID, taskID, dbErr)
				// Continue to send ACK even if DB insert fails to avoid blocking the device
			}
		}
	}

	// Step 3: Send upload_ack
	ackMsg := map[string]interface{}{
		"type":    "upload_ack",
		"task_id": taskID,
	}
	dc.WriteMu.Lock()
	defer dc.WriteMu.Unlock()
	if err := wsjson.Write(ctx, dc.Conn, ackMsg); err != nil {
		log.Printf("[TRANSFER] Device %s: failed to send upload_ack for task=%s: %v", dc.DeviceID, taskID, err)
		return
	}
	dc.RecordEvent("outbound", ackMsg)
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
	log.Printf("[UPLOAD_FAILED] Received from device %s: full message=%+v", dc.DeviceID, msg)

	// Try to extract bucket info if present
	if bucket, ok := data["bucket"].(string); ok {
		log.Printf("[UPLOAD_FAILED] Device %s: task=%s bucket=%s reason=%q retries=%d",
			dc.DeviceID, taskID, bucket, reason, retryCount)
	} else {
		log.Printf("[UPLOAD_FAILED] Device %s: task=%s reason=%q retries=%d",
			dc.DeviceID, taskID, reason, retryCount)
	}

	// Log configured S3 bucket for comparison
	if h.s3 != nil {
		log.Printf("[UPLOAD_FAILED] Keystone configured bucket: %s", h.s3.Bucket())
	}
}

// onStatus handles "status" message and updates the device status snapshot
func (h *TransferHandler) onStatus(dc *services.DeviceConn, msg map[string]interface{}) {
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
// @Router       /api/v1/transfer/devices [get]
func (h *TransferHandler) ListDevices(c *gin.Context) {
	devices := h.hub.ListDevices()
	c.JSON(http.StatusOK, gin.H{"devices": devices})
}

// GetDeviceEvents returns recent events for a device.
//
// @Summary      Get device events
// @Description  Returns up to `limit` recent inbound/outbound events for the device
// @Tags         transfer
// @Produce      json
// @Param        device_id  path   string  true   "Device ID"
// @Param        limit      query  int     false  "Max events to return (default 100)"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Router       /api/v1/transfer/{device_id}/events [get]
func (h *TransferHandler) GetDeviceEvents(c *gin.Context) {
	deviceID := c.Param("device_id")
	limit := 100
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
	}

	dc := h.hub.Get(deviceID)
	if dc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not connected"})
		return
	}

	events := dc.Events(limit)
	c.JSON(http.StatusOK, gin.H{"device_id": deviceID, "events": events})
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
// @Router       /api/v1/transfer/{device_id}/upload_request [post]
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
// @Router       /api/v1/transfer/{device_id}/upload_all [post]
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
	log.Printf("[UPLOAD_ALL] Device %s current status: pending=%d uploading=%d failed=%d waiting_ack=%d",
		deviceID, dc.Status.PendingCount, dc.Status.UploadingCount, dc.Status.FailedCount, dc.Status.WaitingACKCount)

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
// @Router       /api/v1/transfer/{device_id}/cancel [post]
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
// @Router       /api/v1/transfer/{device_id}/upload_ack [post]
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

// ForwardRecorderRPC proxies a request to the device's axon_recorder RPC server.
//
// @Summary      Forward RPC to device recorder
// @Description  Proxies GET/POST to http://{device_ip}:{recorder_port}/rpc/{rpc_path}
// @Tags         transfer
// @Param        device_id  path  string  true  "Device ID"
// @Param        rpc_path   path  string  true  "RPC path (e.g. status, config, begin)"
// @Success      200
// @Failure      404  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Router       /api/v1/transfer/{device_id}/recorder/{rpc_path} [get]
// @Router       /api/v1/transfer/{device_id}/recorder/{rpc_path} [post]
func (h *TransferHandler) ForwardRecorderRPC(c *gin.Context) {
	deviceID := c.Param("device_id")
	rpcPath := c.Param("rpc_path")

	dc := h.hub.Get(deviceID)
	if dc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not connected"})
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d/rpc/%s", dc.RemoteIP, h.cfg.RecorderRPCPort, rpcPath)

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	req.Header = c.Request.Header.Clone()

	resp, err := h.client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("recorder unreachable: %v", err)})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
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
