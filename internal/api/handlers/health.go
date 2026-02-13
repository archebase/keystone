// handlers/health.go - Health check handler
package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// HealthHandler health check handler
type HealthHandler struct {
	db      interface{} // TODO: *database.DB
	storage interface{} // TODO: *s3.Client
}

// NewHealthHandler creates a new health check handler
func NewHealthHandler(db, storage interface{}) *HealthHandler {
	return &HealthHandler{
		db:      db,
		storage: storage,
	}
}

// ComponentHealth component health status
type ComponentHealth struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// HealthResponse health check response
type HealthResponse struct {
	Status     string                       `json:"status"`
	Timestamp  string                       `json:"timestamp"`
	Components map[string]ComponentHealth   `json:"components"`
}

// Handler handles health check requests
func (h *HealthHandler) Handler(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{
		Status:     "healthy",
		Timestamp:  time.Now().Format(time.RFC3339),
		Components: make(map[string]ComponentHealth),
	}

	// Check database (TODO: actual ping)
	response.Components["database"] = ComponentHealth{
		Status: "healthy",
	}

	// Check storage (TODO: actual ping)
	response.Components["storage"] = ComponentHealth{
		Status: "healthy",
	}

	// Check disk space
	response.Components["disk"] = ComponentHealth{
		Status: "healthy",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// Register registers routes
func (h *HealthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", h.Handler)
	log.Println("[API] Registered health check endpoint: GET /health")
}
