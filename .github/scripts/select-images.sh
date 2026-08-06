#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

event_name="${1:?event name is required}"
manual_image="${2:-}"
shift 2

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

publish_keystone=false
publish_stereo_split=false
publish_calibration=false

if [[ "$event_name" == "workflow_dispatch" ]]; then
  case "$manual_image" in
    keystone)
      publish_keystone=true
      ;;
    stereo-split)
      publish_stereo_split=true
      ;;
    calibration)
      publish_calibration=true
      ;;
    all)
      publish_keystone=true
      publish_stereo_split=true
      publish_calibration=true
      ;;
    *)
      echo "unsupported image selection: $manual_image" >&2
      exit 1
      ;;
  esac
else
  for path in "$@"; do
    case "$path" in
      jobs/stereo-split/split_mcap_stereo_imu.py)
        publish_stereo_split=true
        publish_calibration=true
        ;;
      jobs/stereo-split/*)
        publish_stereo_split=true
        ;;
      jobs/calibration/*)
        publish_calibration=true
        ;;
      *)
        publish_keystone=true
        ;;
    esac
  done

  # Preserve the existing Keystone publication behavior for empty pushes.
  if [[ "$publish_keystone" == "false" \
    && "$publish_stereo_split" == "false" \
    && "$publish_calibration" == "false" ]]; then
    publish_keystone=true
  fi
fi

echo "keystone=$publish_keystone" >> "$GITHUB_OUTPUT"
echo "stereo_split=$publish_stereo_split" >> "$GITHUB_OUTPUT"
echo "calibration=$publish_calibration" >> "$GITHUB_OUTPUT"
echo "Publish Keystone: $publish_keystone"
echo "Publish stereo-split: $publish_stereo_split"
echo "Publish calibration: $publish_calibration"
