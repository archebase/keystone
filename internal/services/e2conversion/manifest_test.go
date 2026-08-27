// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package e2conversion

import (
	"testing"
	"time"
)

var fixedManifestTime = time.Unix(1_700_000_000, 0).UTC()

func validManifest() processingManifest {
	var manifest processingManifest
	manifest.SchemaVersion = 1
	manifest.Status = "succeeded"
	manifest.Kind = Kind
	manifest.OutputFormat = e2OutputFormat
	manifest.Generation = 2
	manifest.ProcessorImage = "registry.example/e2@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest.Source.URI = "tos://bucket/capture.tar"
	manifest.Source.SizeBytes = 100
	manifest.Outputs.MCAP.Name = outputMcapName
	manifest.Outputs.MCAP.SizeBytes = 200
	manifest.Outputs.MCAP.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	manifest.Outputs.Metadata.Name = outputMetadataName
	manifest.Outputs.Metadata.SizeBytes = 20
	manifest.Outputs.Metadata.SHA256 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	manifest.Outputs.Calibration.Name = outputCalibrationName
	manifest.Outputs.Calibration.SizeBytes = 30
	manifest.Outputs.Calibration.SHA256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	manifest.Stats.LeftVideos = 10
	manifest.Stats.RightVideos = 10
	manifest.Stats.IMUMessages = 100
	manifest.StartedAt = fixedManifestTime
	manifest.FinishedAt = fixedManifestTime.Add(1)
	return manifest
}

func TestValidateE2ManifestStats(t *testing.T) {
	manifest := validManifest()
	contract, err := validateManifestStats(manifest)
	if err != nil {
		t.Fatalf("validateManifestStats() error = %v", err)
	}
	if contract.LeftTopic != leftVideoTopic || contract.RightTopic != rightVideoTopic ||
		contract.ExpectedLeft != 10 || contract.ExpectedRight != 10 || contract.ExpectedIMU != 100 {
		t.Fatalf("contract = %+v", contract)
	}
}

func TestValidateE2ManifestStatsRejectsLegacyFormat(t *testing.T) {
	manifest := validManifest()
	manifest.OutputFormat = "stereo_h264"
	if _, err := validateManifestStats(manifest); err == nil {
		t.Fatal("legacy stereo format was accepted")
	}
}

func TestValidateE2ManifestStatsRejectsMismatchedVideoCounts(t *testing.T) {
	manifest := validManifest()
	manifest.Stats.RightVideos++
	if _, err := validateManifestStats(manifest); err == nil {
		t.Fatal("mismatched video counts were accepted")
	}
}
