// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package server provides HTTP server for Keystone Edge API
package server

import (
	"context"
	"fmt"
	"net"
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
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/jmoiron/sqlx"
)

// Server represents the HTTP server
type Server struct {
	cfg              *config.Config
	health           *handlers.HealthHandler
	auth             *handlers.AuthHandler
	storage          *handlers.StorageHandler
	transfer         *handlers.TransferHandler
	recorder         *handlers.RecorderHandler
	episode          *handlers.EpisodeHandler
	task             *handlers.TaskHandler
	batch            *handlers.BatchHandler
	robotType        *handlers.RobotTypeHandler
	robot            *handlers.RobotHandler
	factory          *handlers.FactoryHandler
	dataCollector    *handlers.DataCollectorHandler
	station          *handlers.StationHandler
	organization     *handlers.OrganizationHandler
	skill            *handlers.SkillHandler
	inspector        *handlers.InspectorHandler
	sop              *handlers.SOPHandler
	scene            *handlers.SceneHandler
	subscene         *handlers.SubsceneHandler
	order            *handlers.OrderHandler
	syncHandler      *handlers.SyncHandler
	syncWorker       *services.SyncWorker
	httpServer       *http.Server
	transferWSServer *http.Server
	recorderWSServer *http.Server
	shutdownMu       sync.RWMutex
	isRunning        bool
	engine           *gin.Engine
}

