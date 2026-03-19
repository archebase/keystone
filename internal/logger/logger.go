// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package logger provides a centralized logger for the Keystone Edge application
package logger

import (
	"io"
	"log"
	"os"
	"sync"
)

var (
	// defaultLogger is the package-level logger instance
	defaultLogger *log.Logger
	once          sync.Once
	mu            sync.RWMutex
)

// Options holds logger configuration
type Options struct {
	Prefix string
	Flags  int
}

// DefaultOptions returns the default logger options
func DefaultOptions() Options {
	return Options{
		Prefix: "[KEYSTONE-EDGE] ",
		Flags:  log.LstdFlags,
	}
}

// Init initializes the default logger with the given options
func Init(opts Options) {
	once.Do(func() {
		defaultLogger = log.New(os.Stdout, opts.Prefix, opts.Flags)
	})
}

// InitWithWriter initializes the default logger with a custom writer
func InitWithWriter(w io.Writer, opts Options) {
	once.Do(func() {
		defaultLogger = log.New(w, opts.Prefix, opts.Flags)
	})
}

// MustInit initializes the logger and panics on error
func MustInit(opts Options) {
	once.Do(func() {
		defaultLogger = log.New(os.Stdout, opts.Prefix, opts.Flags)
		if defaultLogger == nil {
			panic("failed to initialize logger")
		}
	})
}

// Set sets the default logger (for testing or custom initialization)
func Set(logger *log.Logger) {
	mu.Lock()
	defer mu.Unlock()
	defaultLogger = logger
}

// Get returns the default logger
func Get() *log.Logger {
	mu.RLock()
	defer mu.RUnlock()
	if defaultLogger == nil {
		// Return a default logger if not initialized
		return log.New(os.Stdout, "", log.LstdFlags)
	}
	return defaultLogger
}

// Print calls the default logger's Print
func Print(v ...interface{}) {
	Get().Print(v...)
}

// Printf calls the default logger's Printf
func Printf(format string, v ...interface{}) {
	Get().Printf(format, v...)
}

// Println calls the default logger's Println
func Println(v ...interface{}) {
	Get().Println(v...)
}

// Fatal calls the default logger's Fatal and exits
func Fatal(v ...interface{}) {
	Get().Fatal(v...)
}

// Fatalf calls the default logger's Fatalf and exits
func Fatalf(format string, v ...interface{}) {
	Get().Fatalf(format, v...)
}

// Fatalln calls the default logger's Fatalln and exits
func Fatalln(v ...interface{}) {
	Get().Fatalln(v...)
}

// Panic calls the default logger's Panic and panics
func Panic(v ...interface{}) {
	Get().Panic(v...)
}

// Panicf calls the default logger's Panicf and panics
func Panicf(format string, v ...interface{}) {
	Get().Panicf(format, v...)
}

// Panicln calls the default logger's Panicln and panics
func Panicln(v ...interface{}) {
	Get().Panicln(v...)
}
