// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/services"
)

// DeviceStateHandler streams recorder state and recorder/transfer connection events.
type DeviceStateHandler struct {
	broker      *services.DeviceStateBroker
	recorderHub *services.RecorderHub
	transferHub *services.TransferHub
}

// NewDeviceStateHandler creates a DeviceStateHandler.
func NewDeviceStateHandler(broker *services.DeviceStateBroker, recorderHub *services.RecorderHub, transferHub *services.TransferHub) *DeviceStateHandler {
	return &DeviceStateHandler{broker: broker, recorderHub: recorderHub, transferHub: transferHub}
}

// RegisterRoutes registers device state stream routes.
func (h *DeviceStateHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/device-state/stream", h.Stream)
}

// Stream emits Server-Sent Events for device state changes.
func (h *DeviceStateHandler) Stream(c *gin.Context) {
	if h == nil || h.broker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "device state stream is not configured"})
		return
	}

	deviceFilter := deviceStateStreamDeviceFilter(c)
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()

	events, unsubscribe := h.broker.Subscribe(64)
	defer unsubscribe()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !deviceStateEventMatchesFilter(ev, deviceFilter) {
				continue
			}
			if err := writeDeviceStateSSE(w, ev); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			w.Flush()
		}
	}
}

func writeDeviceStateSSE(w gin.ResponseWriter, ev services.DeviceStateEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	eventType := strings.TrimSpace(stringFromEvent(ev, "type"))
	if eventType == "" {
		eventType = "device_state"
	}
	if version, ok := ev["state_version"]; ok {
		if _, err := fmt.Fprintf(w, "id: %v\n", version); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func stringFromEvent(ev services.DeviceStateEvent, key string) string {
	if ev == nil {
		return ""
	}
	if s, ok := ev[key].(string); ok {
		return s
	}
	return ""
}

func deviceStateStreamDeviceFilter(c *gin.Context) map[string]struct{} {
	ids := c.QueryArray("device_id")
	if csv := strings.TrimSpace(c.Query("device_ids")); csv != "" {
		ids = append(ids, strings.Split(csv, ",")...)
	}
	filter := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			filter[id] = struct{}{}
		}
	}
	return filter
}

func deviceStateEventMatchesFilter(ev services.DeviceStateEvent, filter map[string]struct{}) bool {
	if len(filter) == 0 {
		return true
	}
	deviceID := strings.TrimSpace(stringFromEvent(ev, "device_id"))
	_, ok := filter[deviceID]
	return ok
}

func publishDeviceConnectionEvent(
	broker *services.DeviceStateBroker,
	recorderHub *services.RecorderHub,
	transferHub *services.TransferHub,
	deviceID string,
	source string,
) {
	deviceID = strings.TrimSpace(deviceID)
	if broker == nil || deviceID == "" {
		return
	}
	recorderConnected := recorderHub != nil && recorderHub.Get(deviceID) != nil
	transferConnected := transferHub != nil && transferHub.Get(deviceID) != nil
	broker.Publish(deviceID, services.DeviceStateEvent{
		"type":               "device_connection",
		"recorder_connected": recorderConnected,
		"transfer_connected": transferConnected,
		"connected":          recorderConnected && transferConnected,
		"source":             strings.TrimSpace(source),
	})
}