// New creates a new server instance.
// db and s3Client are optional; pass nil to disable Verified ACK.
// syncWorker is optional; pass nil to disable cloud sync API.
func New(cfg *config.Config, db *sqlx.DB, s3Client *s3.Client, syncWorker *services.SyncWorker) *Server {
	// Create Gin engine
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(gin.Logger())

	// Create handlers
	healthHandler := handlers.NewHealthHandler(nil, nil)
	var authHandler *handlers.AuthHandler
	if db != nil {
		authHandler = handlers.NewAuthHandler(db, &cfg.Auth)
	}
	var storageHandler *handlers.StorageHandler
	if s3Client != nil {
		storageHandler = handlers.NewStorageHandler(s3Client, &cfg.Auth)
	}

	// Recorder hub must exist before TransferHandler (transfer disconnect notifies recorder via RPC).
	recorderHub := services.NewRecorderHub()
	recorderHandler := handlers.NewRecorderHandler(recorderHub, &cfg.AxonRecorder, db)
	recorderRPCTimeout := time.Duration(cfg.AxonRecorder.ResponseTimeout) * time.Second

	// Create TransferHub and TransferHandler for Transfer Service
	transferHub := services.NewTransferHub(cfg.AxonTransfer.MaxEvents)
	transferHandler := handlers.NewTransferHandler(transferHub, &cfg.AxonTransfer, db, s3Client, cfg.Storage.Bucket, cfg.AxonTransfer.FactoryID, recorderHub, recorderRPCTimeout)

	// Create EpisodeHandler for episode listing
	episodeHandler := handlers.NewEpisodeHandler(db, s3Client, cfg.Storage.Bucket)

	// Create TaskHandler for task configuration
	taskHandler := handlers.NewTaskHandler(db, transferHub, recorderHub, recorderRPCTimeout)

	// Create database-dependent handlers only when DB is available
	var (
		batchHandler         *handlers.BatchHandler
		robotTypeHandler     *handlers.RobotTypeHandler
		robotHandler         *handlers.RobotHandler
		factoryHandler       *handlers.FactoryHandler
		dataCollectorHandler *handlers.DataCollectorHandler
		stationHandler       *handlers.StationHandler
		organizationHandler  *handlers.OrganizationHandler
		skillHandler         *handlers.SkillHandler
		inspectorHandler     *handlers.InspectorHandler
		sopHandler           *handlers.SOPHandler
		sceneHandler         *handlers.SceneHandler
		subsceneHandler      *handlers.SubsceneHandler
		orderHandler         *handlers.OrderHandler
	)
	if db != nil {
		batchHandler = handlers.NewBatchHandler(db, recorderHub, recorderRPCTimeout)
		robotTypeHandler = handlers.NewRobotTypeHandler(db)
		robotHandler = handlers.NewRobotHandler(db, recorderHub, transferHub)
		factoryHandler = handlers.NewFactoryHandler(db)
		dataCollectorHandler = handlers.NewDataCollectorHandler(db)
		stationHandler = handlers.NewStationHandler(db)
		organizationHandler = handlers.NewOrganizationHandler(db)
		skillHandler = handlers.NewSkillHandler(db)
		inspectorHandler = handlers.NewInspectorHandler(db)
		sopHandler = handlers.NewSOPHandler(db)
		sceneHandler = handlers.NewSceneHandler(db)
		subsceneHandler = handlers.NewSubsceneHandler(db)
		orderHandler = handlers.NewOrderHandler(db, recorderHub, recorderRPCTimeout)
	}

	// Create SyncHandler for cloud sync API
	var syncHandler *handlers.SyncHandler
	if db != nil {
		syncHandler = handlers.NewSyncHandler(db, syncWorker)
	}

	s := &Server{
		cfg:           cfg,
		health:        healthHandler,
		auth:          authHandler,
		storage:       storageHandler,
		transfer:      transferHandler,
		recorder:      recorderHandler,
		episode:       episodeHandler,
		task:          taskHandler,
		batch:         batchHandler,
		robotType:     robotTypeHandler,
		robot:         robotHandler,
		factory:       factoryHandler,
		dataCollector: dataCollectorHandler,
		station:       stationHandler,
		organization:  organizationHandler,
		skill:         skillHandler,
		inspector:     inspectorHandler,
		sop:           sopHandler,
		scene:         sceneHandler,
		subscene:      subsceneHandler,
		order:         orderHandler,
		syncHandler:   syncHandler,
		syncWorker:    syncWorker,
		engine:        engine,
	}

	s.httpServer = &http.Server{
		Addr:         cfg.Server.BindAddr,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout) * time.Second,
		Handler:      s.buildRoutes(),
	}

	// Create separate WebSocket server on WSPort
	wsAddr := fmt.Sprintf(":%d", cfg.AxonTransfer.WSPort)
	s.transferWSServer = &http.Server{
		Addr:         wsAddr,
		ReadTimeout:  0, // Controlled by application-level readTimeout
		WriteTimeout: 0, // Must be 0 for WebSocket long-lived connections
		Handler:      s.buildTransferWSRoutes(transferHandler),
	}

	s.recorderWSServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.AxonRecorder.WSPort),
		ReadTimeout:  0,
		WriteTimeout: 0,
		Handler:      s.buildRecorderWSRoutes(recorderHandler),
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

	v1Routes := v1.Group("")
	if s.auth != nil {
		s.auth.RegisterRoutes(v1Routes)
	}
	if s.storage != nil {
		s.storage.RegisterRoutes(v1Routes)
	}

	// Transfer Service API
	v1Transfer := v1Routes.Group("/transfer")
	s.transfer.RegisterRoutes(v1Transfer)

	// Episodes API
	v1Episodes := v1Routes.Group("/episodes")
	s.episode.RegisterRoutes(v1Episodes)

	// Tasks API
	v1Tasks := v1Routes.Group("")
	s.task.RegisterRoutes(v1Tasks)
	if s.batch != nil {
		s.batch.RegisterRoutes(v1Tasks)
	}
	if s.robotType != nil {
		s.robotType.RegisterRoutes(v1Tasks)
	}
	if s.robot != nil {
		s.robot.RegisterRoutes(v1Tasks)
	}
	if s.factory != nil {
		s.factory.RegisterRoutes(v1Tasks)
	}
	if s.dataCollector != nil {
		s.dataCollector.RegisterRoutes(v1Tasks)
	}
	if s.station != nil {
		s.station.RegisterRoutes(v1Tasks)
	}
	if s.organization != nil {
		s.organization.RegisterRoutes(v1Tasks)
	}
	if s.skill != nil {
		s.skill.RegisterRoutes(v1Tasks)
	}
	if s.inspector != nil {
		s.inspector.RegisterRoutes(v1Tasks)
	}
	if s.sop != nil {
		s.sop.RegisterRoutes(v1Tasks)
	}
	if s.scene != nil {
		s.scene.RegisterRoutes(v1Tasks)
	}
	if s.subscene != nil {
		s.subscene.RegisterRoutes(v1Tasks)
	}
	if s.order != nil {
		s.order.RegisterRoutes(v1Tasks)
	}

	// Cloud Sync API
	if s.syncHandler != nil {
		s.syncHandler.RegisterRoutes(v1Routes)
	}

	// Axon callbacks
	v1Callbacks := v1Routes.Group("/callbacks")

	// Task callbacks
	s.task.RegisterCallbackRoutes(v1Callbacks)

	v1Recorder := v1Routes.Group("/recorder")
	s.recorder.RegisterRoutes(v1Recorder)

	// Swagger documentation - serve at both root and api/v1 path
	s.engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	s.engine.GET("/api/v1/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	return s.engine
}

