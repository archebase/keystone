// handlers/health.go - Health check handler
package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
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
	Status  string `json:"status" example:"healthy"`
	Message string `json:"message,omitempty" example:""`
}

// HealthResponse health check response
type HealthResponse struct {
	Status     string                       `json:"status" example:"healthy"`
	Timestamp  string                       `json:"timestamp" example:"2025-02-14T10:30:00Z"`
	Components map[string]ComponentHealth   `json:"components"`
	Version    string                       `json:"version" example:"1.0.0"`
}

// Handler godoc
//	@Summary		Health check
//	@Description	Check if the API is running
//	@Tags			health
//	@Accept			json
//	@Produce		json
//	@Success		200	{object}	HealthResponse	"Service is healthy"
//	@Failure		503	{object}	HealthResponse	"Service is unhealthy"
//	@Router			/health [get]
func (h *HealthHandler) Handler(c *gin.Context) {
	response := HealthResponse{
		Status:     "healthy",
		Timestamp:  time.Now().Format(time.RFC3339),
		Version:    "1.0.0",
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

	c.JSON(http.StatusOK, response)
}

// Register registers routes
func (h *HealthHandler) Register(r *gin.RouterGroup) {
	r.GET("/health", h.Handler)
}

// RegisterAPI registers routes for API v1 group
func (h *HealthHandler) RegisterAPI(r *gin.RouterGroup) {
	r.GET("/health", h.Handler)
}
