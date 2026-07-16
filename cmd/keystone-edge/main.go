// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// main.go - Keystone Edge service entry point
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/server"
	"archebase.com/keystone-edge/internal/services"
	"archebase.com/keystone-edge/internal/services/dgwcompat"
	"archebase.com/keystone-edge/internal/storage/database"
	"archebase.com/keystone-edge/internal/storage/s3"
	tosstorage "archebase.com/keystone-edge/internal/storage/tos"
)

//	@title			Keystone Edge API
//	@version		1.0
//	@description	Backend for edge data collection scenarios.
//	@host			localhost:8080
//	@BasePath		/api/v1

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "Show version information")
	configPath := flag.String("config", "/etc/keystone-edge/config.toml", "Configuration file path")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Keystone Edge %s (built: %s)\n", version, buildTime)
		os.Exit(0)
	}

	logFile, err := os.OpenFile("keystone-edge.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open keystone-edge.log: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = logFile.Close()
	}()

	logger.InitWithWriter(logFile, logger.DefaultOptions())

	if err := godotenv.Load(); err != nil {
		logger.Printf("[SERVER] Failed to load .env file: %v", err)
	}

	logger.Printf("[SERVER] Starting Keystone Edge %s", version)
	logger.Printf("[SERVER] Config file: %s", *configPath)

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("[SERVER] Failed to load config: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		logger.Fatalf("[SERVER] Invalid config: %v", err)
	}

	logger.Printf("[SERVER] Config loaded: mode=%s, bind=%s", cfg.Server.Mode, cfg.Server.BindAddr)

	// Initialize database connection
	db, err := database.Connect(&database.Config{
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		logger.Fatalf("[DATABASE] Failed to connect to database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Printf("[DATABASE] Failed to close database: %v", err)
		}
	}()

	// Auto-run pending migrations on server start
	if err := database.Migrate(db.DB); err != nil {
		logger.Fatalf("[DATABASE] Failed to run database migrations: %v", err)
	}

	// Initialize S3/MinIO storage
	s3Client, err := s3.Connect(&s3.Config{
		Endpoint:     cfg.Storage.Endpoint,
		AccessKey:    cfg.Storage.AccessKey,
		SecretKey:    cfg.Storage.SecretKey,
		Bucket:       cfg.Storage.Bucket,
		UseSSL:       cfg.Storage.UseSSL,
		EnsureBucket: cfg.Storage.EnsureBucket,
	})
	if err != nil {
		logger.Printf("[S3] Failed to connect to S3/MinIO: %v", err)
		s3Client = nil
	}

	// TODO: Start QA worker

	// Initialize cloud sync worker
	var syncWorker *services.SyncWorker
	var sourceReader services.SourceObjectReader
	if cfg.Storage.Type == "tos" {
		sourceReader = tosstorage.NewReader(cfg.Storage, time.Duration(cfg.Sync.OSSTimeoutSec)*time.Second)
	} else if s3Client != nil {
		sourceReader = services.NewMinioSourceObjectReader(s3Client)
	}
	if cfg.Sync.Enabled && cfg.Hilbert.BaseURL != "" && cfg.Hilbert.AccessKey != "" && cfg.Hilbert.SecretKey != "" && sourceReader != nil {
		syncWorker = services.NewSyncWorker(db.DB, nil, s3Client, cfg.Storage.Bucket, services.SyncWorkerConfig{
			BatchSize:       cfg.Sync.BatchSize,
			MaxConcurrent:   cfg.Sync.MaxConcurrent,
			MaxRetries:      cfg.Sync.MaxRetries,
			AutoScanEnabled: cfg.Sync.AutoScanEnabled,
			IntervalSec:     cfg.Sync.WorkerIntervalSec,
			RetryBaseSec:    cfg.Sync.RetryBaseSec,
			RetryMaxSec:     cfg.Sync.RetryMaxSec,
			RetryJitterSec:  cfg.Sync.RetryJitterSec,
		}, &cfg.Sync)
		syncWorker.SetHilbertRawDataClient(auth.NewHilbertClient(&cfg.Hilbert))
		syncWorker.SetSourceObjectReader(sourceReader)

		syncWorker.Start()
		logger.Printf("[SYNC] Hilbert raw-data sync worker started: hilbert_base=%s auto_scan=%t", cfg.Hilbert.BaseURL, cfg.Sync.AutoScanEnabled)
	} else {
		logger.Println("[SYNC] Cloud sync disabled (KEYSTONE_SYNC_ENABLED=false, missing Hilbert config, or source object reader unavailable)")
	}

	// Initialize and start HTTP server
	srv := server.New(cfg, db.DB, s3Client, syncWorker)
	if err := srv.Start(); err != nil {
		logger.Fatalf("[SERVER] Failed to start server: %v", err)
	}
	dgwCompatServer, err := dgwcompat.StartFromEnv(db.DB, srv.EpisodeQAEnqueuer())
	if err != nil {
		logger.Fatalf("[DGW_COMPAT] Failed to start compatibility server: %v", err)
	}

	logger.Println("[SERVER] Keystone Edge started successfully")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Println("[SERVER] Shutting down...")

	shutdownTimeout := 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Printf("[SERVER] Error during shutdown after %s (timeout_ms=%d): %v", shutdownTimeout, shutdownTimeout.Milliseconds(), err)
		} else {
			logger.Printf("[SERVER] Error during shutdown: %v", err)
		}
	}
	dgwCompatServer.Stop(ctx)

	logger.Println("[SERVER] Keystone Edge stopped")
}