// buildTransferWSRoutes constructs the WebSocket-only router using standard net/http
func (s *Server) buildTransferWSRoutes(transferHandler *handlers.TransferHandler) http.Handler {
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

// buildRecorderWSRoutes constructs the WebSocket router for Axon Recorder RPC.
func (s *Server) buildRecorderWSRoutes(recorderHandler *handlers.RecorderHandler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/recorder/", func(w http.ResponseWriter, r *http.Request) {
		deviceID := strings.TrimPrefix(r.URL.Path, "/recorder/")
		if deviceID == "" || deviceID == r.URL.Path {
			// #nosec G706 -- Set aside for now
			logger.Printf("[SERVER] Rejected: empty or invalid device_id (path=%s)", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		recorderHandler.HandleWebSocket(w, r, deviceID)
	})

	return mux
}

// Start starts the HTTP server
func (s *Server) Start() error {
	s.shutdownMu.Lock()
	s.isRunning = true
	s.shutdownMu.Unlock()

	logger.Printf("[SERVER] Starting HTTP server on %s", s.cfg.Server.BindAddr)
	logger.Printf("[SERVER] Swagger UI: http://localhost%s/swagger/index.html", s.cfg.Server.BindAddr)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("[SERVER] HTTP server error: %v", err)
		}
	}()

	// Start WebSocket server on separate port
	logger.Printf("[SERVER] Transfer WebSocket server listening on %d", s.cfg.AxonTransfer.WSPort)

	go func() {
		if err := s.transferWSServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("[SERVER] Transfer WebSocket server error: %v", err)
		}
	}()

	if s.recorderWSServer != nil {
		recorderWSAddr := fmt.Sprintf(":%d", s.cfg.AxonRecorder.WSPort)
		ln, err := net.Listen("tcp", recorderWSAddr)
		if err != nil {
			logger.Printf("[SERVER] Recorder WebSocket server listen failed: %v", err)
		} else {
			logger.Printf("[SERVER] Recorder WebSocket server listening on %d", s.cfg.AxonRecorder.WSPort)
			go func() {
				if err := s.recorderWSServer.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Printf("[SERVER] Recorder WebSocket server error: %v", err)
				}
			}()
		}
	}

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

	logger.Printf("[SERVER] Shutting down HTTP server")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Server.ShutdownTimeout)*time.Second)
	defer cancel()

	// Shutdown both servers
	if err := s.httpServer.Shutdown(ctx); err != nil {
		logger.Printf("[SERVER] HTTP server shutdown error: %v", err)
	}
	if err := s.transferWSServer.Shutdown(ctx); err != nil {
		logger.Printf("[SERVER] Transfer WebSocket server shutdown error: %v", err)
	}
	if s.recorderWSServer != nil {
		if err := s.recorderWSServer.Shutdown(ctx); err != nil {
			logger.Printf("[SERVER] Recorder WebSocket server shutdown error: %v", err)
		}
	}

	// Stop sync worker
	if s.syncWorker != nil {
		s.syncWorker.Stop()
	}

	return nil
}

// Addr returns the server address
func (s *Server) Addr() string {
	return s.httpServer.Addr
}
