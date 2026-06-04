// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"database/sql"
	"strings"
	"testing"
)

func TestBuildDPDirectRawTags_MergesInDocumentedOrder(t *testing.T) {
	got, err := buildDPDirectRawTags(dpRawTagsInput{
		Profile: DPDeviceProfile{
			DeviceID: "asset-1",
			Tags: map[string]string{
				"profile": "tag",
				"same":    "value",
			},
		},
		McapKey: "edge-factory/factory/device/task.mcap",
		SidecarTags: map[string]string{
			"same":        "value",
			"array_field": `["a","b"]`,
			"empty_value": "",
		},
		EpisodeID:       42,
		EpisodePublicID: "episode-public-42",
		TaskID:          77,
		FactoryID:       sql.NullInt64{Int64: 3, Valid: true},
		OrganizationID:  sql.NullInt64{Int64: 9, Valid: true},
	})
	if err != nil {
		t.Fatalf("buildDPDirectRawTags() error = %v", err)
	}

	cases := map[string]string{
		"profile":                "tag",
		"same":                   "value",
		dpReservedDeviceIDTagKey: "asset-1",
		dpReservedRawFileTagKey:  "task.mcap",
		"array_field":            `["a","b"]`,
		"empty_value":            "",
		"episode_id":             "episode-public-42",
		"keystone_episode_id":    "42",
		"sync_channel":           "keystone_direct",
		"task_id":                "77",
		"factory_id":             "3",
		"organization_id":        "9",
	}
	for key, want := range cases {
		if got[key] != want {
			t.Fatalf("tag[%q]=%q want %q tags=%+v", key, got[key], want, got)
		}
	}
	if _, ok := got["device_id"]; ok {
		t.Fatalf("ordinary device_id raw tag must not be injected: %+v", got)
	}
}

func TestBuildDPDirectRawTags_UsesMcapKeyBasenameNotSidecarMcapFile(t *testing.T) {
	got, err := buildDPDirectRawTags(dpRawTagsInput{
		Profile: DPDeviceProfile{
			DeviceID: "asset-1",
			Tags:     map[string]string{"profile": "tag"},
		},
		McapKey: "bucket/minio/path/actual.mcap",
		SidecarTags: map[string]string{
			"mcap_file": "sidecar-claimed.mcap",
		},
		EpisodeID:       1,
		EpisodePublicID: "episode-1",
	})
	if err != nil {
		t.Fatalf("buildDPDirectRawTags() error = %v", err)
	}
	if got[dpReservedRawFileTagKey] != "actual.mcap" {
		t.Fatalf("raw_file=%q want actual.mcap", got[dpReservedRawFileTagKey])
	}
	if got["mcap_file"] != "sidecar-claimed.mcap" {
		t.Fatalf("sidecar mcap_file should remain ordinary sidecar tag: %+v", got)
	}
}

func TestBuildDPDirectRawTags_ConflictingTagsFail(t *testing.T) {
	tests := []struct {
		name  string
		input dpRawTagsInput
	}{
		{
			name: "profile conflicts with reserved device id",
			input: dpRawTagsInput{
				Profile: DPDeviceProfile{
					DeviceID: "asset-1",
					Tags:     map[string]string{dpReservedDeviceIDTagKey: "other-device"},
				},
				McapKey:         "bucket/file.mcap",
				EpisodeID:       1,
				EpisodePublicID: "episode-1",
			},
		},
		{
			name: "sidecar conflicts with profile",
			input: dpRawTagsInput{
				Profile: DPDeviceProfile{
					DeviceID: "asset-1",
					Tags:     map[string]string{"scene": "profile"},
				},
				McapKey:         "bucket/file.mcap",
				SidecarTags:     map[string]string{"scene": "sidecar"},
				EpisodeID:       1,
				EpisodePublicID: "episode-1",
			},
		},
		{
			name: "sidecar conflicts with keystone extra",
			input: dpRawTagsInput{
				Profile: DPDeviceProfile{
					DeviceID: "asset-1",
					Tags:     map[string]string{"profile": "tag"},
				},
				McapKey:         "bucket/file.mcap",
				SidecarTags:     map[string]string{"sync_channel": "other"},
				EpisodeID:       1,
				EpisodePublicID: "episode-1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildDPDirectRawTags(tt.input); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("error=%v want conflict", err)
			}
		})
	}
}

func TestBuildDPDirectRawTags_RejectsEmptyKeyAndRawFile(t *testing.T) {
	_, err := buildDPDirectRawTags(dpRawTagsInput{
		Profile: DPDeviceProfile{
			DeviceID: "asset-1",
			Tags:     map[string]string{"": "value"},
		},
		McapKey:         "bucket/file.mcap",
		EpisodeID:       1,
		EpisodePublicID: "episode-1",
	})
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("empty key error=%v", err)
	}

	_, err = buildDPDirectRawTags(dpRawTagsInput{
		Profile: DPDeviceProfile{
			DeviceID: "asset-1",
			Tags:     map[string]string{"profile": "tag"},
		},
		McapKey:         "bucket/",
		EpisodeID:       1,
		EpisodePublicID: "episode-1",
	})
	if err == nil || !strings.Contains(err.Error(), "raw_file") {
		t.Fatalf("empty raw_file error=%v", err)
	}
}
