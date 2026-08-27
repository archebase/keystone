#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Orbit entrypoint for the E2 multimodal conversion job."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys


def now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def identity(path: Path) -> dict[str, object]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        while chunk := stream.read(8 * 1024 * 1024):
            digest.update(chunk)
            size += len(chunk)
    return {"name": path.name, "size_bytes": size, "sha256": digest.hexdigest()}


def safe_extract(archive: Path, destination: Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    script = """
import os, tarfile, sys
archive, destination = sys.argv[1:]
with tarfile.open(archive, 'r:*') as tar:
    members = tar.getmembers()
    if len(members) > 10000:
        raise RuntimeError('tar contains too many members')
    total = 0
    for member in members:
        name = member.name
        if member.islnk() or member.issym() or not (member.isfile() or member.isdir()):
            raise RuntimeError('tar contains an unsafe member: ' + name)
        target = os.path.realpath(os.path.join(destination, name))
        if os.path.commonpath((os.path.realpath(destination), target)) != os.path.realpath(destination):
            raise RuntimeError('tar member escapes extraction directory: ' + name)
        if member.isfile():
            if member.size > 50 * 1024**3:
                raise RuntimeError('tar member exceeds size limit: ' + name)
            total += member.size
            if total > 60 * 1024**3:
                raise RuntimeError('tar contents exceed size limit')
    tar.extractall(destination, filter='data')
"""
    try:
        subprocess.run([sys.executable, "-c", script, str(archive), str(destination)], check=True)
    except subprocess.CalledProcessError as error:
        raise RuntimeError(f"tar extraction failed: {archive.name}") from error


def find_root(extracted: Path) -> Path:
    required = ("Camera0/video.mp4", "Camera1/video.mp4", "Sensors/accel.csv", "Sensors/gyro.csv")
    candidates = [extracted, *[path for path in extracted.iterdir() if path.is_dir()]]
    for candidate in candidates:
        if all((candidate / name).is_file() for name in required):
            return candidate
    raise RuntimeError("tar must contain E2 files at its root or under one wrapper directory")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output-binding", type=Path, required=True)
    parser.add_argument("--scratch", type=Path, required=True)
    parser.add_argument("--expected-source-size", type=int, required=True)
    parser.add_argument("--expected-source-checksum", default="")
    parser.add_argument("--source-uri", required=True)
    parser.add_argument("--processor-image", required=True)
    parser.add_argument("--generation", type=int, required=True)
    args = parser.parse_args()
    try:
        from e2_converter import convert

        source = args.input.resolve()
        if source.stat().st_size != args.expected_source_size:
            raise RuntimeError("source size mismatch")
        source_identity = identity(source)
        if args.expected_source_checksum and source_identity["sha256"] != args.expected_source_checksum.lower():
            raise RuntimeError("source checksum mismatch")
        work = args.scratch.resolve()
        extracted = work / "extracted"
        safe_extract(source, extracted)
        result = convert(
            find_root(extracted), work / "outputs", args.source_uri,
            args.expected_source_size, args.generation, args.processor_image
        )

        outputs = work / "outputs"
        manifest = {
            "schema_version": 1, "kind": "e2_multimodal_conversion", "status": "succeeded",
            "output_format": "h264_ros2_mcap", "nominal_fps": result["nominal_fps"],
            "ros_distribution": "humble",
            "source": {"uri": args.source_uri, "size_bytes": args.expected_source_size},
            "outputs": {"mcap": identity(outputs / "output_bag.mcap"),
                        "metadata": identity(outputs / "metadata.yaml"),
                        "calibration": identity(outputs / "calibration.json")},
            "calibration": {
                "schema": result["calibration_schema"],
                "source_files": [
                    "Camera0/camera_params.json",
                    "Camera1/camera_params.json",
                    "Sensors/imu_calibration.json",
                ],
            },
            **result, "started_at": now(), "finished_at": now(),
        }
        args.output_binding.mkdir(parents=True, exist_ok=True)
        for name in ("output_bag.mcap", "metadata.yaml", "calibration.json"):
            shutil.copyfile(outputs / name, args.output_binding / name)
        (args.output_binding / "processing_manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        print(json.dumps(manifest, separators=(",", ":"), sort_keys=True))
        return 0
    except Exception as error:
        print(f"E2 conversion failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
