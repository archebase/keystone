// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"testing"
)

const testSidecarJSON = `{
  "device": {
    "device_id": "robot_01",
    "hostname": "sh01-dc54",
    "ros_distro": "jazzy"
  },
  "mcap_file": "task_20260414_054145_205_27_fdd80b4c.mcap",
  "recording": {
    "checksum_sha256": "e234b514",
    "duration_sec": 121.966222012,
    "file_size_bytes": 147960982,
    "message_count": 222251,
    "recorder_version": "0.3.1",
    "recording_finished_at": "2026-04-14T09:17:07.825Z",
    "recording_started_at": "2026-04-14T09:15:05.858Z",
    "topics_recorded": [
      "/hal/camera/head/color/camera_info",
      "/system/info"
    ]
  },
  "task": {
    "data_collector_id": "刘备",
    "factory": "Shanghai Factory",
    "order_id": "采集",
    "scene": "卧室",
    "skills": ["pick"],
    "subscene": "床",
    "task_id": "task_20260414_054145_205_27_fdd80b4c"
  },
  "topics_summary": [
    {
      "frequency_hz": 0.0,
      "message_count": 394,
      "topic": "/calibration/glove_calibration/left_adc"
    }
  ],
  "version": "1.0"
}`

func TestFlattenSidecar_BasicFields(t *testing.T) {
	tags, err := flattenSidecar([]byte(testSidecarJSON))
	if err != nil {
		t.Fatalf("flattenSidecar failed: %v", err)
	}

	cases := map[string]string{
		"device.device_id":                "robot_01",
		"device.hostname":                 "sh01-dc54",
		"device.ros_distro":               "jazzy",
		"mcap_file":                       "task_20260414_054145_205_27_fdd80b4c.mcap",
		"recording.checksum_sha256":       "e234b514",
		"recording.recorder_version":      "0.3.1",
		"recording.file_size_bytes":       "147960982",
		"recording.message_count":         "222251",
		"recording.duration_sec":          "121.966222012",
		"recording.recording_started_at":  "2026-04-14T09:15:05.858Z",
		"recording.recording_finished_at": "2026-04-14T09:17:07.825Z",
		"task.data_collector_id":          "刘备",
		"task.factory":                    "Shanghai Factory",
		"task.scene":                      "卧室",
		"task.subscene":                   "床",
		"task.task_id":                    "task_20260414_054145_205_27_fdd80b4c",
		"version":                         "1.0",
	}

	for key, want := range cases {
		got, ok := tags[key]
		if !ok {
			t.Errorf("key %q missing from tags", key)
			continue
		}
		if got != want {
			t.Errorf("tags[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestFlattenSidecar_ArraysEncodedAsJSONString(t *testing.T) {
	tags, err := flattenSidecar([]byte(testSidecarJSON))
	if err != nil {
		t.Fatalf("flattenSidecar failed: %v", err)
	}

	cases := map[string]string{
		"recording.topics_recorded": `["/hal/camera/head/color/camera_info","/system/info"]`,
		"task.skills":               `["pick"]`,
	}

	for key, want := range cases {
		got, ok := tags[key]
		if !ok {
			t.Errorf("key %q missing from tags", key)
			continue
		}
		if got != want {
			t.Errorf("tags[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestFlattenSidecar_TopicsSummaryExcluded(t *testing.T) {
	tags, err := flattenSidecar([]byte(testSidecarJSON))
	if err != nil {
		t.Fatalf("flattenSidecar failed: %v", err)
	}

	for key := range tags {
		if len(key) >= 14 && key[:14] == "topics_summary" {
			t.Errorf("topics_summary should be excluded, but found key %q", key)
		}
	}
}

func TestFlattenSidecar_InvalidJSON(t *testing.T) {
	_, err := flattenSidecar([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestFlattenSidecar_EmptyObject(t *testing.T) {
	tags, err := flattenSidecar([]byte(`{}`))
	if err != nil {
		t.Fatalf("flattenSidecar failed: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected empty tags for empty object, got %v", tags)
	}
}
