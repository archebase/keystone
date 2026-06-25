// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	hub          *services.RecorderHub
	transferHub  *services.TransferHub
	stateBroker  *services.DeviceStateBroker
	cfg          *config.RecorderConfig
	db           *sqlx.DB
	callbackURLs callbackURLs
}

// NewRecorderHandler creates a new RecorderHandler.
func NewRecorderHandler(hub *services.RecorderHub, cfg *config.RecorderConfig, db *sqlx.DB) *RecorderHandler {
	return &RecorderHandler{hub: hub, cfg: cfg, db: db}
}

// SetCallbackPublicBaseURL configures callback URLs sent in recorder task config RPCs.
func (h *RecorderHandler) SetCallbackPublicBaseURL(callbackPublicBaseURL string) {
	if h == nil {
		return
	}
	h.callbackURLs = newCallbackURLs(callbackPublicBaseURL)
}

// SetDeviceStateDeps enables device connection/state event publishing.
func (h *RecorderHandler) SetDeviceStateDeps(transferHub *services.TransferHub, broker *services.DeviceStateBroker) {
	if h == nil {
		return
	}
	h.transferHub = transferHub
	h.stateBroker = broker
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
	if !h.authorizeRecorderWebSocket(w, r, deviceID) {
		return
	}

	// Allow any origin in dev; tighten in production
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logger.Printf("%s WebSocket accept error: %v", recorderLogPrefix(deviceID), err)
		return
	}

	remoteIP := extractIP(r.RemoteAddr)
	rc := h.hub.NewRecorderConn(conn, deviceID, remoteIP)
	replacedConn := h.hub.ConnectReplacingExisting(deviceID, rc)
	closeReplacedRecorderConn(deviceID, replacedConn)
	publishDeviceConnectionEvent(h.stateBroker, h.hub, h.transferHub, deviceID, "recorder_connected")

	defer func() {
		if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
			if !isExpectedWebSocketCloseError(err) {
				logger.Printf("%s WebSocket close error: %v", recorderLogPrefix(deviceID), err)
			}
		}
	}()
	defer func() {
		if h.hub.Disconnect(deviceID, rc) {
			publishDeviceConnectionEvent(h.stateBroker, h.hub, h.transferHub, deviceID, "recorder_disconnected")
			revertRunnableTasksOnDeviceDisconnect(h.db, deviceID, nil, 0, false)
		}
	}()

	ctx := r.Context()
	go h.pingLoop(ctx, rc)

	// #nosec G706 -- Set aside for now
	logger.Printf("%s connected from %s", recorderLogPrefix(deviceID), remoteIP)
	go h.syncRecorderStateFromDevice(ctx, rc)

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if !isExpectedWebSocketCloseError(err) {
				logger.Printf("%s disconnected: %v", recorderLogPrefix(deviceID), err)
			}
			return
		}

		rc.LastSeenAt = time.Now()

		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			logger.Printf("%s invalid JSON: %v", recorderLogPrefix(deviceID), err)
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

	if !h.requireRecorderReadyForConfig(c) {
		return
	}

	if !h.requireTaskConfigurable(c, taskID) {
		return
	}

	h.overrideTaskConfigCallbackURLs(params)

	if !h.callRPC(c, "config", params) {
		return
	}

	advanceTaskPendingToReady(h.db, c.Param("device_id"), taskID, "config")
}

func (h *RecorderHandler) overrideTaskConfigCallbackURLs(params map[string]interface{}) {
	if h == nil || !h.callbackURLs.configured() || params == nil {
		return
	}
	taskConfig, ok := params["task_config"].(map[string]interface{})
	if !ok || taskConfig == nil {
		return
	}
	taskConfig["start_callback_url"] = h.callbackURLs.startURL()
	taskConfig["finish_callback_url"] = h.callbackURLs.finishURL()
}

func (h *RecorderHandler) requireTaskConfigurable(c *gin.Context, taskID string) bool {
	if h == nil || h.db == nil {
		return true
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "task_id_required",
			"error": "task_config.task_id is required",
		})
		return false
	}

	deviceID := strings.TrimSpace(c.Param("device_id"))
	status, ok, err := currentOwnedTaskStatus(c.Request.Context(), h.db, deviceID, taskID)
	if err != nil {
		logger.Printf("%s failed to check task configurability: err=%v", recorderTaskLogPrefix(deviceID, taskID), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check task status"})
		return false
	}
	if !ok || status != "pending" {
		c.JSON(http.StatusConflict, gin.H{
			"code":           "task_not_configurable",
			"error":          "task is not configurable",
			"current_status": taskStatusLogValue(status, "not_found"),
		})
		return false
	}
	return true
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

	if !h.requireTaskBeginable(c, taskID) {
		return
	}

	if !h.callRPC(c, "begin", params) {
		return
	}

	// If RPC succeeded (HTTP 200), advance task status: pending/ready -> in_progress.
	// pending is allowed because recorder may preserve ready state across a transient
	// WebSocket disconnect while Keystone has already rolled the task back.
	if taskID != "" && h.db != nil {
		rowsAffected, _, err := advanceTaskPendingOrReadyToInProgress(h.db, taskID)
		if err != nil {
			logger.Printf("%s failed to advance task pending/ready->in_progress after begin: err=%v", recorderTaskLogPrefix(c.Param("device_id"), taskID), err)
			return
		}
		if rowsAffected == 0 {
			h.logBeginTransitionNoop(c.Param("device_id"), taskID)
		}
	}
}

