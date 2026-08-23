// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package server provides HTTP server for Keystone Edge API.
//
// @securityDefinitions.apikey DeviceAuth
// @in header
// @name Device-Authorization
// @description Persistent administrator-issued device credential, optionally prefixed with Bearer.
//
// @securityDefinitions.apikey DeviceJWT
// @in header
// @name Authorization
// @description Temporary Device JWT using the Bearer scheme; do not send together with Device-Authorization.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
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
	"archebase.com/keystone-edge/internal/middleware"
	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/services/autosync"
	"archebase.com/keystone-edge/internal/services/calibration"
	"archebase.com/keystone-edge/internal/services/depthnorm"
	"archebase.com/keystone-edge/internal/services/deviceauth"
	"archebase.com/keystone-edge/internal/services/stereosplit"
	"archebase.com/keystone-edge/internal/storage/s3"
	tosstorage "archebase.com/keystone-edge/internal/storage/tos"

	"github.com/jmoiron/sqlx"
)

const dcPlanAutoSyncInterval = 5 * time.Minute

// Server represents the HTTP server
type Server struct {
	cfg                 *config.Config
	db                  *sqlx.DB
	health              *handlers.HealthHandler
	auth                *handlers.AuthHandler
	storage             *handlers.StorageHandler
	transfer            *handlers.TransferHandler
	recorder            *handlers.RecorderHandler
	deviceState         *handlers.DeviceStateHandler
	episode             *handlers.EpisodeHandler
	qa                  *handlers.EpisodeQAHandler
	task                *handlers.TaskHandler
	robot               *handlers.RobotHandler
	deviceRegistration  *handlers.DeviceRegistrationHandler
	dataCollector       *handlers.DataCollectorHandler
	station             *handlers.StationHandler
	workspace           *handlers.WorkspaceHandler
	dcPlan              *handlers.DCPlanHandler
	dataOps             *handlers.DataOpsHandler
	dataStats           *handlers.DataProductionStatisticsHandler
	productionDashboard *handlers.ProductionDashboardHandler
	syncHandler         *handlers.SyncHandler
	localCleanup        *handlers.LocalCleanupHandler
	localCleanupWorker  *services.LocalCleanupWorker
	syncWorker          *services.SyncWorker
	autoSync            *autosync.Manager
	autoSyncSettings    *handlers.AutoSyncSettingsHandler
	stereoSplit         *stereosplit.Manager
	depthNorm           *depthnorm.Manager
	calibration         *calibration.Manager
	calibrationHandler  *handlers.CalibrationHandler
	cameraCalibration   *handlers.CameraCalibrationHandler
	workspaceSync       *services.WorkspaceSyncService
	dcPlanSync          *services.DCPlanSyncService
	httpServer          *http.Server
	transferWSServer    *http.Server
	recorderWSServer    *http.Server
	dcPlanSyncCancel    context.CancelFunc
	dcPlanSyncDone      chan struct{}
	shutdownMu          sync.RWMutex
	isRunning           bool
	engine              *gin.Engine
}

func axonTransferWriteTimeout(cfg *config.TransferConfig) time.Duration {
	if cfg == nil || cfg.WriteTimeout <= 0 {
		return services.DefaultTransferWriteTimeout
	}
	return time.Duration(cfg.WriteTimeout) * time.Second
}

func loadBalancerHealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("ok\n"))
	}
}

