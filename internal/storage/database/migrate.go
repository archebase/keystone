// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package database provides database connection and migration management
package database

import (
	"embed"
	"errors"
	"fmt"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jmoiron/sqlx"

	// MySQL driver is required for database/sql to work with MySQL
	_ "github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs all pending database migrations
func Migrate(db *sqlx.DB) error {
	logger.Println("[DATABASE] Running database migrations...")

	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to create migration source: %w", err)
	}

	driver, err := mysql.WithInstance(db.DB, &mysql.Config{
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "mysql", driver)
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	// Enable verbose logging for debugging
	m.Log = &logWriter{}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logger.Println("[DATABASE] No pending migrations")
			return nil
		}
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}

	logger.Printf("[DATABASE] Migrations applied successfully (version=%d, dirty=%v)", version, dirty)
	return nil
}

// logWriter implements golang-migrate's Logger interface
type logWriter struct{}

func (l *logWriter) Printf(format string, v ...interface{}) {
	logger.Printf("[DATABASE] "+format, v...)
}

func (l *logWriter) Verbose() bool {
	return true
}