func (h *RecorderHandler) requireTaskBeginable(c *gin.Context, taskID string) bool {
	if h == nil || h.db == nil {
		return true
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "task_id_required",
			"error": "task_id is required",
		})
		return false
	}

	deviceID := strings.TrimSpace(c.Param("device_id"))
	status, ok, err := currentOwnedTaskStatus(c.Request.Context(), h.db, deviceID, taskID)
	if err != nil {
		logger.Printf("%s failed to check task beginability: err=%v", recorderTaskLogPrefix(deviceID, taskID), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check task status"})
		return false
	}
	if !ok || (status != "pending" && status != "ready") {
		c.JSON(http.StatusConflict, gin.H{
			"code":           "task_not_beginable",
			"error":          "task is not beginable",
			"current_status": taskStatusLogValue(status, "not_found"),
		})
		return false
	}

	if h.hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "recorder hub is not configured"})
		return false
	}
	rc := h.hub.Get(deviceID)
	if rc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": services.ErrRecorderNotConnected.Error()})
		return false
	}
	if !rc.IsStateSynced() {
		c.JSON(http.StatusConflict, gin.H{
			"code":  "recorder_state_syncing",
			"error": "recorder state is syncing; retry after initial state snapshot",
		})
		return false
	}

	state := rc.GetState()
	if recorderStateAge(state) > defaultRecorderFreshMaxAge {
		refreshed, _, err := h.refreshRecorderState(c.Request.Context(), deviceID, rc, -1)
		if err != nil {
			statusCode := http.StatusConflict
			if errors.Is(err, services.ErrRecorderRPCTimeout) {
				statusCode = http.StatusGatewayTimeout
			}
			out := h.recorderStateResponse(deviceID, rc, false)
			out["code"] = "recorder_state_refresh_failed"
			out["error"] = err.Error()
			c.JSON(statusCode, out)
			return false
		}
		state = refreshed
	}

	current := strings.ToLower(strings.TrimSpace(state.CurrentState))
	if current != "ready" || strings.TrimSpace(state.TaskID) != taskID {
		c.JSON(http.StatusConflict, gin.H{
			"code":             "task_not_beginable",
			"error":            "recorder is not ready for task",
			"current_status":   status,
			"recorder_state":   state.CurrentState,
			"recorder_task_id": state.TaskID,
		})
		return false
	}
	return true
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
	params = h.fillRecorderTaskIDFromCache(c.Param("device_id"), params)
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
			logger.Printf("%s failed to revert task after cancel RPC: err=%v", recorderTaskLogPrefix(deviceID, taskID), err)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			logger.Printf("%s task revert skipped after cancel RPC (not found or not ready/in_progress)", recorderTaskLogPrefix(deviceID, taskID))
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
			logger.Printf("%s failed to revert task ready->pending after clear: err=%v", recorderTaskLogPrefix(c.Param("device_id"), taskID), err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			logger.Printf("%s task ready->pending skipped after clear (not found or not ready)", recorderTaskLogPrefix(c.Param("device_id"), taskID))
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

	timeout := recorderRPCResponseTimeout(h.cfg)
	response, err := h.hub.SendRPC(c.Request.Context(), deviceID, "get_stats", nil, timeout)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrRecorderNotConnected):
			c.JSON(http.StatusOK, disconnectedRecorderStatsResponse())
		case errors.Is(err, services.ErrRecorderRPCTimeout):
			logRecorderRPCTimeout(deviceID, "get_stats", "", "stats", timeout, err)
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

const defaultRecorderFreshMaxAge = time.Second

func isTruthyQuery(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}

