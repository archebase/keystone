#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Run the fixed DECXIN stereo H.264 conversion contract for an Orbit Job."""

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

from convert_mcap_stereo_h264 import ConverterConfig, StereoSplitH264Converter
from mcap.reader import make_reader


OUTPUT_MCAP_NAME = "output_bag.mcap"
OUTPUT_METADATA_NAME = "metadata.yaml"
MANIFEST_NAME = "processing_manifest.json"
COPY_BUFFER_BYTES = 8 * 1024 * 1024
COPY_ATTEMPTS = 3
COPY_RETRY_SECONDS = 1
SCRATCH_SPACE_MULTIPLIER = 3
MCAP_MAGIC = b"\x89MCAP0\r\n"
CALIBRATION_ATTACHMENT_NAME = "calibration.json"
CALIBRATION_MEDIA_TYPE = "application/json"


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
    return FileIdentity(size_bytes, digest.hexdigest())


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
    return FileIdentity(size_bytes, digest.hexdigest())


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
        leading = stream.read(len(MCAP_MAGIC))
        stream.seek(-len(MCAP_MAGIC), os.SEEK_END)
        trailing = stream.read(len(MCAP_MAGIC))
    if leading != MCAP_MAGIC or trailing != MCAP_MAGIC:
        raise RuntimeError(f"MCAP output has invalid magic: {path.name}")
    return identity


def publish_file(source: Path, destination: Path, expected: FileIdentity) -> None:
    actual = copy_and_hash_with_retries(source, destination)
    if actual != expected:
        raise RuntimeError(f"published output identity mismatch: {destination.name}")


def write_json(path: Path, value: dict[str, object]) -> None:
    encoded = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as stream:
        stream.write(encoded)
        stream.flush()
        os.fsync(stream.fileno())


def require_scratch_capacity(scratch: Path, source_size_bytes: int) -> None:
    scratch.mkdir(parents=True, exist_ok=True)
    required = source_size_bytes * SCRATCH_SPACE_MULTIPLIER
    available = shutil.disk_usage(scratch).free
    if available < required:
        raise RuntimeError(f"insufficient scratch space: required {required} bytes, available {available} bytes")


def calibration_provenance(
    document: dict[str, object], args: argparse.Namespace
) -> tuple[str, str]:
    document_session_id = str(document.get("calibration_session_id", "")).strip()
    document_capture_id = str(document.get("capture_id", "")).strip()
    argument_session_id = str(getattr(args, "calibration_session_id", "")).strip()
    argument_capture_id = str(getattr(args, "calibration_capture_id", "")).strip()

    if bool(document_session_id) != bool(document_capture_id):
        raise RuntimeError(
            "calibration JSON must contain calibration_session_id and capture_id together"
        )
    if bool(argument_session_id) != bool(argument_capture_id):
        raise RuntimeError(
            "calibration result arguments must contain calibration_session_id "
            "and capture_id together"
        )
    if (
        document_session_id
        and argument_session_id
        and document_session_id != argument_session_id
    ):
        raise RuntimeError(
            "calibration JSON calibration_session_id does not match the argument"
        )
    if (
        document_capture_id
        and argument_capture_id
        and document_capture_id != argument_capture_id
    ):
        raise RuntimeError("calibration JSON capture_id does not match the argument")

    return (
        argument_session_id or document_session_id,
        argument_capture_id or document_capture_id,
    )


def load_calibration_result(args: argparse.Namespace) -> tuple[bytes, dict[str, object]] | None:
    values = (
        args.calibration_result,
        args.calibration_camera_serial,
        args.expected_calibration_size,
        args.expected_calibration_checksum,
    )
    if not any(value not in (None, "", 0) for value in values):
        return None
    if any(value in (None, "", 0) for value in values):
        raise RuntimeError("calibration result arguments must be provided together")

    path = args.calibration_result.resolve()
    if not path.is_file():
        raise RuntimeError(f"calibration result does not exist: {path}")
    data = path.read_bytes()
    digest = hashlib.sha256(data).hexdigest()
    if len(data) != args.expected_calibration_size:
        raise RuntimeError(
            "calibration result size mismatch: "
            f"expected {args.expected_calibration_size}, got {len(data)}"
        )
    if digest != args.expected_calibration_checksum.lower():
        raise RuntimeError(
            "calibration result checksum mismatch: "
            f"expected {args.expected_calibration_checksum.lower()}, got {digest}"
        )
    try:
        document = json.loads(data)
    except json.JSONDecodeError as error:
        raise RuntimeError("calibration result JSON is invalid") from error
    if not isinstance(document, dict):
        raise RuntimeError("calibration result JSON must contain an object")
    if not document:
        raise RuntimeError("calibration JSON must contain an object")
    session_id, capture_id = calibration_provenance(document, args)
    return data, {
        "attachment_name": CALIBRATION_ATTACHMENT_NAME,
        "media_type": CALIBRATION_MEDIA_TYPE,
        "camera_serial": args.calibration_camera_serial,
        "session_id": session_id,
        "capture_id": capture_id,
        "size_bytes": len(data),
        "sha256": digest,
    }


