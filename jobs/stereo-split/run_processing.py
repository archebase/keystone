#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Run the fixed DECXIN stereo split contract for an Orbit Job."""

from __future__ import annotations

import argparse
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import time
from typing import Iterable

from split_mcap_stereo_imu import DecxinMcapStereoImuSplitter


OUTPUT_MCAP_NAME = "output_bag.mcap"
OUTPUT_METADATA_NAME = "metadata.yaml"
MANIFEST_NAME = "processing_manifest.json"
COPY_BUFFER_BYTES = 8 * 1024 * 1024
COPY_ATTEMPTS = 3
COPY_RETRY_SECONDS = 1
SCRATCH_SPACE_MULTIPLIER = 4
MCAP_MAGIC = b"\x89MCAP0\r\n"


@dataclass(frozen=True)
class FileIdentity:
    size_bytes: int
    sha256: str


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def copy_and_hash(source: Path, destination: Path) -> FileIdentity:
    digest = hashlib.sha256()
    size_bytes = 0
    destination.parent.mkdir(parents=True, exist_ok=True)

    with source.open("rb") as source_stream, destination.open("wb") as destination_stream:
        while chunk := source_stream.read(COPY_BUFFER_BYTES):
            destination_stream.write(chunk)
            digest.update(chunk)
            size_bytes += len(chunk)
        destination_stream.flush()
        os.fsync(destination_stream.fileno())

    if destination.stat().st_size != size_bytes:
        raise RuntimeError(f"copy size mismatch for {destination}")
    return FileIdentity(size_bytes=size_bytes, sha256=digest.hexdigest())


def copy_and_hash_with_retries(source: Path, destination: Path) -> FileIdentity:
    for attempt in range(1, COPY_ATTEMPTS + 1):
        try:
            return copy_and_hash(source, destination)
        except (OSError, RuntimeError):
            if attempt == COPY_ATTEMPTS:
                raise
            time.sleep(COPY_RETRY_SECONDS * attempt)
    raise AssertionError("copy retry loop exhausted")


def stage_local_file(source: Path, destination: Path) -> FileIdentity:
    partial = destination.with_name(f".{destination.name}.partial")
    identity = copy_and_hash_with_retries(source, partial)
    os.replace(partial, destination)
    return identity


def hash_file(path: Path) -> FileIdentity:
    digest = hashlib.sha256()
    size_bytes = 0
    with path.open("rb") as stream:
        while chunk := stream.read(COPY_BUFFER_BYTES):
            digest.update(chunk)
            size_bytes += len(chunk)
    return FileIdentity(size_bytes=size_bytes, sha256=digest.hexdigest())


def require_output(path: Path) -> FileIdentity:
    if not path.is_file():
        raise RuntimeError(f"required output is missing: {path.name}")
    identity = hash_file(path)
    if identity.size_bytes <= 0:
        raise RuntimeError(f"required output is empty: {path.name}")
    return identity


def require_mcap_output(path: Path) -> FileIdentity:
    identity = require_output(path)
    if identity.size_bytes < len(MCAP_MAGIC) * 2:
        raise RuntimeError(f"MCAP output is too small: {path.name}")
    with path.open("rb") as stream:
        leading_magic = stream.read(len(MCAP_MAGIC))
        stream.seek(-len(MCAP_MAGIC), os.SEEK_END)
        trailing_magic = stream.read(len(MCAP_MAGIC))
    if leading_magic != MCAP_MAGIC or trailing_magic != MCAP_MAGIC:
        raise RuntimeError(f"MCAP output has invalid magic: {path.name}")
    return identity


def publish_file(source: Path, destination: Path, expected: FileIdentity) -> None:
    actual = copy_and_hash_with_retries(source, destination)
    if actual != expected:
        raise RuntimeError(f"published output identity mismatch: {destination.name}")


def write_manifest(path: Path, manifest: dict[str, object]) -> None:
    encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as stream:
        stream.write(encoded)
        stream.flush()
        os.fsync(stream.fileno())