func recorderRefreshMaxAge(c *gin.Context) time.Duration {
	value := strings.TrimSpace(c.Query("max_age_ms"))
	if value == "" {
		return defaultRecorderFreshMaxAge
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms < 0 {
		return defaultRecorderFreshMaxAge
	}
	return time.Duration(ms) * time.Millisecond
}

func recorderStateAge(state services.RecorderState) time.Duration {
	if state.UpdatedAt.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	age := time.Since(state.UpdatedAt)
	if age < 0 {
		return 0
	}
	return age
}

func recorderStateAgeMS(state services.RecorderState) int64 {
	age := recorderStateAge(state)
	if age == time.Duration(1<<63-1) {
		return 0
	}
	return age.Milliseconds()
}

func (h *RecorderHandler) disconnectedRecorderStateResponse(deviceID string) gin.H {
	return gin.H{
		"connected":      false,
		"current_state":  "unknown",
		"previous_state": "",
		"task_id":        "",
		"updated_at":     time.Now().UTC(),
		"state_synced":   false,
		"syncing":        false,
		"fresh":          false,
		"age_ms":         0,
		"source":         "disconnected",
		"state_version":  h.recorderStateVersion(deviceID),
	}
}

func (h *RecorderHandler) recorderStateResponse(deviceID string, rc *services.RecorderConn, fresh bool) gin.H {
	st := rc.GetState()
	synced := rc.IsStateSynced()
	return gin.H{
		"connected":      true,
		"current_state":  st.CurrentState,
		"previous_state": recorderPreviousStateFromRaw(st.Raw),
		"task_id":        st.TaskID,
		"updated_at":     st.UpdatedAt,
		"state_synced":   synced,
		"syncing":        !synced,
		"fresh":          fresh,
		"age_ms":         recorderStateAgeMS(st),
		"source":         strings.TrimSpace(st.Source),
		"state_version":  h.recorderStateVersion(deviceID),
	}
}

func (h *RecorderHandler) recorderStateVersion(deviceID string) uint64 {
	if h == nil || h.stateBroker == nil {
		return 0
	}
	return h.stateBroker.CurrentVersion(strings.TrimSpace(deviceID))
}

func (h *RecorderHandler) refreshRecorderState(ctx context.Context, deviceID string, rc *services.RecorderConn, maxAge time.Duration) (services.RecorderState, bool, error) {
	if h == nil || h.hub == nil || rc == nil || strings.TrimSpace(deviceID) == "" {
		return services.RecorderState{}, false, services.ErrRecorderNotConnected
	}
	if h.hub.Get(deviceID) != rc {
		return services.RecorderState{}, false, services.ErrRecorderNotConnected
	}

	state := rc.GetState()
	if maxAge >= 0 && rc.IsStateSynced() && recorderStateAge(state) <= maxAge {
		return state, true, nil
	}

	timeout := recorderRPCResponseTimeout(h.cfg)
	response, err := h.hub.SendRPC(ctx, deviceID, "get_state", nil, timeout)
	if err != nil {
		if errors.Is(err, services.ErrRecorderRPCTimeout) {
			logRecorderRPCTimeout(deviceID, "get_state", "", "state_refresh", timeout, err)
		}
		h.markRecorderSyncing(rc, "get_state", err)
		return rc.GetState(), false, err
	}
	if response == nil || !response.Success {
		message := "recorder get_state returned unsuccessful response"
		if response != nil && strings.TrimSpace(response.Message) != "" {
			message = strings.TrimSpace(response.Message)
		}
		err := errors.New(message)
		h.markRecorderSyncing(rc, "get_state", err)
		return rc.GetState(), false, err
	}
	if h.hub.Get(deviceID) != rc {
		return services.RecorderState{}, false, services.ErrRecorderNotConnected
	}

	state = recorderStateFromRPCData(response.Data)
	if strings.TrimSpace(state.CurrentState) == "" {
		err := errors.New("recorder get_state returned empty state")
		h.markRecorderSyncing(rc, "get_state", err)
		return rc.GetState(), false, err
	}
	if err := h.applyRecorderStateSnapshot(rc, state, "get_state"); err != nil {
		return rc.GetState(), false, err
	}
	return rc.GetState(), true, nil
}

func (h *RecorderHandler) markRecorderSyncing(rc *services.RecorderConn, source string, syncErr error) {
	if h == nil || h.hub == nil || rc == nil || h.hub.Get(rc.DeviceID) != rc {
		return
	}
	rc.MarkStateSyncing(source, syncErr)
	st := rc.GetState()
	if h.stateBroker != nil {
		ev := recorderStateEvent(st, "recorder_syncing", source, false)
		ev["state_synced"] = false
		ev["syncing"] = true
		if syncErr != nil {
			ev["error"] = syncErr.Error()
		}
		h.stateBroker.Publish(rc.DeviceID, ev)
	}
}

func (h *RecorderHandler) publishRecorderStateSnapshot(rc *services.RecorderConn, state services.RecorderState, source string, fresh bool) {
	if h == nil || h.stateBroker == nil || rc == nil {
		return
	}
	h.stateBroker.Publish(rc.DeviceID, recorderStateEvent(state, "recorder_state", source, fresh))
}

func recorderStateEvent(state services.RecorderState, eventType string, source string, fresh bool) services.DeviceStateEvent {
	synced := strings.TrimSpace(state.CurrentState) != ""
	return services.DeviceStateEvent{
		"type":           eventType,
		"current_state":  state.CurrentState,
		"previous_state": recorderPreviousStateFromRaw(state.Raw),
		"task_id":        state.TaskID,
		"state_synced":   synced,
		"syncing":        !synced,
		"fresh":          fresh,
		"age_ms":         recorderStateAgeMS(state),
		"source":         strings.TrimSpace(source),
	}
}

func (h *RecorderHandler) handleRecorderRPCTimeout(ctx context.Context, deviceID string, action string, params map[string]interface{}, timeout time.Duration, timeoutErr error) {
	taskID := firstNonEmptyString(params, "task_id")
	logRecorderRPCTimeout(deviceID, action, taskID, "api_rpc", timeout, timeoutErr)
	if rc := h.hub.Get(deviceID); rc != nil {
		h.markRecorderSyncing(rc, "rpc_timeout:"+action, timeoutErr)
		syncTimeout := timeout
		if syncTimeout <= 0 {
			syncTimeout = recorderRPCResponseTimeout(h.cfg)
		}
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
		go func() {
			defer cancel()
			h.syncRecorderStateFromDevice(syncCtx, rc)
		}()
	}
	if h.stateBroker == nil {
		return
	}
	h.stateBroker.Publish(deviceID, services.DeviceStateEvent{
		"type":           "recorder_operation_unknown",
		"action":         strings.TrimSpace(action),
		"task_id":        taskID,
		"result_unknown": true,
		"source":         "rpc_timeout:" + strings.TrimSpace(action),
		"error":          timeoutErr.Error(),
		"timeout":        timeoutLogValue(timeout),
		"timeout_ms":     timeoutLogMilliseconds(timeout),
	})
}

func (h *RecorderHandler) requireRecorderReadyForConfig(c *gin.Context) bool {
	deviceID := strings.TrimSpace(c.Param("device_id"))
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return false
	}
	if h == nil || h.hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "recorder hub is not configured"})
		return false
	}

	if h.transferHub != nil && h.transferHub.Get(deviceID) == nil {
		c.JSON(http.StatusConflict, gin.H{
			"code":  "transfer_not_connected",
			"error": "transfer is not connected; config is rejected",
		})
		return false
	}

	rc := h.hub.Get(deviceID)
	if rc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": services.ErrRecorderNotConnected.Error()})
		return false
	}
	if !rc.IsStateSynced() {
		c.JSON(http.StatusConflict, gin.H{
			"code":  "recorder_state_syncing",
			"error": "recorder state is syncing; retry after initial state snapshot",
		})
		return false
	}

	state := rc.GetState()
	if recorderStateAge(state) > defaultRecorderFreshMaxAge {
		refreshed, _, err := h.refreshRecorderState(c.Request.Context(), deviceID, rc, -1)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, services.ErrRecorderRPCTimeout) {
				status = http.StatusGatewayTimeout
			}
			out := h.recorderStateResponse(deviceID, rc, false)
			out["code"] = "recorder_state_refresh_failed"
			out["error"] = err.Error()
			c.JSON(status, out)
			return false
		}
		state = refreshed
	}

	current := strings.ToLower(strings.TrimSpace(state.CurrentState))
	if current != "idle" {
		c.JSON(http.StatusConflict, gin.H{
			"code":          "recorder_busy",
			"error":         "recorder is not idle; config is rejected",
			"current_state": state.CurrentState,
			"task_id":       state.TaskID,
		})
		return false
	}
	return true
}