def load_embedded_calibration(path: Path) -> dict[str, object] | None:
    with path.open("rb") as stream:
        attachments = [
            attachment
            for attachment in make_reader(stream).iter_attachments()
            if attachment.name == CALIBRATION_ATTACHMENT_NAME
        ]
    if not attachments:
        return None
    if len(attachments) != 1:
        raise RuntimeError("output MCAP contains duplicate calibration attachments")
    attachment = attachments[0]
    try:
        document = json.loads(attachment.data)
    except json.JSONDecodeError as error:
        raise RuntimeError("embedded calibration attachment JSON is invalid") from error
    if not isinstance(document, dict) or not document:
        raise RuntimeError("embedded calibration attachment must contain an object")
    camera_serial = str(document.get("camera_serial", "")).strip()
    session_id = str(document.get("calibration_session_id", "")).strip()
    capture_id = str(document.get("capture_id", "")).strip()
    if bool(session_id) != bool(capture_id):
        raise RuntimeError(
            "embedded calibration attachment must contain calibration_session_id "
            "and capture_id together"
        )
    if not camera_serial:
        raise RuntimeError(
            "embedded calibration attachment is missing camera_serial"
        )
    data = bytes(attachment.data)
    return {
        "attachment_name": CALIBRATION_ATTACHMENT_NAME,
        "media_type": attachment.media_type,
        "camera_serial": camera_serial,
        "session_id": session_id,
        "capture_id": capture_id,
        "size_bytes": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
    }


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
    calibration = load_calibration_result(args)

    started_at = utc_now()
    local_input = scratch / "input" / "source.mcap"
    local_mcap = scratch / OUTPUT_MCAP_NAME
    local_metadata = scratch / OUTPUT_METADATA_NAME
    local_manifest = scratch / MANIFEST_NAME
    source_identity = stage_local_file(source, local_input)
    if source_identity.size_bytes != args.expected_source_size:
        raise RuntimeError(
            f"source size mismatch: expected {args.expected_source_size}, got {source_identity.size_bytes}"
        )
    if args.expected_source_checksum:
        expected = args.expected_source_checksum.lower()
        if source_identity.sha256 != expected:
            raise RuntimeError(f"source checksum mismatch: expected {expected}, got {source_identity.sha256}")

    config = ConverterConfig()
    stats = StereoSplitH264Converter(config).convert(
        local_input,
        local_mcap,
        calibration_attachment=calibration[0] if calibration is not None else None,
    )
    manifest_calibration = None
    processing_mode = "timestamp_repair" if stats.input_mode == "split_h264" else "convert"
    metadata: dict[str, object] = {
        "schema_version": 1,
        "format": "mcap",
        "processing_mode": processing_mode,
        "video": {
            "codec": "h264",
            "profile": "high",
            "encoder": "libx264",
            "resolution": f"{config.eye_width}x{config.eye_height}",
            "nominal_fps": config.nominal_fps,
            "target_bitrate": config.target_bitrate,
            "max_bitrate": config.max_bitrate,
            "gop": config.gop,
            "topics": [config.left_topic, config.right_topic],
        },
        "imu_topic": config.imu_topic,
        "stats": asdict(stats),
    }
    write_json(local_metadata, metadata)
    mcap_identity = require_mcap_output(local_mcap)
    metadata_identity = require_output(local_metadata)
    publish_file(local_mcap, output_binding / OUTPUT_MCAP_NAME, mcap_identity)
    publish_file(local_metadata, output_binding / OUTPUT_METADATA_NAME, metadata_identity)

    manifest: dict[str, object] = {
        "schema_version": 2,
        "status": "succeeded",
        "kind": args.kind,
        "processing_mode": processing_mode,
        "output_format": "stereo_h264",
        "generation": args.generation,
        "processor_image": args.processor_image,
        "source": {
            "uri": args.source_uri,
            "binding_path": str(source),
            "size_bytes": source_identity.size_bytes,
            "sha256": source_identity.sha256,
        },
        "outputs": {
            "mcap": {"name": OUTPUT_MCAP_NAME, **asdict(mcap_identity)},
            "metadata": {"name": OUTPUT_METADATA_NAME, **asdict(metadata_identity)},
        },
        "stats": asdict(stats),
        "started_at": started_at,
        "finished_at": utc_now(),
    }
    if manifest_calibration is not None:
        manifest["calibration"] = manifest_calibration
    write_json(local_manifest, manifest)
    publish_file(local_manifest, output_binding / MANIFEST_NAME, hash_file(local_manifest))
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
    parser.add_argument("--calibration-result", type=Path)
    parser.add_argument("--calibration-camera-serial", default="")
    parser.add_argument("--calibration-session-id", default="")
    parser.add_argument("--calibration-capture-id", default="")
    parser.add_argument("--expected-calibration-size", type=positive_integer)
    parser.add_argument("--expected-calibration-checksum", default="")
    return parser


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        manifest = run(args)
    except Exception as error:
        print(f"stereo split convert failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(manifest, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
