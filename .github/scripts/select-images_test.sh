#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
selector="$script_dir/select-images.sh"

run_case() {
  local name="$1"
  local expected_keystone="$2"
  local expected_stereo_split="$3"
  local selection_output
  shift 3

  selection_output="$(mktemp)"
  GITHUB_OUTPUT="$selection_output" bash "$selector" "$@" >/dev/null

  if [[ "$(wc -l < "$selection_output")" -ne 2 ]] \
    || ! grep -Fxq "keystone=$expected_keystone" "$selection_output" \
    || ! grep -Fxq "stereo_split=$expected_stereo_split" "$selection_output"; then
    echo "$name: unexpected selection output" >&2
    sed 's/^/  /' "$selection_output" >&2
    rm -f "$selection_output"
    return 1
  fi

  rm -f "$selection_output"
}

run_case "manual Keystone" true false workflow_dispatch keystone
run_case "manual stereo-split" false true workflow_dispatch stereo-split
run_case "manual all" true true workflow_dispatch all
run_case "ordinary push" true false push "" internal/server/server.go
run_case "stereo-split push" false true push "" jobs/stereo-split/Dockerfile
run_case "mixed push" true true push "" internal/server/server.go jobs/stereo-split/Dockerfile
run_case "empty push" true false push ""

invalid_output="$(mktemp)"
if GITHUB_OUTPUT="$invalid_output" bash "$selector" workflow_dispatch invalid >/dev/null 2>&1; then
  echo "invalid manual selection unexpectedly succeeded" >&2
  rm -f "$invalid_output"
  exit 1
fi
rm -f "$invalid_output"

echo "image publication selection tests passed"