func (h *RecorderHandler) callRPC(c *gin.Context, action string, params map[string]interface{}) bool {
	deviceID := strings.TrimSpace(c.Param("device_id"))
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return false
	}

	timeout := recorderRPCResponseTimeout(h.cfg)
	response, err := h.hub.SendRPC(c.Request.Context(), deviceID, action, params, timeout)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrRecorderNotConnected):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, services.ErrRecorderRPCTimeout):
			h.handleRecorderRPCTimeout(c.Request.Context(), deviceID, action, params, timeout, err)
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error":          err.Error(),
				"action":         action,
				"result_unknown": true,
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return false
	}

	if response != nil && response.Success {
		state := recorderStateFromRPCData(response.Data)
		if strings.TrimSpace(state.TaskID) == "" && recorderStateKeepsTaskID(state.CurrentState) {
			state.TaskID = recorderTaskIDFromRPCParams(params)
		}
		if strings.TrimSpace(state.CurrentState) != "" {
			if rc := h.hub.Get(deviceID); rc != nil {
				_ = h.applyRecorderStateSnapshot(rc, state, "rpc_response:"+action)
			}
		}
	}

	c.JSON(http.StatusOK, response)
	return response != nil && response.Success
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

func recorderTaskIDFromRPCParams(params map[string]interface{}) string {
	taskID := firstNonEmptyString(params, "task_id")
	if taskID != "" {
		return taskID
	}
	return stringValue(mapValue(params, "task_config"), "task_id")
}

