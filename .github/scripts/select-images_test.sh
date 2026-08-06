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
  local expected_calibration="$4"
  local selection_output
  shift 4

  selection_output="$(mktemp)"
  GITHUB_OUTPUT="$selection_output" bash "$selector" "$@" >/dev/null

  if [[ "$(wc -l < "$selection_output")" -ne 3 ]] \
    || ! grep -Fxq "keystone=$expected_keystone" "$selection_output" \
    || ! grep -Fxq "stereo_split=$expected_stereo_split" "$selection_output" \
    || ! grep -Fxq "calibration=$expected_calibration" "$selection_output"; then
    echo "$name: unexpected selection output" >&2
    sed 's/^/  /' "$selection_output" >&2
    rm -f "$selection_output"
    return 1
  fi

  rm -f "$selection_output"
}

run_case "manual Keystone" true false false workflow_dispatch keystone
run_case "manual stereo-split" false true false workflow_dispatch stereo-split
run_case "manual calibration" false false true workflow_dispatch calibration
run_case "manual all" true true true workflow_dispatch all
run_case "ordinary push" true false false push "" internal/server/server.go
run_case "stereo-split push" false true false push "" jobs/stereo-split/Dockerfile
run_case "calibration push" false false true push "" jobs/calibration/Dockerfile
run_case "shared splitter push" false true true push "" jobs/stereo-split/split_mcap_stereo_imu.py
run_case "mixed push" true true true push "" \
  internal/server/server.go \
  jobs/stereo-split/Dockerfile \
  jobs/calibration/Dockerfile
run_case "empty push" true false false push ""

invalid_output="$(mktemp)"
if GITHUB_OUTPUT="$invalid_output" bash "$selector" workflow_dispatch invalid >/dev/null 2>&1; then
  echo "invalid manual selection unexpectedly succeeded" >&2
  rm -f "$invalid_output"
  exit 1
fi
rm -f "$invalid_output"

echo "image publication selection tests passed"
