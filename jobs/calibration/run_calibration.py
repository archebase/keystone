#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

"""Preprocess one raw DECXIN MCAP and run the ArcheBase calibration pipeline."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from typing import Callable, Iterable, NoReturn
import uuid

from preprocess_decxin import PreprocessingRejected
from preprocess_decxin import preprocess_decxin as default_preprocess


COPY_BUFFER_BYTES = 8 * 1024 * 1024
MCAP_MAGIC = b"\x89MCAP0\r\n"
CALIBRATION_ROOT = Path(os.environ.get("ARCHEBASE_CALIB_ROOT", "/workspace"))
CALIBRATION_CLI = CALIBRATION_ROOT / "calib_cli" / "calibrate.py"
CALIBRATION_CONFIG = CALIBRATION_ROOT / "calib_cli" / "config.example.json"
CALIBRATION_REVISION = os.environ.get("ARCHEBASE_CALIB_REVISION", "unknown").strip() or "unknown"
ALGORITHM_VERSION = f"archebase-calib-{CALIBRATION_REVISION}"


class CalibrationRejected(RuntimeError):
    """The calibration pipeline completed with a domain-level rejection."""

    def __init__(self, error_path: Path) -> None:
        super().__init__(f"calibration pipeline rejected the capture: {error_path}")
        self.error_path = error_path


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def positive_integer(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("must be greater than zero")
    return parsed


def sha256_value(value: str) -> str:
    normalized = value.strip().lower()
    if re.fullmatch(r"[0-9a-f]{64}", normalized) is None:
        raise argparse.ArgumentTypeError("must be a 64-character hexadecimal SHA-256")
    return normalized


def uuid_value(value: str) -> str:
    try:
        parsed = uuid.UUID(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be a canonical UUIDv4") from error
    if parsed.version != 4 or str(parsed) != value:
        raise argparse.ArgumentTypeError("must be a canonical UUIDv4")
    return str(parsed)


def nonempty_value(value: str) -> str:
    normalized = value.strip()
    if not normalized:
        raise argparse.ArgumentTypeError("must not be empty")
    return normalized


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


def read_json_object(path: Path, label: str) -> dict[str, object]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError(f"cannot read {label}: {path}: {error}") from error
    if not isinstance(value, dict):
        raise RuntimeError(f"{label} must contain a JSON object: {path}")
    return value


def write_result(path: Path, result: dict[str, object]) -> None:
    path = path.expanduser().resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        raise RuntimeError(f"result JSON already exists: {path}")
    encoded = (json.dumps(result, indent=2, sort_keys=True) + "\n").encode("utf-8")
    temporary_path = path.parent / f".{path.name}.{uuid.uuid4()}.tmp"
    try:
        with temporary_path.open("xb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        os.link(temporary_path, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        temporary_path.unlink(missing_ok=True)


def run_archebase_calibration(preprocessed_mcap: Path, output_directory: Path) -> None:
    command = [
        sys.executable,
        str(CALIBRATION_CLI),
        "--mcap", str(preprocessed_mcap),
        "--config", str(CALIBRATION_CONFIG),
        "--output", str(output_directory),
    ]
    completed = subprocess.run(command, check=False)
    if completed.returncode == 0:
        return
    error_path = output_directory / "error.json"
    if error_path.is_file():
        raise CalibrationRejected(error_path)
    raise RuntimeError(
        f"ArcheBase calibration CLI exited with status {completed.returncode} without error.json"
    )


def source_document(args: argparse.Namespace, input_path: Path, size_bytes: int, checksum: str) -> dict[str, object]:
    return {
        "uri": args.source_uri,
        "binding_path": str(input_path),
        "size_bytes": size_bytes,
        "sha256": checksum,
    }


def failure_message(document: dict[str, object]) -> str:
    error = document.get("error")
    if isinstance(error, dict):
        message = error.get("message")
        if isinstance(message, str) and message.strip():
            return message.strip()
    return "calibration pipeline rejected the capture"


def failed_result(
    args: argparse.Namespace,
    source: dict[str, object],
    started_at: str,
    failure: dict[str, object],
    preprocessing: dict[str, int | str] | None = None,
) -> dict[str, object]:
    details: dict[str, object] = {"failure": failure}
    if preprocessing is not None:
        details["preprocessing"] = preprocessing
    return {
        "schema_version": 1,
        "status": "failed",
        "algorithm_version": ALGORITHM_VERSION,
        "calibration_session_id": args.calibration_session_id,
        "capture_id": args.capture_id,
        "camera_serial": args.camera_serial,
        "processor_image": args.processor_image,
        "error_message": failure_message(failure),
        "source": source,
        "result": details,
        "started_at": started_at,
        "finished_at": utc_now(),
    }


def run(
    args: argparse.Namespace,
    *,
    preprocess: Callable[
        [Path, Path], tuple[Path, dict[str, int | str]]
    ] = default_preprocess,
    calibrate: Callable[[Path, Path], None] = run_archebase_calibration,
) -> dict[str, object]:
    started_at = utc_now()
    input_path = args.input.expanduser().resolve()
    size_bytes, checksum = inspect_mcap(input_path)
    if size_bytes != args.expected_source_size:
        raise RuntimeError(
            f"source size mismatch: expected {args.expected_source_size}, got {size_bytes}"
        )
    if checksum != args.expected_source_checksum:
        raise RuntimeError(
            f"source checksum mismatch: expected {args.expected_source_checksum}, got {checksum}"
        )

    output_path = args.output.expanduser().resolve()
    if output_path.exists():
        raise RuntimeError(f"result JSON already exists: {output_path}")
    scratch_root = args.scratch.expanduser().resolve()
    scratch_root.mkdir(parents=True, exist_ok=True)
    work_directory = Path(
        tempfile.mkdtemp(prefix=f"calibration-{args.capture_id}-", dir=scratch_root)
    )
    source = source_document(args, input_path, size_bytes, checksum)
    try:
        preprocessed_mcap, preprocessing = preprocess(
            input_path, work_directory / "preprocessed"
        )
    except PreprocessingRejected as error:
        failure = {
            "stage": "preprocessing",
            "error": {
                "type": type(error).__name__,
                "message": str(error),
            },
        }
        result = failed_result(args, source, started_at, failure)
        write_result(output_path, result)
        return result

    calibration_output = work_directory / "calibration-output"

    try:
        calibrate(preprocessed_mcap, calibration_output)
    except CalibrationRejected as error:
        failure = read_json_object(error.error_path, "calibration failure JSON")
        result = failed_result(args, source, started_at, failure, preprocessing)
        write_result(output_path, result)
        return result

    result = {
        "schema_version": 1,
        "status": "succeeded",
        "algorithm_version": ALGORITHM_VERSION,
        "calibration_session_id": args.calibration_session_id,
        "capture_id": args.capture_id,
        "camera_serial": args.camera_serial,
        "processor_image": args.processor_image,
        "source": source,
        "result": {
            "preprocessing": preprocessing,
            "precheck": read_json_object(
                calibration_output / "precheck" / "result.json", "precheck result"
            ),
            "quality": read_json_object(
                calibration_output / "calibration_quality.json", "calibration quality"
            ),
            "calibration": read_json_object(
                calibration_output / "calibration.json", "calibration result"
            ),
        },
        "started_at": started_at,
        "finished_at": utc_now(),
    }
    write_result(output_path, result)
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--calibration-session-id", type=uuid_value, required=True)
    parser.add_argument("--capture-id", type=uuid_value, required=True)
    parser.add_argument("--camera-serial", type=nonempty_value, required=True)
    parser.add_argument("--expected-source-size", type=positive_integer, required=True)
    parser.add_argument("--expected-source-checksum", type=sha256_value, required=True)
    parser.add_argument("--source-uri", type=nonempty_value, required=True)
    parser.add_argument("--processor-image", type=nonempty_value, required=True)
    parser.add_argument("--scratch", type=Path, default=Path("/scratch"))
    return parser


def print_failure_and_exit(error: Exception) -> NoReturn:
    print(f"calibration job failed: {error}", file=sys.stderr)
    raise SystemExit(1)


def main(argv: Iterable[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        result = run(args)
    except Exception as error:
        print_failure_and_exit(error)
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
