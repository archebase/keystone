// Package database provides MySQL database connection wrapper
package database

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	// Register MySQL driver
	_ "github.com/go-sql-driver/mysql"
)

// DB MySQL database connection
type DB struct {
	*sql.DB
}

// Config database configuration
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime int           // seconds
	MaxRetries      int           // number of connection retry attempts
	RetryInterval   time.Duration // time between retries
}

// Connect creates a database connection with retry logic
func Connect(cfg *Config) (*DB, error) {
	// Apply defaults for retry configuration
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 10
	}
	retryInterval := cfg.RetryInterval
	if retryInterval <= 0 {
		retryInterval = 2 * time.Second
	}

	var db *sql.DB
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		db, err = sql.Open("mysql", cfg.DSN)
		if err != nil {
			log.Printf("[DATABASE] Attempt %d/%d: Failed to open database: %v", attempt, maxRetries, err)
			time.Sleep(retryInterval)
			continue
		}

		// Configure connection pool
		db.SetMaxOpenConns(cfg.MaxOpenConns)
		db.SetMaxIdleConns(cfg.MaxIdleConns)
		db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)

		// Verify connection
		if err := db.Ping(); err != nil {
			log.Printf("[DATABASE] Attempt %d/%d: Failed to ping database: %v", attempt, maxRetries, err)
			_ = db.Close() // ignore error
			time.Sleep(retryInterval)
			continue
		}

		log.Printf("[DATABASE] Connected to MySQL successfully after %d attempt(s)", attempt)
		return &DB{DB: db}, nil
	}

	return nil, fmt.Errorf("failed to connect to database after %d attempts: %w", maxRetries, err)
}

// Close closes the database connection
func (db *DB) Close() error {
	log.Println("[DATABASE] Closing database connection")
	return db.DB.Close()
}
