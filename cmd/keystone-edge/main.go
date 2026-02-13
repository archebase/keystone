// main.go - Keystone Edge service entry point
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"archebase.com/keystone-edge/internal/config"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
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

	// TODO: Initialize database connection
	// TODO: Initialize storage layer
	// TODO: Start API server
	// TODO: Start QA worker
	// TODO: Start sync worker

	logger.Println("Keystone Edge started successfully")

	// Block main thread
	select {}
}
