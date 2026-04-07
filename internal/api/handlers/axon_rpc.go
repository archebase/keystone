// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
)

// RecorderHandler handles REST and WebSocket traffic for Axon Recorder RPC.
type RecorderHandler struct {
	hub *services.RecorderHub
	cfg *config.RecorderConfig
	db  *sqlx.DB
}

// NewRecorderHandler creates a new RecorderHandler.
func NewRecorderHandler(hub *services.RecorderHub, cfg *config.RecorderConfig, db *sqlx.DB) *RecorderHandler {
	return &RecorderHandler{hub: hub, cfg: cfg, db: db}
}

// ConfigRequest represents the request body for config RPC.
// @Description Request body for recorder config
type ConfigRequest struct {
	// Recorder task configuration
	TaskConfig RecorderTaskConfig `json:"task_config"`
}

// RecorderTaskConfig represents task_config payload for recorder config.
// @Description Detailed recorder task configuration
type RecorderTaskConfig struct {
	// Unique task identifier
	TaskID string `json:"task_id"`
	// Recorder device identifier
	// @default robot-001
	DeviceID string `json:"device_id,omitempty"`
	// Data collector identifier
	// @default collector-001
	DataCollectorID string `json:"data_collector_id,omitempty"`
	// Parent order or job identifier
	// @default order-001
	OrderID string `json:"order_id,omitempty"`
	// Human operator identifier
	// @default alice
	OperatorName string `json:"operator_name,omitempty"`
	// Recording scene label
	// @default warehouse_pickup
	Scene string `json:"scene,omitempty"`
	// Recording subscene label
	// @default aisle_a
	Subscene string `json:"subscene,omitempty"`
	// Skill tags associated with this recording
	// @default ["pick","place"]
	Skills []string `json:"skills,omitempty"`
	// Factory identifier
	// @default factory-shanghai
	Factory string `json:"factory,omitempty"`
	// Topic list to record
	// @default ["/imu/data","/camera0/rgb"]
	Topics []string `json:"topics,omitempty"`
	// Callback URL invoked when recording starts
	// @default http://127.0.0.1:9999/api/v1/tasks/start
	StartCallbackURL string `json:"start_callback_url,omitempty"`
	// Callback URL invoked when recording finishes
	// @default http://127.0.0.1:9999/api/v1/tasks/finish
	FinishCallbackURL string `json:"finish_callback_url,omitempty"`
	// User JWT token for callback authentication
	// @default eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...
	UserToken string `json:"user_token,omitempty"`
	// Recording start timestamp in ISO8601 format
	// @default 2026-03-13T10:00:00Z
	StartedAt string `json:"started_at,omitempty"`
}

// BeginRequest represents the request body for begin RPC.
// @Description Request body for starting a recording
type BeginRequest struct {
	// Task ID for the recording session
	// @example task-001
	TaskID string `json:"task_id"`
}

// FinishRequest represents the request body for finish RPC.
// @Description Request body for finishing a recording
type FinishRequest struct {
	// Task ID for the recording session
	// @example task-001
	TaskID string `json:"task_id"`
}

// RegisterRoutes registers REST routes for Axon Recorder RPC.
func (h *RecorderHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/devices", h.ListDevices)
	apiV1.GET("/:device_id/state", h.GetState)
	apiV1.GET("/:device_id/stats", h.GetStats)
	apiV1.POST("/:device_id/config", h.Config)
	apiV1.POST("/:device_id/begin", h.Begin)
	apiV1.POST("/:device_id/finish", h.Finish)
	apiV1.POST("/:device_id/pause", h.Pause)
	apiV1.POST("/:device_id/resume", h.Resume)
	apiV1.POST("/:device_id/cancel", h.Cancel)
	apiV1.POST("/:device_id/clear", h.Clear)
	apiV1.POST("/:device_id/quit", h.Quit)
}

// HandleWebSocket handles recorder WebSocket connections.
func (h *RecorderHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request, deviceID string) {
	if deviceID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

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
			logger.Printf("[RECORDER] Device %s: DB query error: %v", deviceID, err)
		}
		// Check count regardless of DB error (count defaults to 0 on error)
		if count == 0 {
			logger.Printf("[RECORDER] Device %s: robot not found in database", deviceID)
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	// Allow any origin in dev; tighten in production
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logger.Printf("[RECORDER] Device %s: WebSocket accept error: %v", deviceID, err)
		return
	}
	defer func() {
		if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
			logger.Printf("[RECORDER] Device %s: WebSocket close error: %v", deviceID, err)
		}
	}()

	ctx := r.Context()
	go h.pingLoop(ctx, conn)

	remoteIP := extractIP(r.RemoteAddr)
	rc := h.hub.NewRecorderConn(conn, deviceID, remoteIP)
	h.hub.Connect(deviceID, rc)
	defer h.hub.Disconnect(deviceID)
	defer revertRunnableTasksOnDeviceDisconnect(h.db, deviceID, nil, 0, false)

	// #nosec G706 -- Set aside for now
	logger.Printf("[RECORDER] Recorder %s connected from %s", deviceID, remoteIP)

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			logger.Printf("[RECORDER] Recorder %s disconnected: %v", deviceID, err)
			return
		}

		rc.LastSeenAt = time.Now()

		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			logger.Printf("[RECORDER] Recorder %s invalid JSON: %v", deviceID, err)
			continue
		}

		h.handleMessage(deviceID, rc, msg)
	}
}

