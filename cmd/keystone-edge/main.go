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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/cloud"
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

	closeLog := initLoggerFromEnv()
	defer closeLog()

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

	// Initialize S3/MinIO independently from TOS. TOS-only deployments may use
	// the default Volcengine credential chain and leave MinIO credentials empty.
	var s3Client *s3.Client
	if cfg.Storage.AccessKey != "" && cfg.Storage.SecretKey != "" {
		s3Client, err = s3.Connect(&s3.Config{
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
	} else {
		logger.Printf("[STORAGE] S3/MinIO client disabled: credentials not configured")
	}

	// TODO: Start QA worker

	// Initialize cloud sync worker
	var syncWorker *services.SyncWorker
	var minioSourceReader services.SourceObjectReader
	if s3Client != nil {
		minioSourceReader = services.NewMinioSourceObjectReader(s3Client)
	}
	var tosSourceReader services.SourceObjectReader
	if cfg.TOSStorage.Type == "tos" {
		tosSourceReader = tosstorage.NewReader(cfg.TOSStorage, time.Duration(cfg.Sync.OSSTimeoutSec)*time.Second)
	}
	if cfg.Sync.Enabled && cfg.Hilbert.BaseURL != "" && cfg.Hilbert.AccessKey != "" && cfg.Hilbert.SecretKey != "" &&
		(minioSourceReader != nil || tosSourceReader != nil) {
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
		syncWorker.SetSourceObjectReader(minioSourceReader)
		syncWorker.SetTOSSourceObjectReader(cfg.TOSStorage.Bucket, tosSourceReader)
		syncWorker.SetTOSObjectUploader(cloud.NewTOSS3Uploader(
			time.Duration(cfg.Sync.OSSTimeoutSec)*time.Second,
			cfg.Server.Mode,
		))

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

func initLoggerFromEnv() func() {
	output := strings.TrimSpace(os.Getenv("KEYSTONE_LOG_OUTPUT"))
	switch strings.ToLower(output) {
	case "", "stdout":
		logger.InitWithWriter(os.Stdout, logger.DefaultOptions())
		return func() {}
	case "stderr":
		logger.InitWithWriter(os.Stderr, logger.DefaultOptions())
		return func() {}
	}

	if strings.HasSuffix(output, string(os.PathSeparator)) {
		output = filepath.Join(output, "keystone-edge.log")
	}
	if info, err := os.Stat(output); err == nil && info.IsDir() { // #nosec G703 -- log destination is an operator-controlled environment setting.
		output = filepath.Join(output, "keystone-edge.log")
	}
	logFile, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) // #nosec G304,G703 -- log destination is an operator-controlled write-only path.
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open log output %q: %v\n", output, err)
		os.Exit(1)
	}
	logger.InitWithWriter(logFile, logger.DefaultOptions())
	return func() {
		_ = logFile.Close()
	}
}
