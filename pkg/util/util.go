// util/util.go - Utility functions
package util

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureDir ensures directory exists
func EnsureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// FileExists checks if file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// GetDiskUsage gets disk usage
func GetDiskUsage(path string) (DiskUsage, error) {
	_ = path

	// TODO: Implement proper disk usage check using syscall.Statfs
	// Simplified implementation, returns mock data
	return DiskUsage{
		Path:    path,
		Total:   500 * 1024 * 1024 * 1024, // 500GB
		Used:    100 * 1024 * 1024 * 1024, // 100GB
		Free:    400 * 1024 * 1024 * 1024, // 400GB
		UsedPercent: 20,
	}, nil
}

// DiskUsage disk usage information
type DiskUsage struct {
	Path       string
	Total      uint64
	Used       uint64
	Free       uint64
	UsedPercent int
}

func (du DiskUsage) String() string {
	return fmt.Sprintf("%s: %d%% used", du.Path, du.UsedPercent)
}

// EnsurePath joins path and ensures directory exists
func EnsurePath(base, rel string) (string, error) {
	path := filepath.Join(base, rel)
	if err := EnsureDir(path); err != nil {
		return "", err
	}
	return path, nil
}