// Config sends config RPC to the recorder.
//
// @Summary      Recorder config
// @Description  Sends config RPC to the Axon recorder (optional JSON body as params)
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path   string  true  "Recorder device ID"
// @Param        body       body   ConfigRequest  false  "Task configuration"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/config [post]
func (h *RecorderHandler) Config(c *gin.Context) {
	params, ok := h.bindOptionalParams(c)
	if !ok {
		return
	}

	var taskID string
	if params != nil {
		if tc, ok := params["task_config"].(map[string]interface{}); ok {
			taskID, _ = tc["task_id"].(string)
			taskID = strings.TrimSpace(taskID)
		}
	}

	if !h.callRPC(c, "config", params) {
		return
	}

	// If RPC succeeded (HTTP 200), advance task status: pending -> ready.
	// This is best-effort; failures should not change the RPC response.
	if taskID != "" && h.db != nil {
		now := time.Now().UTC()
		res, err := h.db.Exec(
			`UPDATE tasks
			 SET
			   status = 'ready',
			   ready_at = CASE WHEN ready_at IS NULL THEN ? ELSE ready_at END,
			   updated_at = ?
			 WHERE task_id = ? AND status = 'pending' AND deleted_at IS NULL`,
			now, now, taskID,
		)
		if err != nil {
			logger.Printf("[RECORDER] Device %s: failed to advance task pending->ready after config: task=%s err=%v", c.Param("device_id"), taskID, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			logger.Printf("[RECORDER] Device %s: task pending->ready skipped after config (not found or not pending): task=%s", c.Param("device_id"), taskID)
		}
	}
}

// Begin sends begin recording RPC to the recorder.
//
// @Summary      Begin recording
// @Description  Sends begin RPC to the Axon recorder (optional JSON body as params)
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path   string  true  "Recorder device ID"
// @Param        body       body   BeginRequest  false  "Task ID for the recording"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/begin [post]
func (h *RecorderHandler) Begin(c *gin.Context) {
	params, ok := h.bindOptionalParams(c)
	if !ok {
		return
	}

	var taskID string
	if params != nil {
		if v, ok := params["task_id"].(string); ok {
			taskID = strings.TrimSpace(v)
		}
	}

	if !h.callRPC(c, "begin", params) {
		return
	}

	// If RPC succeeded (HTTP 200), advance task status: ready -> in_progress.
	if taskID != "" && h.db != nil {
		now := time.Now().UTC()
		res, err := h.db.Exec(
			`UPDATE tasks
			 SET
			   status = 'in_progress',
			   started_at = CASE WHEN started_at IS NULL THEN ? ELSE started_at END,
			   updated_at = ?
			 WHERE task_id = ? AND status = 'ready' AND deleted_at IS NULL`,
			now, now, taskID,
		)
		if err != nil {
			logger.Printf("[RECORDER] Device %s: failed to advance task ready->in_progress after begin: task=%s err=%v", c.Param("device_id"), taskID, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			logger.Printf("[RECORDER] Device %s: task ready->in_progress skipped after begin (not found or not ready): task=%s", c.Param("device_id"), taskID)
		}
	}
}

// Finish sends finish recording RPC to the recorder.
//
// @Summary      Finish recording
// @Description  Sends finish RPC to the Axon recorder (optional JSON body as params)
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path   string  true  "Recorder device ID"
// @Param        body       body   FinishRequest  false  "Task ID to finish"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/finish [post]
func (h *RecorderHandler) Finish(c *gin.Context) {
	params, ok := h.bindOptionalParams(c)
	if !ok {
		return
	}
	h.callRPC(c, "finish", params)
}

// Pause sends pause RPC to the recorder.
//
// @Summary      Pause recording
// @Description  Sends pause RPC to the Axon recorder
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Param        body       body  object  false  "Empty body"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/pause [post]
func (h *RecorderHandler) Pause(c *gin.Context) {
	h.callRPC(c, "pause", nil)
}

// Resume sends resume RPC to the recorder.
//
// @Summary      Resume recording
// @Description  Sends resume RPC to the Axon recorder
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Param        body       body  object  false  "Empty body"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/resume [post]
func (h *RecorderHandler) Resume(c *gin.Context) {
	h.callRPC(c, "resume", nil)
}

// Cancel sends cancel RPC to the recorder.
//
// @Summary      Cancel recording
// @Description  Sends cancel RPC to the Axon recorder. If task_id is provided and the RPC succeeds, sets task status from ready or in_progress back to pending (other statuses are left unchanged). Does not advance batch status.
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Param        body       body  object  false  "Empty body"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/cancel [post]
func (h *RecorderHandler) Cancel(c *gin.Context) {
	params, ok := h.bindOptionalParams(c)
	if !ok {
		return
	}

	var taskID string
	if params != nil {
		if v, ok := params["task_id"].(string); ok {
			taskID = strings.TrimSpace(v)
		}
	}

	if !h.callRPC(c, "cancel", params) {
		return
	}

	// If RPC succeeded (HTTP 200), only when the task is ready or in_progress: revert to pending.
	// Best-effort: failures should not change the RPC response.
	if taskID != "" && h.db != nil {
		deviceID := c.Param("device_id")
		now := time.Now().UTC()
		res, err := h.db.Exec(
			`UPDATE tasks
			 SET
			   status = 'pending',
			   updated_at = ?
			 WHERE task_id = ? AND status IN ('ready', 'in_progress') AND deleted_at IS NULL`,
			now, taskID,
		)
		if err != nil {
			logger.Printf("[RECORDER] Device %s: failed to revert task after cancel RPC: task=%s err=%v", deviceID, taskID, err)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			logger.Printf("[RECORDER] Device %s: task revert skipped after cancel RPC (not found or not ready/in_progress): task=%s", deviceID, taskID)
		}
	}
}

// Clear sends clear RPC to the recorder.
//
// @Summary      Clear recorder
// @Description  Sends clear RPC to the Axon recorder. If task_id is provided and the RPC succeeds, it will revert task status from ready to pending.
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Param        body       body  object  false  "Optional body (supports task_id)"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/clear [post]
func (h *RecorderHandler) Clear(c *gin.Context) {
	params, ok := h.bindOptionalParams(c)
	if !ok {
		return
	}

	var taskID string
	if params != nil {
		if v, ok := params["task_id"].(string); ok {
			taskID = strings.TrimSpace(v)
		}
	}

	// Do not forward params to recorder; keep RPC payload stable.
	if !h.callRPC(c, "clear", nil) {
		return
	}

	// If RPC succeeded (HTTP 200), revert task status: ready -> pending.
	// Best-effort: failures should not change the RPC response.
	if taskID != "" && h.db != nil {
		now := time.Now().UTC()
		res, err := h.db.Exec(
			`UPDATE tasks
			 SET
			   status = 'pending',
			   updated_at = ?
			 WHERE task_id = ? AND status = 'ready' AND deleted_at IS NULL`,
			now, taskID,
		)
		if err != nil {
			logger.Printf("[RECORDER] Device %s: failed to revert task ready->pending after clear: task=%s err=%v", c.Param("device_id"), taskID, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			logger.Printf("[RECORDER] Device %s: task ready->pending skipped after clear (not found or not ready): task=%s", c.Param("device_id"), taskID)
		}
	}
}

// Quit sends quit RPC to the recorder.
//
// @Summary      Quit recorder
// @Description  Sends quit RPC to the Axon recorder
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Param        body       body  object  false  "Empty body"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      504  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/quit [post]
func (h *RecorderHandler) Quit(c *gin.Context) {
	h.callRPC(c, "quit", nil)
}

// GetStats requests recorder stats from the device.
//
// @Summary      Get recorder stats
// @Description  Sends get_stats RPC to the Axon recorder and returns the response
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Success      200  {object}  map[string]interface{}  "connected + data (optional error when device reports failure)"
// @Failure      504  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /recorder/{device_id}/stats [get]
func (h *RecorderHandler) GetStats(c *gin.Context) {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}
	if h.hub.Get(deviceID) == nil {
		c.JSON(http.StatusOK, disconnectedRecorderStatsResponse())
		return
	}

	response, err := h.hub.SendRPC(c.Request.Context(), deviceID, "get_stats", nil, time.Duration(h.cfg.ResponseTimeout)*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrRecorderNotConnected):
			c.JSON(http.StatusOK, disconnectedRecorderStatsResponse())
		case errors.Is(err, services.ErrRecorderRPCTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	out := gin.H{
		"connected": true,
		"data":      response.Data,
	}
	if !response.Success && strings.TrimSpace(response.Message) != "" {
		out["error"] = response.Message
	}
	c.JSON(http.StatusOK, out)
}

func disconnectedRecorderStatsResponse() gin.H {
	return gin.H{
		"connected": false,
		"data":      map[string]interface{}{},
	}
}

func (h *RecorderHandler) callRPC(c *gin.Context, action string, params map[string]interface{}) bool {
	deviceID := c.Param("device_id")
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return false
	}

	response, err := h.hub.SendRPC(c.Request.Context(), deviceID, action, params, time.Duration(h.cfg.ResponseTimeout)*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrRecorderNotConnected):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, services.ErrRecorderRPCTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return false
	}

	c.JSON(http.StatusOK, response)
	return true
}

func (h *RecorderHandler) bindOptionalParams(c *gin.Context) (map[string]interface{}, bool) {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil, true
	}

	var params map[string]interface{}
	if err := c.ShouldBindJSON(&params); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return nil, false
	}
	return params, true
}