func (h *RecorderHandler) fillRecorderTaskIDFromCache(deviceID string, params map[string]interface{}) map[string]interface{} {
	if recorderTaskIDFromRPCParams(params) != "" || h == nil || h.hub == nil {
		return params
	}
	rc := h.hub.Get(strings.TrimSpace(deviceID))
	if rc == nil {
		return params
	}
	taskID := strings.TrimSpace(rc.GetState().TaskID)
	if taskID == "" {
		return params
	}
	if params == nil {
		params = map[string]interface{}{}
	}
	params["task_id"] = taskID
	return params
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
	deviceID := strings.TrimSpace(c.Param("device_id"))
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}
	if h == nil || h.hub == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "recorder hub is not configured"})
		return
	}

	rc := h.hub.Get(deviceID)
	if rc == nil {
		c.JSON(http.StatusOK, h.disconnectedRecorderStateResponse(deviceID))
		return
	}

	fresh := false
	if isTruthyQuery(c.Query("refresh")) {
		var err error
		_, fresh, err = h.refreshRecorderState(c.Request.Context(), deviceID, rc, recorderRefreshMaxAge(c))
		if err != nil {
			if errors.Is(err, services.ErrRecorderNotConnected) {
				c.JSON(http.StatusOK, h.disconnectedRecorderStateResponse(deviceID))
				return
			}
			status := http.StatusConflict
			if errors.Is(err, services.ErrRecorderRPCTimeout) {
				status = http.StatusGatewayTimeout
			}
			out := h.recorderStateResponse(deviceID, rc, false)
			out["error"] = err.Error()
			if errors.Is(err, services.ErrRecorderRPCTimeout) {
				out["result_unknown"] = true
			}
			c.JSON(status, out)
			return
		}
	}

	c.JSON(http.StatusOK, h.recorderStateResponse(deviceID, rc, fresh))
}

func recorderPreviousStateFromRaw(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	s, _ := raw["previous"].(string)
	return strings.TrimSpace(s)
}

func (h *RecorderHandler) handleMessage(deviceID string, rc *services.RecorderConn, msg map[string]interface{}) {
	if h.hub.Get(deviceID) != rc {
		logger.Printf("%s ignored message from replaced connection", recorderLogPrefix(deviceID))
		return
	}

	msgType, _ := msg["type"].(string)
	switch msgType {
	case "rpc_response":
		h.handleRPCResponse(deviceID, msg)
	case "state_update":
		h.handleStateUpdate(rc, msg)
	case "connected":
		// #nosec G706 -- Set aside for now
		logger.Printf("%s sent connected event", recorderLogPrefix(deviceID))
	case "config_applied":
		data := mapValue(msg, "data")
		taskID := stringValue(data, "task_id")
		// #nosec G706 -- Set aside for now
		logger.Printf("%s config applied", recorderTaskLogPrefix(deviceID, taskID))
		advanceTaskPendingToReady(h.db, deviceID, taskID, "config_applied")
	default:
		// #nosec G706 -- Set aside for now
		logger.Printf("%s unknown message type %q", recorderLogPrefix(deviceID), msgType)
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
		logger.Printf("%s unmatched response request_id=%s", recorderLogPrefix(deviceID), response.RequestID)
	}
}

func (h *RecorderHandler) handleStateUpdate(rc *services.RecorderConn, msg map[string]interface{}) {
	data := mapValue(msg, "data")
	state := services.RecorderState{
		CurrentState: stringValue(data, "current"),
		TaskID:       stringValue(data, "task_id"),
		Raw:          data,
	}
	_ = h.applyRecorderStateSnapshot(rc, state, "state_update")
}

