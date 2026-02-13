// database/mysql.go - MySQL database connection wrapper
package database

import (
	"database/sql"
	"fmt"
	"log"
	"time"

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
	ConnMaxLifetime int // seconds
}

// Connect creates a database connection
func Connect(cfg *Config) (*DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)

	// Verify connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("[DATABASE] Connected to MySQL successfully")
	return &DB{DB: db}, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	log.Println("[DATABASE] Closing database connection")
	return db.DB.Close()
}
