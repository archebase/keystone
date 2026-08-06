#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Prepare a DECXIN joined-image MCAP for the ArcheBase calibration CLI."""

from __future__ import annotations

from dataclasses import asdict
from pathlib import Path
import sys


JOB_ROOT = Path(__file__).resolve().parent
SHARED_STEREO_SPLIT_ROOT = JOB_ROOT.parent / "stereo-split"
if SHARED_STEREO_SPLIT_ROOT.is_dir():
    sys.path.insert(0, str(SHARED_STEREO_SPLIT_ROOT))
from split_mcap_stereo_imu import (  # noqa: E402
    DecxinMcapStereoImuSplitter,
    SplitInputRejected,
)
if SHARED_STEREO_SPLIT_ROOT.is_dir():
    sys.path.remove(str(SHARED_STEREO_SPLIT_ROOT))


class PreprocessingRejected(RuntimeError):
    """The source capture failed a preprocessing quality requirement."""


def preprocess_decxin(source: Path, output_directory: Path) -> tuple[Path, dict[str, int]]:
    """Split joined JPEG frames and embedded IMU samples into a temporary MCAP."""
    source = source.expanduser().resolve()
    output_directory = output_directory.expanduser().resolve()
    try:
        stats = DecxinMcapStereoImuSplitter().convert(source, output_directory)
    except SplitInputRejected as error:
        raise PreprocessingRejected(str(error)) from error
    output_files = list(output_directory.glob("*.mcap"))
    if len(output_files) != 1 or not output_files[0].is_file():
        raise PreprocessingRejected("DECXIN preprocessing did not produce exactly one MCAP")
    if stats.decoded_images == 0 or stats.left_images != stats.right_images:
        raise PreprocessingRejected("DECXIN preprocessing did not produce a complete stereo sequence")
    if stats.imu_messages == 0:
        raise PreprocessingRejected("DECXIN preprocessing did not produce IMU samples")
    return output_files[0], asdict(stats)
