// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"encoding/json"
)

// optionalJSONPatch supports PATCH-style updates where omitted, null, and value are distinct.
type optionalJSONPatch struct {
	Present bool
	Value   interface{}
}

func (o *optionalJSONPatch) UnmarshalJSON(data []byte) error {
	o.Present = true
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		o.Value = nil
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}