def require_scratch_capacity(scratch: Path, source_size_bytes: int) -> None:
    scratch.mkdir(parents=True, exist_ok=True)
    required_bytes = source_size_bytes * SCRATCH_SPACE_MULTIPLIER
    available_bytes = shutil.disk_usage(scratch).free
    if available_bytes < required_bytes:
        raise RuntimeError(
            "insufficient scratch space: "
            f"required {required_bytes} bytes, available {available_bytes} bytes"
        )


def run(args: argparse.Namespace) -> dict[str, object]:
    source = args.input.resolve()
    output_binding = args.output_binding.resolve()
    scratch = args.scratch.resolve()

    if not source.is_file():
        raise RuntimeError(f"input MCAP does not exist: {source}")
    if not output_binding.is_dir():
        raise RuntimeError(f"output binding is not a directory: {output_binding}")
    if (output_binding / MANIFEST_NAME).exists():
        raise RuntimeError("output binding already contains a processing manifest")
    require_scratch_capacity(scratch, args.expected_source_size)

    started_at = utc_now()
    local_input = scratch / "input" / "source.mcap"
    local_output = scratch / "output_bag"
    local_manifest = scratch / MANIFEST_NAME

    source_identity = stage_local_file(source, local_input)
    if source_identity.size_bytes != args.expected_source_size:
        raise RuntimeError(
            "source size mismatch: "
            f"expected {args.expected_source_size}, got {source_identity.size_bytes}"
        )
    if args.expected_source_checksum:
        expected_checksum = args.expected_source_checksum.lower()
        if source_identity.sha256 != expected_checksum:
            raise RuntimeError(
                "source checksum mismatch: "
                f"expected {expected_checksum}, got {source_identity.sha256}"
            )

    stats = DecxinMcapStereoImuSplitter().convert(local_input, local_output)
    local_mcap = local_output / OUTPUT_MCAP_NAME
    local_metadata = local_output / OUTPUT_METADATA_NAME
    mcap_identity = require_mcap_output(local_mcap)
    metadata_identity = require_output(local_metadata)

    publish_file(local_mcap, output_binding / OUTPUT_MCAP_NAME, mcap_identity)
    publish_file(local_metadata, output_binding / OUTPUT_METADATA_NAME, metadata_identity)

    manifest: dict[str, object] = {
        "schema_version": 1,
        "status": "succeeded",
        "kind": args.kind,
        "generation": args.generation,
        "processor_image": args.processor_image,
        "source": {
            "uri": args.source_uri,
            "binding_path": str(source),
            "size_bytes": source_identity.size_bytes,
            "sha256": source_identity.sha256,
        },
        "outputs": {
            "mcap": {
                "name": OUTPUT_MCAP_NAME,
                "size_bytes": mcap_identity.size_bytes,
                "sha256": mcap_identity.sha256,
            },
            "metadata": {
                "name": OUTPUT_METADATA_NAME,
                "size_bytes": metadata_identity.size_bytes,
                "sha256": metadata_identity.sha256,
            },
        },
        "stats": asdict(stats),
        "started_at": started_at,
        "finished_at": utc_now(),
    }
    write_manifest(local_manifest, manifest)
    manifest_identity = hash_file(local_manifest)
    publish_file(local_manifest, output_binding / MANIFEST_NAME, manifest_identity)
    return manifest


def positive_integer(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("must be greater than zero")
    return parsed


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output-binding", type=Path, required=True)
    parser.add_argument("--scratch", type=Path, required=True)
    parser.add_argument("--expected-source-size", type=positive_integer, required=True)
    parser.add_argument("--expected-source-checksum", default="")
    parser.add_argument("--source-uri", required=True)
    parser.add_argument("--processor-image", required=True)
    parser.add_argument("--kind", choices=("stereo_split",), required=True)
    parser.add_argument("--generation", type=positive_integer, required=True)
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        manifest = run(args)
    except Exception as error:
        print(f"stereo split failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(manifest, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