func (h *RecorderHandler) syncRecorderStateFromDevice(ctx context.Context, rc *services.RecorderConn) {
	if h == nil || h.hub == nil || rc == nil || strings.TrimSpace(rc.DeviceID) == "" {
		return
	}
	if h.hub.Get(rc.DeviceID) != rc {
		return
	}

	if _, _, err := h.refreshRecorderState(ctx, rc.DeviceID, rc, -1); err != nil {
		if !errors.Is(err, services.ErrRecorderNotConnected) && !errors.Is(err, context.Canceled) {
			logger.Printf("%s get_state after connect failed: %v", recorderLogPrefix(rc.DeviceID), err)
		}
	}
}

func (h *RecorderHandler) applyRecorderStateSnapshot(rc *services.RecorderConn, state services.RecorderState, source string) error {
	if rc == nil {
		return nil
	}
	previous := rc.GetState()
	state.Source = strings.TrimSpace(source)
	state.TaskID = strings.TrimSpace(state.TaskID)
	if !recorderStateKeepsTaskID(state.CurrentState) {
		state.TaskID = ""
	}
	if state.TaskID == "" && recorderStateCanInheritTaskID(state.CurrentState, state.Source) {
		state.TaskID = strings.TrimSpace(rc.GetState().TaskID)
	}
	if state.TaskID == "" && recorderStateRequiresTaskID(state.CurrentState, state.Source) {
		err := fmt.Errorf("recorder %s snapshot missing task_id for state=%s", state.Source, strings.TrimSpace(state.CurrentState))
		h.markRecorderSyncing(rc, state.Source, err)
		logger.Printf("%s ignored %s state=%s without task_id", recorderLogPrefix(rc.DeviceID), state.Source, state.CurrentState)
		return err
	}
	rc.UpdateState(state)
	st := rc.GetState()
	h.reconcileRecorderTaskState(rc.DeviceID, st, source)
	h.publishRecorderStateSnapshot(rc, st, source, true)
	if recorderStateSnapshotChanged(previous, st) {
		logRecorderStateChange(rc.DeviceID, previous, st, source)
	}
	return nil
}

func recorderStateSnapshotChanged(previous services.RecorderState, next services.RecorderState) bool {
	return !strings.EqualFold(strings.TrimSpace(previous.CurrentState), strings.TrimSpace(next.CurrentState)) ||
		strings.TrimSpace(previous.TaskID) != strings.TrimSpace(next.TaskID)
}

func logRecorderStateChange(deviceID string, previous services.RecorderState, next services.RecorderState, source string) {
	prefix := recorderStateLogPrefix(deviceID, previous.TaskID, next.TaskID)
	previousState := recorderLogState(previous.CurrentState)
	nextState := recorderLogState(next.CurrentState)
	logSource := recorderLogSource(source)
	if !strings.EqualFold(strings.TrimSpace(previous.CurrentState), strings.TrimSpace(next.CurrentState)) {
		logger.Printf("%s state changed: %s -> %s source=%s", prefix, previousState, nextState, logSource)
		return
	}
	logger.Printf("%s task changed: %s -> %s state=%s source=%s",
		prefix,
		recorderLogTaskID(previous.TaskID),
		recorderLogTaskID(next.TaskID),
		nextState,
		logSource,
	)
}

func recorderLogState(state string) string {
	state = strings.TrimSpace(state)
	if state == "" {
		return "-"
	}
	return state
}

func recorderLogTaskID(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "-"
	}
	return taskID
}

func recorderLogSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "state_update"
	}
	return source
}

func recorderLogPrefix(deviceID string) string {
	return logPrefixWithTask("RECORDER", deviceID, "")
}

func recorderTaskLogPrefix(deviceID, taskID string) string {
	return logPrefixWithTask("RECORDER", deviceID, taskID)
}

func recorderStateLogPrefix(deviceID, previousTaskID, nextTaskID string) string {
	taskID := strings.TrimSpace(nextTaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(previousTaskID)
	}
	return recorderTaskLogPrefix(deviceID, taskID)
}

func transferLogPrefix(deviceID string) string {
	return logPrefixWithTask("TRANSFER", deviceID, "")
}

func transferTaskLogPrefix(deviceID, taskID string) string {
	return logPrefixWithTask("TRANSFER", deviceID, taskID)
}

func deviceLogPrefix(deviceID string) string {
	return logPrefixWithTask("DEVICE", deviceID, "")
}

func deviceTaskLogPrefix(deviceID, taskID string) string {
	return logPrefixWithTask("DEVICE", deviceID, taskID)
}

func logPrefixWithTask(component, deviceID, taskID string) string {
	component = strings.TrimSpace(component)
	if component == "" {
		component = "DEVICE"
	}
	deviceID = strings.TrimSpace(deviceID)
	taskID = strings.TrimSpace(taskID)
	prefix := "[" + component + "]"
	if deviceID != "" {
		prefix += "[" + deviceID + "]"
	}
	if taskID != "" {
		prefix += "[" + taskID + "]"
	}
	return prefix
}

