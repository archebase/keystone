// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package server

import (
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
)

func TestAxonTransferWriteTimeoutFromConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.TransferConfig
		want time.Duration
	}{
		{name: "nil config", cfg: nil, want: services.DefaultTransferWriteTimeout},
		{name: "zero config", cfg: &config.TransferConfig{}, want: services.DefaultTransferWriteTimeout},
		{name: "custom seconds", cfg: &config.TransferConfig{WriteTimeout: 7}, want: 7 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := axonTransferWriteTimeout(tt.cfg); got != tt.want {
				t.Fatalf("axonTransferWriteTimeout()=%s want=%s", got, tt.want)
			}
		})
	}
}
