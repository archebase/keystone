// Package server provides HTTP server for Keystone Edge API
package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"archebase.com/keystone-edge/docs"
	"archebase.com/keystone-edge/internal/api/handlers"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/jmoiron/sqlx"
)

// Server represents the HTTP server
type Server struct {
	cfg        *config.Config
	health     *handlers.HealthHandler
	transfer   *handlers.TransferHandler
	episode    *handlers.EpisodeHandler
	task       *handlers.TaskHandler
	httpServer *http.Server
	wsServer   *http.Server
	shutdownMu sync.RWMutex
	isRunning  bool
	engine     *gin.Engine
}

// New creates a new server instance.
// db and s3Client are optional; pass nil to disable Verified ACK.
func New(cfg *config.Config, db *sqlx.DB, s3Client *s3.Client) *Server {
	// Create Gin engine
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(gin.Logger())

	// Create handlers
	healthHandler := handlers.NewHealthHandler(nil, nil)

	// Create TransferHub and TransferHandler for Fleet Manager
	hub := services.NewTransferHub(cfg.Fleet.MaxEvents)
	transferHandler := handlers.NewTransferHandler(hub, &cfg.Fleet, db, s3Client, cfg.Storage.Bucket, cfg.Fleet.FactoryID)

	// Create EpisodeHandler for episode listing
	episodeHandler := handlers.NewEpisodeHandler(db)

	// Create TaskHandler for task configuration
	taskHandler := handlers.NewTaskHandler(db)

	s := &Server{
		cfg:      cfg,
		health:   healthHandler,
		transfer: transferHandler,
		episode:  episodeHandler,
		task:     taskHandler,
		engine:   engine,
	}

	s.httpServer = &http.Server{
		Addr:         cfg.Server.BindAddr,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout) * time.Second,
		Handler:      s.buildRoutes(),
	}

	// Create separate WebSocket server on WSPort
	wsAddr := fmt.Sprintf(":%d", cfg.Fleet.WSPort)
	s.wsServer = &http.Server{
		Addr:         wsAddr,
		ReadTimeout:  0, // Controlled by application-level readTimeout
		WriteTimeout: 0, // Must be 0 for WebSocket long-lived connections
		Handler:      s.buildWSRoutes(transferHandler),
	}

	return s
}

// buildRoutes constructs the HTTP router
func (s *Server) buildRoutes() http.Handler {
	// Set basePath for swagger
	docs.SwaggerInfo.BasePath = "/api/v1"

	bindAddr := s.cfg.Server.BindAddr
	if strings.HasPrefix(bindAddr, ":") {
		docs.SwaggerInfo.Host = "localhost" + bindAddr
	} else {
		docs.SwaggerInfo.Host = bindAddr
	}

	// API v1 group
	v1 := s.engine.Group("/api/v1")

	// Health check - register only in API v1 group
	s.health.RegisterAPI(v1)

	// Fleet Manager: REST API only (WebSocket on separate port)
	v1Transfer := v1.Group("/transfer")
	s.transfer.RegisterRoutes(v1Transfer)

	// Episodes API
	v1Episodes := v1.Group("/episodes")
	s.episode.RegisterRoutes(v1Episodes)

	// Tasks API
	v1Tasks := v1.Group("")
	s.task.RegisterRoutes(v1Tasks)

	// Axon callbacks
	v1Callbacks := v1.Group("/callbacks")
	s.transfer.RegisterCallbackRoutes(v1Callbacks)

	// Task callbacks
	s.task.RegisterCallbackRoutes(v1Callbacks)

	// Swagger documentation - serve at both root and api/v1 path
	s.engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	s.engine.GET("/api/v1/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	return s.engine
}

// buildWSRoutes constructs the WebSocket-only router using standard net/http
func (s *Server) buildWSRoutes(transferHandler *handlers.TransferHandler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/transfer/", func(w http.ResponseWriter, r *http.Request) {
		// Extract device_id from URL path
		deviceID := strings.TrimPrefix(r.URL.Path, "/transfer/")
		if deviceID == "" || deviceID == r.URL.Path {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		transferHandler.HandleWebSocket(w, r, deviceID)
	})

	return mux
}

// Start starts the HTTP server
func (s *Server) Start() error {
	s.shutdownMu.Lock()
	s.isRunning = true
	s.shutdownMu.Unlock()

	log.Printf("[SERVER] Starting HTTP server on %s", s.cfg.Server.BindAddr)
	log.Printf("[SERVER] Swagger UI: http://localhost%s/swagger/index.html", s.cfg.Server.BindAddr)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SERVER] HTTP server error: %v", err)
		}
	}()

	// Start WebSocket server on separate port
	wsAddr := fmt.Sprintf(":%d", s.cfg.Fleet.WSPort)
	log.Printf("[SERVER] Starting WebSocket server on %s", wsAddr)

	go func() {
		if err := s.wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SERVER] WebSocket server error: %v", err)
		}
	}()

	return nil
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	if !s.isRunning {
		s.shutdownMu.Unlock()
		return nil
	}
	s.isRunning = false
	s.shutdownMu.Unlock()

	log.Printf("[SERVER] Shutting down HTTP server")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Server.ShutdownTimeout)*time.Second)
	defer cancel()

	// Shutdown both servers
	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Printf("[SERVER] HTTP server shutdown error: %v", err)
	}
	if err := s.wsServer.Shutdown(ctx); err != nil {
		log.Printf("[SERVER] WebSocket server shutdown error: %v", err)
	}

	return nil
}

// Addr returns the server address
func (s *Server) Addr() string {
	return s.httpServer.Addr
}
