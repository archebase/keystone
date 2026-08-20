#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Export MinIO converted MCAP files with their collector SQLite metadata.

The script only performs S3 GET/LIST operations.  It does not download MCAP
contents: object sizes come from ListObjectsV2, and each CMD's SQLite backup is
downloaded once to a temporary file to look up the original recording metadata.

Example:
  MINIO_ACCESS_KEY=keystone MINIO_SECRET_KEY=keystone123 \\
  python3 scripts/export_converted_mcap_inventory.py \\
    --endpoint http://127.0.0.1:9000 --output converted_mcap_inventory.csv

Requires: boto3 (``python3 -m pip install boto3``)
"""

from __future__ import annotations

import argparse
import csv
import os
import sqlite3
import sys
import tempfile
from collections import defaultdict
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Any, Iterable

try:
    import boto3
    from botocore.client import BaseClient
    from botocore.exceptions import BotoCoreError, ClientError
except ImportError:
    # Keep --help usable on hosts where the optional S3 dependency is absent.
    boto3 = None
    BaseClient = Any
    BotoCoreError = OSError
    ClientError = OSError


DEFAULT_BUCKET = "weifang-data"
DEFAULT_PREFIX = "converted/"
DB_FILENAMES = ("db/collector.sqlite", "db_backup/collector.sqlite")

CSV_FIELDS = (
    "minio_path",
    "file",
    "converted_size_bytes",
    "minio_last_modified",
    "minio_etag",
    "cmd_id",
    "recording_relative_path",
    "filename",
    "original_path",
    "original_size_bytes",
    "original_sha256",
    "size_delta_bytes",
    "started_at",
    "ended_at",
    "duration_ms",
    "device_id",
    "task_name",
    "scene",
    "capture_id",
    "keystone_task_id",
    "task_id",
    "workspace_id",
    "dc_plan_id",
    "session_status",
    "validation_status",
    "file_status",
    "upload_status",
    "uploaded_at",
    "upload_bucket",
    "upload_object_key",
    "db_object_path",
    "match_status",
    "match_reason",
)


@dataclass(frozen=True)
class McapObject:
    key: str
    size: int
    last_modified: str
    etag: str

    @property
    def cmd_id(self) -> str | None:
        parts = PurePosixPath(self.key).parts
        return parts[1] if len(parts) >= 3 and parts[0] == "converted" and parts[1].startswith("CMD-") else None

    @property
    def relative_path(self) -> str | None:
        cmd_id = self.cmd_id
        if not cmd_id:
            return None
        parts = PurePosixPath(self.key).parts
        return "/".join(parts[2:])


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True, help="MinIO S3 endpoint, e.g. http://127.0.0.1:9000")
    parser.add_argument("--bucket", default=DEFAULT_BUCKET)
    parser.add_argument("--prefix", default=DEFAULT_PREFIX)
    parser.add_argument("--output", default="converted_mcap_inventory.csv")
    parser.add_argument("--access-key", default=os.getenv("MINIO_ACCESS_KEY"), help="defaults to MINIO_ACCESS_KEY")
    parser.add_argument("--secret-key", default=os.getenv("MINIO_SECRET_KEY"), help="defaults to MINIO_SECRET_KEY")
    return parser.parse_args()


def list_objects(client: BaseClient, bucket: str, prefix: str) -> list[dict[str, Any]]:
    objects: list[dict[str, Any]] = []
    paginator = client.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        objects.extend(page.get("Contents", []))
    return objects


def find_db_objects(objects: Iterable[dict[str, Any]]) -> dict[str, str]:
    """Return the preferred SQLite backup key for every CMD directory."""
    candidates: dict[str, list[tuple[int, str, str]]] = defaultdict(list)
    for item in objects:
        key = item["Key"]
        path = PurePosixPath(key)
        parts = path.parts
        if len(parts) < 4 or parts[0] != "converted" or not parts[1].startswith("CMD-"):
            continue
        relative = "/".join(parts[2:])
        if relative not in DB_FILENAMES:
            continue
        # Prefer db/ over db_backup/; newest object breaks a tie deterministically.
        preference = 0 if relative == "db/collector.sqlite" else 1
        candidates[parts[1]].append((preference, str(item["LastModified"]), key))
    preferred: dict[str, str] = {}
    for cmd, items in candidates.items():
        best_preference = min(item[0] for item in items)
        best = max((item for item in items if item[0] == best_preference), key=lambda item: item[1])
        preferred[cmd] = best[2]
    return preferred


def normalized_recording_path(path: str) -> str | None:
    """Map a collector path to the relative suffix used below converted/CMD-*.

    Typical input: /data/collector/recordings/2026-08-05/task_2/session_001.mcap
    Typical output: 2026-08-05/task_2/session_001.mcap
    """
    parts = PurePosixPath(path).parts
    try:
        index = parts.index("recordings")
    except ValueError:
        return None
    suffix = parts[index + 1 :]
    return "/".join(suffix) if suffix else None


def converted_relative_path(relative_path: str | None) -> str | None:
    if not relative_path:
        return None
    parts = PurePosixPath(relative_path).parts
    if parts and parts[0] == "recordings":
        parts = parts[1:]
    return "/".join(parts)


def load_records(client: BaseClient, bucket: str, db_key: str) -> list[dict[str, Any]]:
    """Download one SQLite object temporarily and return safe MCAP metadata rows."""
    with tempfile.NamedTemporaryFile(suffix=".sqlite") as database_file:
        client.download_file(bucket, db_key, database_file.name)
        connection = sqlite3.connect(f"file:{database_file.name}?mode=ro", uri=True)
        connection.row_factory = sqlite3.Row
        try:
            rows = connection.execute(
                """
                SELECT
                  rf.path AS original_path, rf.filename, rf.size_bytes AS original_size_bytes,
                  rf.sha256 AS original_sha256, rf.status AS file_status, rf.uploaded_at,
                  cs.device_id, cs.started_at, cs.ended_at, cs.duration_ms,
                  cs.status AS session_status, cs.validation_status, cs.capture_id,
                  cs.keystone_task_id, cs.keystone_task_id AS task_id,
                  cs.keystone_workspace_id AS workspace_id,
                  cs.keystone_dc_plan_id AS dc_plan_id,
                  t.name AS task_name, t.scene,
                  ku.status AS upload_status, ku.bucket AS upload_bucket,
                  ku.object_key AS upload_object_key
                FROM recording_files rf
                JOIN collection_sessions cs ON cs.id = rf.session_id
                LEFT JOIN tasks t ON t.id = cs.task_id
                LEFT JOIN keystone_upload_jobs ku ON ku.recording_file_id = rf.id
                WHERE lower(rf.format) = 'mcap'
                """
            ).fetchall()
            return [dict(row) for row in rows]
        finally:
            connection.close()


def index_records(records: Iterable[dict[str, Any]]) -> tuple[dict[str, list[dict[str, Any]]], dict[str, list[dict[str, Any]]]]:
    by_relative: dict[str, list[dict[str, Any]]] = defaultdict(list)
    by_filename: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for record in records:
        relative = normalized_recording_path(record["original_path"])
        if relative:
            by_relative[relative].append(record)
        by_filename[record["filename"]].append(record)
    return by_relative, by_filename


def choose_record(mcap: McapObject, by_relative: dict[str, list[dict[str, Any]]], by_filename: dict[str, list[dict[str, Any]]]) -> tuple[dict[str, Any] | None, str, str]:
    relative = converted_relative_path(mcap.relative_path)
    exact = by_relative.get(relative or "", [])
    if len(exact) == 1:
        return exact[0], "matched", "relative recording path"
    if len(exact) > 1:
        return None, "ambiguous", "multiple SQLite rows have the same relative recording path"
    filename = PurePosixPath(mcap.key).name
    fallback = by_filename.get(filename, [])
    if len(fallback) == 1:
        return fallback[0], "filename_fallback", "unique filename; relative path was not found"
    if len(fallback) > 1:
        return None, "unmatched", "filename is not unique and relative path was not found"
    return None, "unmatched", "no corresponding MCAP row in the SQLite backup"


def build_row(mcap: McapObject, db_key: str | None, record: dict[str, Any] | None, status: str, reason: str) -> dict[str, Any]:
    row: dict[str, Any] = {field: "" for field in CSV_FIELDS}
    row.update(
        minio_path=mcap.key,
        file=mcap.key,
        converted_size_bytes=mcap.size,
        minio_last_modified=mcap.last_modified,
        minio_etag=mcap.etag,
        cmd_id=mcap.cmd_id or "",
        recording_relative_path=converted_relative_path(mcap.relative_path) or "",
        filename=PurePosixPath(mcap.key).name,
        db_object_path=db_key or "",
        match_status=status,
        match_reason=reason,
    )
    if record:
        row.update(record)
        row["size_delta_bytes"] = mcap.size - int(record["original_size_bytes"])
    return row


def main() -> int:
    args = parse_args()
    if boto3 is None:
        print("error: boto3 is required; install it with: python3 -m pip install boto3", file=sys.stderr)
        return 2
    if not args.access_key or not args.secret_key:
        print("error: provide MINIO_ACCESS_KEY and MINIO_SECRET_KEY (or both CLI options)", file=sys.stderr)
        return 2
    client = boto3.client(
        "s3",
        endpoint_url=args.endpoint,
        aws_access_key_id=args.access_key,
        aws_secret_access_key=args.secret_key,
    )
    objects = list_objects(client, args.bucket, args.prefix)
    databases = find_db_objects(objects)
    mcaps = [
        McapObject(item["Key"], item["Size"], str(item["LastModified"]), item["ETag"])
        for item in objects
        if item["Key"].lower().endswith(".mcap")
        and not PurePosixPath(item["Key"]).name.startswith("._")
    ]
    record_indexes: dict[str, tuple[dict[str, list[dict[str, Any]]], dict[str, list[dict[str, Any]]]]] = {}
    for cmd_id, db_key in databases.items():
        try:
            record_indexes[cmd_id] = index_records(load_records(client, args.bucket, db_key))
        except (OSError, sqlite3.Error, BotoCoreError, ClientError) as error:
            print(f"warning: cannot read {db_key}: {error}", file=sys.stderr)

    with open(args.output, "w", newline="", encoding="utf-8") as output_file:
        writer = csv.DictWriter(output_file, fieldnames=CSV_FIELDS)
        writer.writeheader()
        for mcap in sorted(mcaps, key=lambda item: item.key):
            db_key = databases.get(mcap.cmd_id or "")
            index = record_indexes.get(mcap.cmd_id or "")
            if not db_key:
                writer.writerow(build_row(mcap, None, None, "unmatched", "no collector.sqlite object for CMD"))
            elif not index:
                writer.writerow(build_row(mcap, db_key, None, "unmatched", "SQLite backup could not be read"))
            else:
                record, status, reason = choose_record(mcap, *index)
                writer.writerow(build_row(mcap, db_key, record, status, reason))
    print(f"wrote {len(mcaps)} MCAP rows to {args.output}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
