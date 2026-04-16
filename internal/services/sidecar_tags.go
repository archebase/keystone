// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// flattenSidecar parses sidecar JSON bytes and returns a flat map of string key-value pairs
// suitable for use as RawTags in an upload request.
//
// Nested objects are flattened with dot notation (e.g. "device.device_id").
// Array values are JSON-encoded into a single string under one key (e.g. task.skills -> ["pick"]).
// The "topics_summary" key is intentionally excluded.
func flattenSidecar(data []byte) (map[string]string, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse sidecar json: %w", err)
	}

	result := make(map[string]string)
	flattenValue(result, "", raw)
	return result, nil
}

func flattenValue(out map[string]string, prefix string, v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, child := range val {
			if prefix == "" && k == "topics_summary" {
				continue
			}
			flattenValue(out, joinKey(prefix, k), child)
		}
	case []interface{}:
		b, err := json.Marshal(val)
		if err != nil {
			out[prefix] = fmt.Sprintf("%v", val)
			return
		}
		out[prefix] = string(b)
	case nil:
		out[prefix] = ""
	case bool:
		if val {
			out[prefix] = "true"
		} else {
			out[prefix] = "false"
		}
	case float64:
		out[prefix] = strconv.FormatFloat(val, 'f', -1, 64)
	case string:
		out[prefix] = val
	default:
		out[prefix] = fmt.Sprintf("%v", val)
	}
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