// ListDevices returns all currently connected recorder devices.
//
// @Summary      List connected recorders
// @Description  Returns IDs of all Axon recorders currently connected via WebSocket
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "devices array"
// @Router       /recorder/devices [get]
func (h *RecorderHandler) ListDevices(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"devices": h.hub.ListDevices()})
}

// GetState returns the latest cached recorder state snapshot.
//
// @Summary      Get recorder state
// @Description  Returns the latest cached state snapshot for the given recorder device
// @Tags         recorder
// @Accept       json
// @Produce      json
// @Param        device_id  path  string  true  "Recorder device ID"
// @Success      200  {object}  map[string]interface{}  "connected=false when recorder WS is not active"
// @Router       /recorder/{device_id}/state [get]
func (h *RecorderHandler) GetState(c *gin.Context) {
	deviceID := c.Param("device_id")
	rc := h.hub.Get(deviceID)
	if rc == nil {
		c.JSON(http.StatusOK, gin.H{
			"connected":      false,
			"current_state":  "unknown",
			"previous_state": "",
			"task_id":        "",
			"updated_at":     time.Now().UTC(),
		})
		return
	}
	st := rc.GetState()
	prev := recorderPreviousStateFromRaw(st.Raw)
	c.JSON(http.StatusOK, gin.H{
		"connected":      true,
		"current_state":  st.CurrentState,
		"previous_state": prev,
		"task_id":        st.TaskID,
		"updated_at":     st.UpdatedAt,
	})
}

