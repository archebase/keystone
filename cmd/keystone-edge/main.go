// main.go - Keystone Edge service entry point
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/server"
)

// @title			Keystone Edge API
// @version		1.0
// @description	Backend for edge data collection scenarios.
// @host			localhost:8080
// @BasePath		/api/v1

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// Load .env file
	godotenv.Load()

	// Command line flags
	showVersion := flag.Bool("version", false, "Show version information")
	configPath := flag.String("config", "/etc/keystone-edge/config.toml", "Configuration file path")
	flag.Parse()

	// Show version
	if *showVersion {
		fmt.Printf("Keystone Edge %s (built: %s)\n", version, buildTime)
		os.Exit(0)
	}

	// Initialize logger
	logger := log.New(os.Stdout, "[KEYSTONE-EDGE] ", log.LstdFlags|log.Lshortfile)
	logger.Printf("Starting Keystone Edge %s", version)
	logger.Printf("Config file: %s", *configPath)

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("Failed to load config: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		logger.Fatalf("Invalid config: %v", err)
	}

	logger.Printf("Config loaded: mode=%s, bind=%s", cfg.Server.Mode, cfg.Server.BindAddr)

	// Initialize and start HTTP server
	srv := server.New(cfg)
	if err := srv.Start(); err != nil {
		logger.Fatalf("Failed to start server: %v", err)
	}

	// TODO: Initialize database connection
	// TODO: Initialize storage layer
	// TODO: Start QA worker
	// TODO: Start sync worker

	logger.Println("Keystone Edge started successfully")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Printf("Error during shutdown: %v", err)
	}

	logger.Println("Keystone Edge stopped")
}
