#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Produce a deterministic placeholder calibration result for one MCAP."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import sys
from typing import Iterable
import uuid


ALGORITHM_VERSION = "placeholder-v1"
COPY_BUFFER_BYTES = 8 * 1024 * 1024
MCAP_MAGIC = b"\x89MCAP0\r\n"


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def positive_integer(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("must be greater than zero")
    return parsed


def uuid_value(value: str) -> str:
    try:
        return str(uuid.UUID(value))
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be a UUID") from error


def inspect_mcap(path: Path) -> tuple[int, str]:
    if not path.is_file():
        raise RuntimeError(f"input MCAP does not exist: {path}")
    size_bytes = path.stat().st_size
    if size_bytes < len(MCAP_MAGIC) * 2:
        raise RuntimeError("input MCAP is too small")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        leading_magic = stream.read(len(MCAP_MAGIC))
        stream.seek(0)
        while chunk := stream.read(COPY_BUFFER_BYTES):
            digest.update(chunk)
        stream.seek(-len(MCAP_MAGIC), os.SEEK_END)
        trailing_magic = stream.read(len(MCAP_MAGIC))
    if leading_magic != MCAP_MAGIC or trailing_magic != MCAP_MAGIC:
        raise RuntimeError("input MCAP has invalid magic")
    return size_bytes, digest.hexdigest()


def write_result(path: Path, result: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        raise RuntimeError("result JSON already exists")
    encoded = (json.dumps(result, indent=2, sort_keys=True) + "\n").encode("utf-8")
    with path.open("xb") as stream:
        stream.write(encoded)
        stream.flush()
        os.fsync(stream.fileno())


def run(args: argparse.Namespace) -> dict[str, object]:
    started_at = utc_now()
    size_bytes, checksum = inspect_mcap(args.input.resolve())
    if size_bytes != args.expected_source_size:
        raise RuntimeError(
            f"source size mismatch: expected {args.expected_source_size}, got {size_bytes}"
        )
    if checksum != args.expected_source_checksum.lower():
        raise RuntimeError(
            "source checksum mismatch: "
            f"expected {args.expected_source_checksum.lower()}, got {checksum}"
        )

    result: dict[str, object] = {
        "schema_version": 1,
        "status": "succeeded",
        "algorithm_version": ALGORITHM_VERSION,
        "placeholder": True,
        "calibration_session_id": args.calibration_session_id,
        "capture_id": args.capture_id,
        "processor_image": args.processor_image,
        "source": {
            "uri": args.source_uri,
            "binding_path": str(args.input.resolve()),
            "size_bytes": size_bytes,
            "sha256": checksum,
        },
        "result": {
            "camera_matrix": [
                [1.0, 0.0, 0.0],
                [0.0, 1.0, 0.0],
                [0.0, 0.0, 1.0],
            ],
            "distortion_coefficients": [0.0, 0.0, 0.0, 0.0, 0.0],
            "message": "placeholder result; replace this job image with the real calibration implementation",
        },
        "started_at": started_at,
        "finished_at": utc_now(),
    }
    write_result(args.output.resolve(), result)
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--calibration-session-id", type=uuid_value, required=True)
    parser.add_argument("--capture-id", type=uuid_value, required=True)
    parser.add_argument("--expected-source-size", type=positive_integer, required=True)
    parser.add_argument("--expected-source-checksum", required=True)
    parser.add_argument("--source-uri", required=True)
    parser.add_argument("--processor-image", required=True)
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        result = run(args)
    except Exception as error:
        print(f"placeholder calibration failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