// New creates a new server instance after recovering stale bulk runs.
// db and s3Client are optional; pass nil to disable Verified ACK.
// syncWorker is optional; pass nil to disable cloud sync APIs.
func New(cfg *config.Config, db *sqlx.DB, s3Client *s3.Client, syncWorker *services.SyncWorker) (*Server, error) {
	// Create Gin engine
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(gin.Logger())

	// Create handlers
	healthHandler := handlers.NewHealthHandler(nil, nil)
	var authHandler *handlers.AuthHandler
	if db != nil {
		authHandler = handlers.NewAuthHandler(db, &cfg.Auth, &cfg.Hilbert)
	}

	var calibrationManager *calibration.Manager
	var calibrationHandler *handlers.CalibrationHandler
	var cameraCalibrationHandler *handlers.CameraCalibrationHandler
	if db != nil {
		var orbitClient calibration.Orbit
		var objectReader calibration.ObjectStore
		if cfg.Calibration.OrbitBaseURL != "" {
			client, err := orbitapi.NewClient(
				cfg.Calibration.OrbitBaseURL,
				time.Duration(cfg.Calibration.OrbitTimeoutSec)*time.Second,
			)
			if err != nil {
				return nil, fmt.Errorf("initialize calibration Orbit client: %w", err)
			}
			orbitClient = client
			objectReader = tosstorage.NewClient(
				cfg.TOSStorage,
				time.Duration(cfg.Calibration.OrbitTimeoutSec)*time.Second,
			)
		}
		calibrationManager = calibration.NewManager(
			db,
			orbitClient,
			objectReader,
			calibrationManagerConfig(cfg.Calibration),
		)
		calibrationHandler = handlers.NewCalibrationHandler(calibrationManager)
		cameraCalibrationHandler = handlers.NewCameraCalibrationHandler(
			db,
			tosstorage.NewClient(cfg.TOSStorage, time.Duration(cfg.Calibration.OrbitTimeoutSec)*time.Second),
			cfg.TOSStorage.Bucket,
		)
	}
	var storageHandler *handlers.StorageHandler
	if s3Client != nil || cfg.TOSStorage.Type == "tos" {
		storageHandler = handlers.NewStorageHandler(s3Client, &cfg.Auth, &cfg.TOSStorage)
	}

	// Recorder hub must exist before TransferHandler (transfer disconnect notifies recorder via RPC).
	stateBroker := services.NewDeviceStateBroker()
	recorderHub := services.NewRecorderHub()
	recorderHandler := handlers.NewRecorderHandler(recorderHub, &cfg.AxonRecorder, db)
	recorderHandler.SetCallbackPublicBaseURL(cfg.Server.CallbackPublicBaseURL)
	recorderRPCTimeout := time.Duration(cfg.AxonRecorder.ResponseTimeout) * time.Second

	// Create TransferHub and TransferHandler for Transfer Service
	transferHub := services.NewTransferHub(cfg.AxonTransfer.MaxEvents)
	transferHandler := handlers.NewTransferHandler(transferHub, &cfg.AxonTransfer, db, s3Client, cfg.Storage.Bucket, cfg.AxonTransfer.FactoryID, recorderHub, recorderRPCTimeout)
	recorderHandler.SetDeviceStateDeps(transferHub, stateBroker)
	transferHandler.SetDeviceStateBroker(stateBroker)
	deviceStateHandler := handlers.NewDeviceStateHandler(stateBroker, recorderHub, transferHub)

	// Create EpisodeHandler for episode listing
	episodeBucket := cfg.Storage.Bucket
	if s3Client == nil && cfg.TOSStorage.Type == "tos" {
		episodeBucket = cfg.TOSStorage.Bucket
	}
	episodeHandler := handlers.NewEpisodeHandler(db, episodeBucket, &cfg.Auth)
	qaHandler := handlers.NewEpisodeQAHandler(db, s3Client, cfg.Storage.Bucket, &cfg.Auth, &cfg.TOSStorage)
	qaHandler.SetDeviceStateBroker(stateBroker)
	transferHandler.SetEpisodeQAEnqueuer(qaHandler)

	transferWriteTimeout := axonTransferWriteTimeout(&cfg.AxonTransfer)

	// Create TaskHandler for task configuration
	taskHandler := handlers.NewTaskHandler(db, transferHub, recorderHub, recorderRPCTimeout, transferWriteTimeout)
	taskHandler.SetCallbackPublicBaseURL(cfg.Server.CallbackPublicBaseURL)

	// Create database-dependent handlers only when DB is available
	var (
		robotHandler               *handlers.RobotHandler
		deviceRegistrationHandler  *handlers.DeviceRegistrationHandler
		dataCollectorHandler       *handlers.DataCollectorHandler
		stationHandler             *handlers.StationHandler
		workspaceHandler           *handlers.WorkspaceHandler
		dcPlanHandler              *handlers.DCPlanHandler
		dataOpsHandler             *handlers.DataOpsHandler
		dataStatsHandler           *handlers.DataProductionStatisticsHandler
		productionDashboardHandler *handlers.ProductionDashboardHandler
		workspaceSyncService       *services.WorkspaceSyncService
		dcPlanSyncService          *services.DCPlanSyncService
	)
	if db != nil {
		workspaceSyncService = services.NewWorkspaceSyncService(db, &cfg.Hilbert, nil)
		dcPlanSyncService = services.NewDCPlanSyncService(db, &cfg.Hilbert, nil)
		robotHandler = handlers.NewRobotHandler(db, recorderHub, transferHub)
		deviceRegistrationHandler = handlers.NewDeviceRegistrationHandler(db, cfg.Server.CallbackPublicBaseURL)
		dataCollectorHandler = handlers.NewDataCollectorHandler(db)
		stationHandler = handlers.NewStationHandler(db)
		workspaceHandler = handlers.NewWorkspaceHandler(db, workspaceSyncService)
		dcPlanHandler = handlers.NewDCPlanHandler(db, dcPlanSyncService)
		dataOpsHandler = handlers.NewDataOpsHandler(db)
		dataOpsHandler.SetBulkActionDeps(qaHandler, syncWorker)
		if err := dataOpsHandler.InterruptActiveBulkRuns(context.Background(), cfg.Sync.MaxRetries); err != nil {
			return nil, fmt.Errorf("recover stale bulk runs: %w", err)
		}
		dataStatsHandler = handlers.NewDataProductionStatisticsHandler(db)
		productionDashboardHandler = handlers.NewProductionDashboardHandler(db, recorderHub, transferHub)
	}

	var stereoSplitManager *stereosplit.Manager
	if db != nil && cfg.Derivatives.Enabled {
		orbitClient, err := orbitapi.NewClient(
			cfg.Derivatives.OrbitBaseURL,
			time.Duration(cfg.Derivatives.OrbitTimeoutSec)*time.Second,
		)
		if err != nil {
			return nil, fmt.Errorf("initialize Orbit client: %w", err)
		}
		objectReader := tosstorage.NewClient(
			cfg.TOSStorage,
			time.Duration(cfg.Derivatives.OrbitTimeoutSec)*time.Second,
		)
		stereoSplitManager = stereosplit.NewManager(db, orbitClient, objectReader, stereoSplitConfig(cfg.Derivatives))
		dataOpsHandler.SetStereoSplitManager(stereoSplitManager)
		if calibrationManager != nil {
			calibrationManager.SetStereoPreprocessor(stereoSplitManager)
		}
		if err := dataOpsHandler.ResumeStereoSplitBulkRuns(context.Background()); err != nil {
			return nil, fmt.Errorf("resume stereo split bulk runs: %w", err)
		}
	}

	// Create SyncHandler for cloud sync API
	var syncHandler *handlers.SyncHandler
	var localCleanupHandler *handlers.LocalCleanupHandler
	var localCleanupWorker *services.LocalCleanupWorker
	if db != nil {
		syncHandler = handlers.NewSyncHandler(db, syncWorker)
		if s3Client != nil {
			cleanupService := services.NewLocalCleanupService(db, s3Client, cfg.Storage.Bucket)
			localCleanupHandler = handlers.NewLocalCleanupHandler(cleanupService)
			localCleanupWorker = services.NewLocalCleanupWorker(cleanupService, time.Minute)
			dataOpsHandler.SetLocalCleanupService(cleanupService)
		}
	}

	var depthNormManager *depthnorm.Manager
	if cfg.Server.Mode == config.ModeEdge && cfg.DepthNormalization.Enabled {
		if _, err := os.Stat(cfg.DepthNormalization.Script); err != nil {
			return nil, fmt.Errorf("depth normalization script is unavailable: %w", err)
		}
	}
	if db != nil && s3Client != nil && cfg.Server.Mode == config.ModeEdge && cfg.DepthNormalization.Enabled {
		depthNormManager = depthnorm.NewManager(db, s3Client, cfg.Storage.Bucket, depthnorm.Config{
			Enabled:      cfg.DepthNormalization.Enabled,
			Script:       cfg.DepthNormalization.Script,
			Timeout:      time.Duration(cfg.DepthNormalization.TimeoutSec) * time.Second,
			MinFreeDisk:  int64(cfg.DepthNormalization.MinFreeDiskGB),
			OutputPrefix: cfg.DepthNormalization.OutputPrefix,
		})
	}

	if depthNormManager != nil {
		dataOpsHandler.SetDepthNormalizer(depthNormManager)
	}

	var autoSyncManager *autosync.Manager
	var autoSyncSettingsHandler *handlers.AutoSyncSettingsHandler
	if db != nil && syncWorker != nil && stereoSplitManager != nil {
		autoSyncManager = autosync.NewManager(db, stereoSplitManager, syncWorker, 0, depthNormManager)
		autoSyncManager.SetQAEnqueuer(qaHandler)
		qaHandler.SetAutoSyncCapturer(autoSyncManager)
		autoSyncSettingsHandler = handlers.NewAutoSyncSettingsHandler(autoSyncManager)
	}

	s := &Server{
		cfg:                 cfg,
		db:                  db,
		health:              healthHandler,
		auth:                authHandler,
		storage:             storageHandler,
		transfer:            transferHandler,
		recorder:            recorderHandler,
		deviceState:         deviceStateHandler,
		episode:             episodeHandler,
		qa:                  qaHandler,
		task:                taskHandler,
		robot:               robotHandler,
		deviceRegistration:  deviceRegistrationHandler,
		dataCollector:       dataCollectorHandler,
		station:             stationHandler,
		workspace:           workspaceHandler,
		dcPlan:              dcPlanHandler,
		dataOps:             dataOpsHandler,
		dataStats:           dataStatsHandler,
		productionDashboard: productionDashboardHandler,
		syncHandler:         syncHandler,
		localCleanup:        localCleanupHandler,
		localCleanupWorker:  localCleanupWorker,
		syncWorker:          syncWorker,
		autoSync:            autoSyncManager,
		autoSyncSettings:    autoSyncSettingsHandler,
		stereoSplit:         stereoSplitManager,
		depthNorm:           depthNormManager,
		calibration:         calibrationManager,
		calibrationHandler:  calibrationHandler,
		cameraCalibration:   cameraCalibrationHandler,
		workspaceSync:       workspaceSyncService,
		dcPlanSync:          dcPlanSyncService,
		engine:              engine,
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

	return s, nil
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

	// Root health check for load balancers that probe "/" on the Keystone backend.
	s.engine.GET("/", s.health.Handler)
	// Prefix health check for load balancers that probe the Ingress backend path.
	s.engine.GET("/api", s.health.Handler)

	// Health check
	s.health.RegisterAPI(v1)

	v1Routes := v1.Group("")
	if s.auth != nil {
		// Public auth routes (login, logout) — no middleware required.
		s.auth.RegisterRoutes(v1Routes)

		// Authenticated auth routes:
		//   GET  /auth/me                    — identity or workstation token
		//   POST /auth/workstation/activate — data_collector identity token
		//   POST /auth/me/station/*         — active workstation token
		identityMw := middleware.IdentityJWTAuth(&s.cfg.Auth)
		workstationMw := middleware.JWTAuth(&s.cfg.Auth, s.db)
		meGroup := v1Routes.Group("/auth/me", identityMw)
		stationGroup := v1Routes.Group("/auth/me/station", workstationMw, middleware.RequireRole("data_collector"))
		activationGroup := v1Routes.Group("/auth/workstation", identityMw, middleware.RequireRole("data_collector"))
		s.auth.RegisterAuthenticatedRoutes(meGroup, stationGroup, activationGroup)
	}
	if s.storage != nil {
		s.storage.RegisterRoutes(v1Routes)
	}
	if s.calibrationHandler != nil {
		deviceAuthenticator := deviceauth.New(s.db, deviceauth.Config{JWTSecret: s.cfg.Auth.JWTSecret})
		deviceCalibration := v1Routes.Group("", middleware.DeviceAuth(deviceAuthenticator))
		s.calibrationHandler.RegisterDeviceRoutes(deviceCalibration)
		adminCalibration := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin"))
		s.calibrationHandler.RegisterAdminRoutes(adminCalibration)
		if s.cameraCalibration != nil {
			s.cameraCalibration.RegisterRoutes(adminCalibration)
		}
	}

	// Transfer Service API
	v1Transfer := v1Routes.Group("/transfer")
	s.transfer.RegisterRoutes(v1Transfer)

	// Episodes API
	v1Episodes := v1Routes.Group("/episodes", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireAnyRole("admin", "data_collector"))
	s.episode.RegisterReadRoutes(v1Episodes)
	s.episode.RegisterPresignRoute(v1Routes.Group("/episodes"))
	if s.qa != nil {
		s.qa.RegisterRoutes(v1Routes)
	}

	// Tasks API
	v1Tasks := v1Routes.Group("")
	taskStats := v1Routes.Group("/tasks/statistics", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireAnyRole("admin", "data_collector"))
	taskStats.GET("/breakdown", s.task.GetTaskBreakdown)
	authenticatedTasks := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireAnyRole("admin", "data_collector"))
	s.task.RegisterRoutes(authenticatedTasks)
	collectorTasks := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("data_collector"))
	s.task.RegisterCollectorRoutes(collectorTasks)
	if s.robot != nil {
		s.robot.RegisterRoutes(v1Tasks)
	}
	if s.deviceRegistration != nil {
		s.deviceRegistration.RegisterRoutes(v1Tasks)
		adminDeviceCredentials := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin"))
		s.deviceRegistration.RegisterAdminRoutes(adminDeviceCredentials)
	}
	if s.dataCollector != nil {
		s.dataCollector.RegisterRoutes(v1Tasks)
	}
	if s.station != nil {
		s.station.RegisterRoutes(v1Tasks)
	}
	if s.workspace != nil {
		adminWorkspaces := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin"))
		s.workspace.RegisterRoutes(adminWorkspaces)
	}
	if s.dcPlan != nil {
		readDCPlans := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireAnyRole("admin", "data_collector"))
		s.dcPlan.RegisterReadRoutes(readDCPlans)
		adminDCPlans := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin"))
		s.dcPlan.RegisterAdminRoutes(adminDCPlans)
	}
	if s.dataStats != nil {
		jwtMw := middleware.JWTAuth(&s.cfg.Auth, s.db)
		adminStats := v1Routes.Group("/admin/statistics/data-production", jwtMw, middleware.RequireRole("admin"))
		s.dataStats.RegisterRoutes(adminStats)
		operatorStats := v1Routes.Group("/operator/statistics/data-production", jwtMw, middleware.RequireRole("data_collector"))
		s.dataStats.RegisterOperatorRoutes(operatorStats)
	}
	if s.dataOps != nil {
		jwtMw := middleware.JWTAuth(&s.cfg.Auth, s.db)
		adminDataOps := v1Routes.Group("/data-ops", jwtMw, middleware.RequireRole("admin"))
		s.dataOps.RegisterRoutes(adminDataOps)
	}
	if s.productionDashboard != nil {
		dashboard := v1Routes.Group("/production/dashboard", middleware.DashboardAuth(&s.cfg.Auth, s.db), middleware.RequireAnyRole("admin", "data_collector", "display"))
		s.productionDashboard.RegisterRoutes(dashboard)
	}

	// Cloud Sync API
	if s.syncHandler != nil {
		jwtMw := middleware.JWTAuth(&s.cfg.Auth, s.db)
		readSync := v1Routes.Group("", jwtMw, middleware.RequireAnyRole("admin", "data_collector"))
		adminSync := v1Routes.Group("", jwtMw, middleware.RequireRole("admin"))
		s.syncHandler.RegisterReadRoutes(readSync)
		s.syncHandler.RegisterAdminRoutes(adminSync)
	}
	if s.localCleanup != nil {
		s.localCleanup.RegisterAdminRoutes(v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin")))
	}
	if s.autoSyncSettings != nil {
		adminAutoSync := v1Routes.Group("", middleware.JWTAuth(&s.cfg.Auth, s.db), middleware.RequireRole("admin"))
		s.autoSyncSettings.RegisterRoutes(adminAutoSync)
	}

	// Axon callbacks
	v1Callbacks := v1Routes.Group("/callbacks")

	// Task callbacks
	s.task.RegisterCallbackRoutes(v1Callbacks)

	v1Recorder := v1Routes.Group("/recorder")
	s.recorder.RegisterRoutes(v1Recorder)
	if s.deviceState != nil {
		s.deviceState.RegisterRoutes(v1Routes)
	}

	// Swagger documentation - serve at both root and api/v1 path
	s.engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	s.engine.GET("/api/v1/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	return s.engine
}

// buildTransferWSRoutes constructs the WebSocket-only router using standard net/http
func (s *Server) buildTransferWSRoutes(transferHandler *handlers.TransferHandler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/transfer", loadBalancerHealthHandler)
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

	mux.HandleFunc("/recorder", loadBalancerHealthHandler)
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
	if s.localCleanupWorker != nil {
		s.localCleanupWorker.Start()
	}
	if s.stereoSplit != nil {
		if err := s.stereoSplit.StartReconciler(); err != nil {
			return fmt.Errorf("start stereo split reconciler: %w", err)
		}
		if err := s.stereoSplit.StartVerificationWorkers(); err != nil {
			return fmt.Errorf("start stereo split verification workers: %w", err)
		}
	}
	if s.calibration != nil {
		if err := s.calibration.StartReconciler(); err != nil {
			return fmt.Errorf("start calibration reconciler: %w", err)
		}
	}
	if s.depthNorm != nil {
		if err := s.depthNorm.StartWorker(); err != nil {
			return fmt.Errorf("start depth normalization worker: %w", err)
		}
	}
	if s.autoSync != nil {
		if err := s.autoSync.StartReconciler(); err != nil {
			return fmt.Errorf("start auto sync reconciler: %w", err)
		}
	}
	s.shutdownMu.Lock()
	s.isRunning = true
	s.shutdownMu.Unlock()

	s.syncWorkspacesOnStartup()
	s.syncDCPlansOnStartup()
	s.startPeriodicDCPlanSync()

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

// EpisodeQAEnqueuer exposes the QA handler dependency for services that enqueue episode QA.
func (s *Server) EpisodeQAEnqueuer() interface {
	EnqueueEpisode(episodeID int64)
} {
	if s == nil {
		return nil
	}
	return s.qa
}

func (s *Server) syncWorkspacesOnStartup() {
	if s.workspaceSync == nil || !s.workspaceSync.Configured() {
		logger.Printf("[WORKSPACE] Startup Hilbert workspace sync skipped: service identity config incomplete")
		return
	}
	timeout := time.Duration(s.cfg.Hilbert.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, err := s.workspaceSync.Sync(ctx)
	if err != nil {
		logger.Printf("[WORKSPACE] Startup Hilbert workspace sync failed: %v", err)
		return
	}
	logger.Printf("[WORKSPACE] Startup Hilbert workspace sync completed: synced_count=%d", result.SyncedCount)
}

func (s *Server) syncDCPlansOnStartup() {
	s.syncDCPlansOnce(context.Background(), "Startup")
}

func (s *Server) startPeriodicDCPlanSync() {
	if s.dcPlanSync == nil || !s.dcPlanSync.Configured() {
		logger.Printf("[DC_PLAN] Periodic Hilbert dc plan sync skipped: service identity config incomplete")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.shutdownMu.Lock()
	s.dcPlanSyncCancel = cancel
	s.dcPlanSyncDone = done
	s.shutdownMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(dcPlanAutoSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.syncHilbertResourcesAndDCPlans(ctx, "Periodic")
			case <-ctx.Done():
				return
			}
		}
	}()
	logger.Printf("[DC_PLAN] Periodic Hilbert dc plan sync started: interval=%s", dcPlanAutoSyncInterval)
}

func (s *Server) syncHilbertResourcesAndDCPlans(ctx context.Context, label string) {
	if s.workspaceSync == nil || !s.workspaceSync.Configured() {
		logger.Printf("[HILBERT-SYNC] %s workspace sync skipped: service identity config incomplete", label)
	} else {
		timeout := time.Duration(s.cfg.Hilbert.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		syncCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := s.workspaceSync.Sync(syncCtx)
		cancel()
		if err != nil {
			logger.Printf("[HILBERT-SYNC] %s workspace sync failed; continuing with cached workspaces: %v", label, err)
		} else {
			resourceErrors := 0
			if result.ResourceSync != nil {
				for _, workspaceResult := range result.ResourceSync.WorkspaceResults {
					resourceErrors += len(workspaceResult.Errors)
				}
			}
			logger.Printf("[HILBERT-SYNC] %s workspace and resource sync completed: workspaces=%d resource_errors=%d", label, result.SyncedCount, resourceErrors)
		}
	}

	s.syncDCPlansOnce(ctx, label)
}

func (s *Server) syncDCPlansOnce(ctx context.Context, label string) {
	if s.dcPlanSync == nil || !s.dcPlanSync.Configured() {
		logger.Printf("[DC_PLAN] %s Hilbert dc plan sync skipped: service identity config incomplete", label)
		return
	}
	timeout := time.Duration(s.cfg.Hilbert.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := s.dcPlanSync.SyncAllWorkspaces(syncCtx)
	if err != nil {
		logger.Printf("[DC_PLAN] %s Hilbert dc plan sync failed: %v", label, err)
		return
	}
	logger.Printf(
		"[DC_PLAN] %s Hilbert dc plan sync completed: workspaces=%d failed=%d synced_count=%d page_count=%d",
		label,
		result.WorkspaceCount,
		result.FailedCount,
		result.SyncedCount,
		result.PageCount,
	)
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	if !s.isRunning {
		s.shutdownMu.Unlock()
		return nil
	}
	s.isRunning = false
	dcPlanSyncCancel := s.dcPlanSyncCancel
	dcPlanSyncDone := s.dcPlanSyncDone
	s.dcPlanSyncCancel = nil
	s.dcPlanSyncDone = nil
	s.shutdownMu.Unlock()

	if dcPlanSyncCancel != nil {
		dcPlanSyncCancel()
	}
	if dcPlanSyncDone != nil {
		select {
		case <-dcPlanSyncDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	startedAt := time.Now()
	shutdownTimeout := time.Duration(s.cfg.Server.ShutdownTimeout) * time.Second
	effectiveShutdownTimeout := shutdownTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := deadline.Sub(startedAt); d > 0 && (effectiveShutdownTimeout <= 0 || d < effectiveShutdownTimeout) {
			effectiveShutdownTimeout = d.Round(time.Millisecond)
		}
	}
	logger.Printf("[SERVER] Shutting down HTTP server (timeout=%s timeout_ms=%d)", effectiveShutdownTimeout, effectiveShutdownTimeout.Milliseconds())

	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	logShutdownError := func(component string, err error) {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Printf("[SERVER] %s shutdown timeout after %s (timeout_ms=%d): %v", component, effectiveShutdownTimeout, effectiveShutdownTimeout.Milliseconds(), err)
			return
		}
		logger.Printf("[SERVER] %s shutdown error: %v", component, err)
	}

	var shutdownErr error
	if s.depthNorm != nil {
		if err := s.depthNorm.StopWorker(ctx); err != nil {
			logShutdownError("Depth normalization worker", err)
			shutdownErr = fmt.Errorf("depth normalization worker shutdown: %w", err)
		}
	}
	if s.autoSync != nil {
		if err := s.autoSync.StopReconciler(ctx); err != nil {
			logShutdownError("Auto sync reconciler", err)
			shutdownErr = fmt.Errorf("auto sync reconciler shutdown: %w", err)
		}
	}
	if s.calibration != nil {
		if err := s.calibration.StopReconciler(ctx); err != nil {
			logShutdownError("Calibration reconciler", err)
			shutdownErr = fmt.Errorf("calibration reconciler shutdown: %w", err)
		}
	}

	if s.stereoSplit != nil {
		if err := s.stereoSplit.StopVerificationWorkers(ctx); err != nil {
			logShutdownError("Stereo split verification workers", err)
			shutdownErr = fmt.Errorf("stereo split verification workers shutdown: %w", err)
		}
		if err := s.stereoSplit.StopReconciler(ctx); err != nil {
			logShutdownError("Stereo split reconciler", err)
			shutdownErr = fmt.Errorf("stereo split reconciler shutdown: %w", err)
		}
	}

	// Shutdown both servers
	if err := s.httpServer.Shutdown(ctx); err != nil {
		logShutdownError("HTTP server", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("http server shutdown: %w", err)
		}
	}
	if err := s.transferWSServer.Shutdown(ctx); err != nil {
		logShutdownError("Transfer WebSocket server", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("transfer websocket shutdown: %w", err)
		}
	}
	if s.recorderWSServer != nil {
		if err := s.recorderWSServer.Shutdown(ctx); err != nil {
			logShutdownError("Recorder WebSocket server", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("recorder websocket shutdown: %w", err)
			}
		}
	}

	if s.localCleanupWorker != nil {
		if err := s.localCleanupWorker.Stop(ctx); err != nil {
			logShutdownError("Local cleanup worker", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("local cleanup worker shutdown: %w", err)
			}
		}
	}

	// Stop sync worker
	if s.syncWorker != nil {
		if err := s.syncWorker.Stop(ctx); err != nil {
			logShutdownError("Sync worker", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("sync worker shutdown: %w", err)
			}
		}
	}

	return shutdownErr
}

func stereoSplitConfig(cfg config.DerivativeConfig) stereosplit.Config {
	return stereosplit.Config{
		Enabled:      cfg.Enabled,
		OutputBucket: cfg.OutputBucket,
		OutputPrefix: cfg.OutputPrefix,
		Resources: stereosplit.Resources{
			Requests: cloneServerStringMap(cfg.Resources.Requests),
			Limits:   cloneServerStringMap(cfg.Resources.Limits),
		},
		ActiveDeadline:      cfg.ActiveDeadlineSec,
		TTLSecondsAfterDone: cfg.TTLSecondsAfterDone,
		PollInterval:        time.Duration(cfg.PollIntervalSec) * time.Second,
		MaxSourceBytes:      cfg.MaxSourceBytes,
		LogTailBytes:        cfg.OrbitLogTailBytes,
	}
}

func calibrationManagerConfig(cfg config.CalibrationConfig) calibration.Config {
	return calibration.Config{
		Resources: calibration.Resources{
			Requests: cloneServerStringMap(cfg.Resources.Requests),
			Limits:   cloneServerStringMap(cfg.Resources.Limits),
		},
		ActiveDeadline:      cfg.ActiveDeadlineSec,
		TTLSecondsAfterDone: cfg.TTLSecondsAfterDone,
		PollInterval:        time.Duration(cfg.PollIntervalSec) * time.Second,
		MaxResultBytes:      cfg.MaxResultBytes,
		LogTailBytes:        cfg.OrbitLogTailBytes,
	}
}

func cloneServerStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// Addr returns the server address
func (s *Server) Addr() string {
	return s.httpServer.Addr
}
