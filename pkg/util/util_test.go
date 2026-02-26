// util/util_test.go - Utility functions tests
package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDir(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "Create single level directory",
			path:    filepath.Join(os.TempDir(), "keystone-test-single"),
			wantErr: false,
		},
		{
			name:    "Create nested directory",
			path:    filepath.Join(os.TempDir(), "keystone-test", "nested", "dir"),
			wantErr: false,
		},
		{
			name:    "Create existing directory",
			path:    os.TempDir(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Cleanup
			defer os.RemoveAll(tt.path)

			err := EnsureDir(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureDir() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify directory exists
			if info, err := os.Stat(tt.path); err != nil {
				t.Errorf("EnsureDir() directory does not exist: %v", err)
			} else if !info.IsDir() {
				t.Errorf("EnsureDir() path is not a directory")
			}
		})
	}
}

func TestFileExists(t *testing.T) {
	// Create temporary file
	tmpFile, err := os.CreateTemp("", "keystone-test-*")
	if err != nil {
		t.Fatalf("CreateTemp error: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "Existing file",
			path: tmpFile.Name(),
			want: true,
		},
		{
			name: "Non-existent file",
			path: "/tmp/nonexistent-file-12345",
			want: false,
		},
		{
			name: "Existing directory",
			path: os.TempDir(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileExists(tt.path); got != tt.want {
				t.Errorf("FileExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsurePath(t *testing.T) {
	baseDir := filepath.Join(os.TempDir(), "keystone-ensure-path")
	defer os.RemoveAll(baseDir)

	tests := []struct {
		name    string
		base    string
		rel     string
		wantErr bool
	}{
		{
			name:    "Create relative path",
			base:    baseDir,
			rel:     "subdir",
			wantErr: false,
		},
		{
			name:    "Create nested path",
			base:    baseDir,
			rel:     "a/b/c",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EnsurePath(tt.base, tt.rel)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsurePath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			expected := filepath.Join(tt.base, tt.rel)
			if got != expected {
				t.Errorf("EnsurePath() = %v, want %v", got, expected)
			}

			// Verify directory exists
			if info, err := os.Stat(got); err != nil {
				t.Errorf("EnsurePath() directory does not exist: %v", err)
			} else if !info.IsDir() {
				t.Errorf("EnsurePath() path is not a directory")
			}
		})
	}
}

func TestDiskUsage(t *testing.T) {
	// Note: GetDiskUsage currently uses mock data
	// Actual implementation should use syscall.Statfs
	du, err := GetDiskUsage("/")
	if err != nil {
		t.Errorf("GetDiskUsage() error = %v", err)
		return
	}

	if du.Path == "" {
		t.Error("GetDiskUsage() Path is empty")
	}

	if du.Total == 0 {
		t.Error("GetDiskUsage() Total is 0")
	}

	if du.Used > du.Total {
		t.Errorf("GetDiskUsage() Used (%d) > Total (%d)", du.Used, du.Total)
	}

	if du.UsedPercent < 0 || du.UsedPercent > 100 {
		t.Errorf("GetDiskUsage() UsedPercent = %d, should be 0-100", du.UsedPercent)
	}
}

func TestDiskUsageString(t *testing.T) {
	du := DiskUsage{
		Path:        "/var/lib/keystone",
		Total:       500 * 1024 * 1024 * 1024,
		Used:        100 * 1024 * 1024 * 1024,
		Free:        400 * 1024 * 1024 * 1024,
		UsedPercent: 20,
	}

	str := du.String()
	if str == "" {
		t.Error("DiskUsage.String() returned empty string")
	}

	expected := "/var/lib/keystone: 20% used"
	if str != expected {
		t.Errorf("DiskUsage.String() = %v, want %v", str, expected)
	}
}