func recorderPreviousStateFromRaw(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	s, _ := raw["previous"].(string)
	return strings.TrimSpace(s)
}

func (h *RecorderHandler) handleMessage(deviceID string, rc *services.RecorderConn, msg map[string]interface{}) {
	msgType, _ := msg["type"].(string)
	switch msgType {
	case "rpc_response":
		h.handleRPCResponse(deviceID, msg)
	case "state_update":
		h.handleStateUpdate(rc, msg)
	case "connected":
		// #nosec G706 -- Set aside for now
		logger.Printf("[RECORDER] Recorder %s sent connected event", deviceID)
	default:
		// #nosec G706 -- Set aside for now
		logger.Printf("[RECORDER] Recorder %s unknown message type %q", deviceID, msgType)
	}
}

func (h *RecorderHandler) handleRPCResponse(deviceID string, msg map[string]interface{}) {
	response := &services.RPCResponse{
		Type:      stringValue(msg, "type"),
		RequestID: stringValue(msg, "request_id"),
		Success:   boolValue(msg, "success"),
		Message:   stringValue(msg, "message"),
		Data:      mapValue(msg, "data"),
	}
	if !h.hub.HandleRPCResponse(deviceID, response) {
		logger.Printf("[RECORDER] Recorder %s unmatched response request_id=%s", deviceID, response.RequestID)
	}
}

func (h *RecorderHandler) handleStateUpdate(rc *services.RecorderConn, msg map[string]interface{}) {
	data := mapValue(msg, "data")
	state := services.RecorderState{
		CurrentState: stringValue(data, "current"),
		TaskID:       stringValue(data, "task_id"),
		Raw:          data,
	}
	rc.UpdateState(state)
	// #nosec G706 -- Set aside for now
	logger.Printf("[RECORDER] Recorder %s state=%s task=%s", rc.DeviceID, state.CurrentState, state.TaskID)
}

func (h *RecorderHandler) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(time.Duration(h.cfg.PingInterval) * time.Second)
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
}

func stringValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func boolValue(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

func mapValue(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]interface{})
	return v
}
