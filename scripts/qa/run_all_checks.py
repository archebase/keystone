# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

import argparse
import hashlib
import json
import os
import sys


def parse_args():
    parser = argparse.ArgumentParser(description="Run Keystone episode Python QA checks.")
    parser.add_argument("--mcap", required=True, help="Path to the local MCAP file.")
    parser.add_argument("--sidecar", required=True, help="Path to the local sidecar JSON file.")
    parser.add_argument("--output", required=True, help="Path to write the result JSON.")
    return parser.parse_args()


def load_expected_checksum(sidecar_path):
    with open(sidecar_path, "r", encoding="utf-8") as file:
        sidecar = json.load(file)
    checksum = (
        sidecar.get("recording", {})
        .get("checksum_sha256", "")
        .strip()
        .lower()
    )
    if not checksum:
        return "", "sidecar recording.checksum_sha256 is missing"
    return checksum, ""


def calculate_sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_result(output_path, passed, score, details, metadata):
    result = {
        "passed": passed,
        "score": score,
        "details": details,
        "metadata": metadata,
    }
    with open(output_path, "w", encoding="utf-8") as file:
        json.dump(result, file, ensure_ascii=False)


def main():
    args = parse_args()
    metadata = {
        "check": "checksum_sha256_match",
        "mcap_file_size_bytes": os.path.getsize(args.mcap) if os.path.exists(args.mcap) else 0,
    }

    expected, error = load_expected_checksum(args.sidecar)
    if error:
        metadata["error"] = error
        write_result(args.output, False, 0.0, "MCAP checksum_sha256 missing in sidecar", metadata)
        return 0

    actual = calculate_sha256(args.mcap)
    metadata["expected_sha256"] = expected
    metadata["actual_sha256"] = actual

    if actual != expected:
        write_result(args.output, False, 0.0, "MCAP checksum_sha256 mismatch", metadata)
        return 0

    write_result(args.output, True, 1.0, "MCAP checksum_sha256 matched sidecar", metadata)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"python qa failed: {exc}", file=sys.stderr)
        raise