func recorderStateFromRPCData(data map[string]interface{}) services.RecorderState {
	state := services.RecorderState{
		CurrentState: firstNonEmptyString(data, "state", "current", "current_state"),
		TaskID:       firstNonEmptyString(data, "task_id"),
		Raw:          data,
	}
	if strings.TrimSpace(state.TaskID) == "" {
		state.TaskID = stringValue(mapValue(data, "task_config"), "task_id")
	}
	return state
}

func recorderStateKeepsTaskID(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ready", "recording", "paused":
		return true
	default:
		return false
	}
}

func recorderStateCanInheritTaskID(state string, source string) bool {
	switch strings.TrimSpace(source) {
	case "rpc_response:pause":
		return strings.EqualFold(strings.TrimSpace(state), "paused")
	case "rpc_response:resume":
		return strings.EqualFold(strings.TrimSpace(state), "recording")
	default:
		return false
	}
}

func recorderStateRequiresTaskID(state string, source string) bool {
	switch strings.TrimSpace(source) {
	case "get_state", "state_update":
		return recorderStateKeepsTaskID(state)
	default:
		return false
	}
}

func firstNonEmptyString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(m, key)); value != "" {
			return value
		}
	}
	return ""
}

func (h *RecorderHandler) reconcileRecorderTaskState(deviceID string, state services.RecorderState, source string) {
	if h.db == nil {
		return
	}
	taskID := strings.TrimSpace(state.TaskID)
	if taskID == "" {
		return
	}

	if strings.TrimSpace(source) == "" {
		source = "state_update"
	}
	currentState := strings.ToLower(strings.TrimSpace(state.CurrentState))
	switch currentState {
	case "ready":
		advanceTaskPendingToReady(h.db, deviceID, taskID, source+"_ready")
	case "recording", "paused":
		rowsAffected, previousStatus, err := advanceTaskPendingOrReadyToInProgress(h.db, taskID)
		if err != nil {
			logger.Printf("%s failed to advance task pending/ready->in_progress after %s %s: err=%v", recorderTaskLogPrefix(deviceID, taskID), source, currentState, err)
			return
		}
		if rowsAffected > 0 {
			logger.Printf("%s task status updated: %s -> in_progress reason=%s_%s", recorderTaskLogPrefix(deviceID, taskID), taskStatusLogValue(previousStatus, "unknown"), source, currentState)
		}
	}
}

func advanceTaskPendingToReady(db *sqlx.DB, deviceID, taskID, source string) {
	taskID = strings.TrimSpace(taskID)
	if db == nil || taskID == "" {
		return
	}
	previousStatus, _, _ := currentTaskStatus(db, taskID)
	now := time.Now().UTC()
	res, err := db.Exec(
		`UPDATE tasks
		 SET
		   status = 'ready',
		   ready_at = CASE WHEN ready_at IS NULL THEN ? ELSE ready_at END,
		   updated_at = ?
		 WHERE task_id = ? AND status = 'pending' AND deleted_at IS NULL`,
		now, now, taskID,
	)
	if err != nil {
		logger.Printf("%s failed to advance task pending->ready after %s: err=%v", recorderTaskLogPrefix(deviceID, taskID), source, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.Printf("%s task status updated: %s -> ready reason=%s", recorderTaskLogPrefix(deviceID, taskID), taskStatusLogValue(previousStatus, "unknown"), source)
	}
}

func advanceTaskPendingOrReadyToInProgress(db *sqlx.DB, taskID string) (int64, string, error) {
	if db == nil {
		return 0, "", nil
	}
	taskID = strings.TrimSpace(taskID)
	previousStatus, _, _ := currentTaskStatus(db, taskID)
	now := time.Now().UTC()
	res, err := db.Exec(
		`UPDATE tasks
		 SET
		   status = 'in_progress',
		   started_at = CASE WHEN started_at IS NULL THEN ? ELSE started_at END,
		   updated_at = ?
		 WHERE task_id = ? AND status IN ('pending', 'ready') AND deleted_at IS NULL`,
		now, now, taskID,
	)
	if err != nil {
		return 0, previousStatus, err
	}
	rowsAffected, _ := res.RowsAffected()
	return rowsAffected, previousStatus, nil
}

func (h *RecorderHandler) logBeginTransitionNoop(deviceID, taskID string) {
	status, ok, err := currentTaskStatus(h.db, taskID)
	if err != nil {
		logger.Printf("%s task status lookup failed after begin: err=%v", recorderTaskLogPrefix(deviceID, taskID), err)
		return
	}
	if ok && (status == "in_progress" || status == "completed") {
		return
	}
	if !ok {
		logger.Printf("%s task pending/ready->in_progress skipped after begin (task not found)", recorderTaskLogPrefix(deviceID, taskID))
		return
	}
	logger.Printf("%s task pending/ready->in_progress skipped after begin (current_status=%s)", recorderTaskLogPrefix(deviceID, taskID), status)
}

func currentTaskStatus(db *sqlx.DB, taskID string) (string, bool, error) {
	if db == nil {
		return "", false, nil
	}
	var status string
	err := db.Get(&status, `SELECT status FROM tasks WHERE task_id = ? AND deleted_at IS NULL`, strings.TrimSpace(taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return status, true, nil
}

func (h *RecorderHandler) pingLoop(ctx context.Context, rc *services.RecorderConn) {
	interval := recorderPingInterval(h.cfg)
	if interval <= 0 || rc == nil || rc.Conn == nil {
		return
	}
	timeout := recorderPingTimeout(h.cfg)
	if timeout <= 0 {
		timeout = interval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := rc.Conn.Ping(pingCtx)
			timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(pingCtx.Err(), context.DeadlineExceeded)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					logWebSocketPingFailure("RECORDER", rc.DeviceID, timeout, timedOut, err)
					if closeErr := rc.Conn.CloseNow(); closeErr != nil {
						if !isExpectedWebSocketCloseError(closeErr) {
							logger.Printf("%s close after ping failure: %v", recorderLogPrefix(rc.DeviceID), closeErr)
						}
					}
				}
				return
			}
			rc.LastSeenAt = time.Now()
		case <-ctx.Done():
			return
		}
	}
}

