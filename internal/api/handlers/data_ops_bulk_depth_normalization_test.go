// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"testing"
)

func TestDepthNormalizationPreviewDecision(t *testing.T) {
	tests := []struct {
		name     string
		row      dataOpsDepthNormalizationPreviewRow
		want     string
		eligible bool
	}{
		{
			name: "eligible first generation",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType: sql.NullString{String: "ZJ-WA1-D", Valid: true},
			},
			eligible: true,
		},
		{
			name: "eligible failed retry",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType:       sql.NullString{String: "ZJ-WA1-D", Valid: true},
				ProcessingStatus: sql.NullString{String: "failed", Valid: true},
			},
			eligible: true,
		},
		{
			name: "wrong device",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType: sql.NullString{String: "Ego Portal Lite", Valid: true},
			},
			want: depthNormBulkReasonWrongDevice,
		},
		{
			name: "already normalized",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType:       sql.NullString{String: "ZJ-WA1-D", Valid: true},
				ProcessingStatus: sql.NullString{String: "succeeded", Valid: true},
			},
			want: depthNormBulkReasonAlreadyNormalized,
		},
		{
			name: "active",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType:       sql.NullString{String: "ZJ-WA1-D", Valid: true},
				ProcessingStatus: sql.NullString{String: "running", Valid: true},
			},
			want: depthNormBulkReasonProcessingActive,
		},
		{
			name: "sync evidence locks source",
			row: dataOpsDepthNormalizationPreviewRow{
				DeviceType:   sql.NullString{String: "ZJ-WA1-D", Valid: true},
				SyncEvidence: true,
			},
			want: depthNormBulkReasonCloudSourceLocked,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, eligible := depthNormalizationPreviewDecision(test.row)
			if eligible != test.eligible || got != test.want {
				t.Fatalf("decision = (%q, %t), want (%q, %t)", got, eligible, test.want, test.eligible)
			}
		})
	}
}
