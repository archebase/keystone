// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package e2conversion

import (
	"database/sql"
	"testing"
)

func TestValidateE2EpisodeAdmission(t *testing.T) {
	base := episodeAdmissionRow{
		IngestionChannel: "data_gateway",
		StorageBackend:   "keystone_tos",
		McapPath:         "device-uploads/robot/capture/upload/capture.tar",
		QAStatus:         QAApproved,
		DeviceType:       "Ego Portal E2",
	}
	if err := validateE2EpisodeAdmission(base); err != nil {
		t.Fatalf("valid E2 episode rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*episodeAdmissionRow)
	}{
		{"wrong device", func(row *episodeAdmissionRow) { row.DeviceType = "Ego Portal Stereo" }},
		{"wrong ingestion", func(row *episodeAdmissionRow) { row.IngestionChannel = "axon_transfer" }},
		{"wrong backend", func(row *episodeAdmissionRow) { row.StorageBackend = "minio" }},
		{"wrong suffix", func(row *episodeAdmissionRow) { row.McapPath = "capture.tar.gz" }},
		{"QA pending", func(row *episodeAdmissionRow) { row.QAStatus = QAPending }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row := base
			test.mutate(&row)
			if err := validateE2EpisodeAdmission(row); err == nil {
				t.Fatal("invalid E2 episode was accepted")
			}
		})
	}
}

func TestValidateE2EpisodeAdmissionDoesNotRequireChecksum(t *testing.T) {
	row := episodeAdmissionRow{
		IngestionChannel: "data_gateway",
		StorageBackend:   "keystone_tos",
		McapPath:         "capture.TAR",
		QAStatus:         QAApproved,
		DeviceType:       "Ego Portal E2",
		Metadata:         sql.NullString{Valid: false},
	}
	if err := validateE2EpisodeAdmission(row); err != nil {
		t.Fatalf("E2 admission incorrectly required checksum/metadata: %v", err)
	}
}