func recorderRPCResponseTimeout(cfg *config.RecorderConfig) time.Duration {
	if cfg == nil || cfg.ResponseTimeout <= 0 {
		return 15 * time.Second
	}
	return time.Duration(cfg.ResponseTimeout) * time.Second
}

func timeoutLogValue(timeout time.Duration) string {
	if timeout <= 0 {
		return "unknown"
	}
	return timeout.String()
}

func timeoutLogMilliseconds(timeout time.Duration) int64 {
	if timeout <= 0 {
		return 0
	}
	return timeout.Milliseconds()
}

func logRecorderRPCTimeout(deviceID, action, taskID, source string, timeout time.Duration, err error) {
	deviceID = strings.TrimSpace(deviceID)
	action = strings.TrimSpace(action)
	if action == "" {
		action = "unknown"
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	taskID = strings.TrimSpace(taskID)
	if taskID != "" {
		logger.Printf("%s RPC timeout after %s (timeout_ms=%d): action=%s source=%s err=%v", recorderTaskLogPrefix(deviceID, taskID), timeoutLogValue(timeout), timeoutLogMilliseconds(timeout), action, source, err)
		return
	}
	logger.Printf("%s RPC timeout after %s (timeout_ms=%d): action=%s source=%s err=%v", recorderLogPrefix(deviceID), timeoutLogValue(timeout), timeoutLogMilliseconds(timeout), action, source, err)
}

func logWebSocketPingFailure(component, deviceID string, timeout time.Duration, timedOut bool, err error) {
	component = strings.TrimSpace(component)
	deviceID = strings.TrimSpace(deviceID)
	prefix := logPrefixWithTask(component, deviceID, "")
	if timedOut {
		logger.Printf("%s ping timeout after %s (timeout_ms=%d): %v", prefix, timeoutLogValue(timeout), timeoutLogMilliseconds(timeout), err)
		return
	}
	logger.Printf("%s ping failed (timeout=%s timeout_ms=%d): %v", prefix, timeoutLogValue(timeout), timeoutLogMilliseconds(timeout), err)
}

func recorderPingInterval(cfg *config.RecorderConfig) time.Duration {
	if cfg == nil || cfg.PingInterval <= 0 {
		return 0
	}
	return time.Duration(cfg.PingInterval) * time.Second
}

func recorderPingTimeout(cfg *config.RecorderConfig) time.Duration {
	if cfg == nil || cfg.PingTimeout <= 0 {
		return 0
	}
	return time.Duration(cfg.PingTimeout) * time.Second
}

func closeReplacedRecorderConn(deviceID string, rc *services.RecorderConn) {
	if rc == nil || rc.Conn == nil {
		return
	}
	logger.Printf("%s closing replaced WebSocket connection", recorderLogPrefix(deviceID))
	if err := rc.Conn.CloseNow(); err != nil {
		if !isExpectedWebSocketCloseError(err) {
			logger.Printf("%s replaced WebSocket close error: %v", recorderLogPrefix(deviceID), err)
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
