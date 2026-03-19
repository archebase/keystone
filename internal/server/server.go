// Package server provides HTTP server for Keystone Edge API
package server

import (
	"context"
	"fmt"
	"log"
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
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/storage/s3"

	"github.com/jmoiron/sqlx"
)

// Server represents the HTTP server
type Server struct {
	cfg              *config.Config
	health           *handlers.HealthHandler
	transfer         *handlers.TransferHandler
	recorder         *handlers.RecorderHandler
	episode          *handlers.EpisodeHandler
	task             *handlers.TaskHandler
	robotType        *handlers.RobotTypeHandler
	robot            *handlers.RobotHandler
	factory          *handlers.FactoryHandler
	dataCollector    *handlers.DataCollectorHandler
	station          *handlers.StationHandler
	httpServer       *http.Server
	transferWSServer *http.Server
	recorderWSServer *http.Server
	shutdownMu       sync.RWMutex
	isRunning        bool
	engine           *gin.Engine
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

	// Create TransferHub and TransferHandler for Transfer Service
	transferHub := services.NewTransferHub(cfg.AxonTransfer.MaxEvents)
	transferHandler := handlers.NewTransferHandler(transferHub, &cfg.AxonTransfer, db, s3Client, cfg.Storage.Bucket, cfg.AxonTransfer.FactoryID)

	// Create recorderHub and RecorderHandler for Axon Recorder RPC
	recorderHub := services.NewRecorderHub()
	recorderHandler := handlers.NewRecorderHandler(recorderHub, &cfg.AxonRecorder, db)

	// Create EpisodeHandler for episode listing
	episodeHandler := handlers.NewEpisodeHandler(db)

	// Create TaskHandler for task configuration
	taskHandler := handlers.NewTaskHandler(db)

	// Create database-dependent handlers only when DB is available
	var (
		robotTypeHandler     *handlers.RobotTypeHandler
		robotHandler         *handlers.RobotHandler
		factoryHandler       *handlers.FactoryHandler
		dataCollectorHandler *handlers.DataCollectorHandler
		stationHandler       *handlers.StationHandler
	)
	if db != nil {
		robotTypeHandler = handlers.NewRobotTypeHandler(db)
		robotHandler = handlers.NewRobotHandler(db)
		factoryHandler = handlers.NewFactoryHandler(db)
		dataCollectorHandler = handlers.NewDataCollectorHandler(db)
		stationHandler = handlers.NewStationHandler(db)
	}

	s := &Server{
		cfg:           cfg,
		health:        healthHandler,
		transfer:      transferHandler,
		recorder:      recorderHandler,
		episode:       episodeHandler,
		task:          taskHandler,
		robotType:     robotTypeHandler,
		robot:         robotHandler,
		factory:       factoryHandler,
		dataCollector: dataCollectorHandler,
		station:       stationHandler,
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

	// Transfer Service API
	v1Transfer := v1.Group("/transfer")
	s.transfer.RegisterRoutes(v1Transfer)

	// Episodes API
	v1Episodes := v1.Group("/episodes")
	s.episode.RegisterRoutes(v1Episodes)

	// Tasks API
	v1Tasks := v1.Group("")
	s.task.RegisterRoutes(v1Tasks)
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

	// Axon callbacks
	v1Callbacks := v1.Group("/callbacks")
	s.transfer.RegisterCallbackRoutes(v1Callbacks)

	// Task callbacks
	s.task.RegisterCallbackRoutes(v1Callbacks)

	v1Recorder := v1.Group("/recorder")
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
			log.Printf("[RECORDER] Rejected: empty or invalid device_id (path=%s)", r.URL.Path)
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

	log.Printf("[SERVER] Starting HTTP server on %s", s.cfg.Server.BindAddr)
	log.Printf("[SERVER] Swagger UI: http://localhost%s/swagger/index.html", s.cfg.Server.BindAddr)

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SERVER] HTTP server error: %v", err)
		}
	}()

	// Start WebSocket server on separate port
	log.Printf("[SERVER] Transfer WebSocket server listening on %d", s.cfg.AxonTransfer.WSPort)

	go func() {
		if err := s.transferWSServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SERVER] Transfer WebSocket server error: %v", err)
		}
	}()

	if s.recorderWSServer != nil {
		recorderWSAddr := fmt.Sprintf(":%d", s.cfg.AxonRecorder.WSPort)
		ln, err := net.Listen("tcp", recorderWSAddr)
		if err != nil {
			log.Printf("[SERVER] Recorder WebSocket server listen failed: %v", err)
		} else {
			log.Printf("[SERVER] Recorder WebSocket server listening on %d", s.cfg.AxonRecorder.WSPort)
			go func() {
				if err := s.recorderWSServer.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("[SERVER] Recorder WebSocket server error: %v", err)
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

	log.Printf("[SERVER] Shutting down HTTP server")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Server.ShutdownTimeout)*time.Second)
	defer cancel()

	// Shutdown both servers
	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Printf("[SERVER] HTTP server shutdown error: %v", err)
	}
	if err := s.transferWSServer.Shutdown(ctx); err != nil {
		log.Printf("[SERVER] Transfer WebSocket server shutdown error: %v", err)
	}
	if s.recorderWSServer != nil {
		if err := s.recorderWSServer.Shutdown(ctx); err != nil {
			log.Printf("[SERVER] Recorder WebSocket server shutdown error: %v", err)
		}
	}

	return nil
}

// Addr returns the server address
func (s *Server) Addr() string {
	return s.httpServer.Addr
}
