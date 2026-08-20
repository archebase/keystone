#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

"""Add a 数采员 column to a converted MCAP inventory CSV.

The collector is resolved through Keystone Mercury using this chain:

    keystone_task_id -> task.dc_plan_id -> dc_plan.operator -> collector.name

Only read-only Mercury APIs are called. The input CSV is never overwritten.

Example:
  MERCURY_ACCOUNT=admin MERCURY_PASSWORD=... \\
    python3 scripts/enrich_converted_mcap_collectors.py \\
      /tmp/converted_mcap_inventory.csv \\
      /tmp/converted_mcap_inventory_with_collectors.csv
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterator
from pathlib import Path
from typing import Any


DEFAULT_BASE_URL = "https://keystone-mercury.archebase.cn/api/v1"
COLLECTOR_COLUMN = "数采员"
DERIVED_COLUMNS = ("workspace_id", "file", "dc_plan_id", "task_id", COLLECTOR_COLUMN)


class MercuryError(RuntimeError):
    """Raised when a Mercury request or response is invalid."""


class MercuryClient:
    def __init__(self, base_url: str, account: str, password: str, timeout: float) -> None:
        self.base_url = base_url.rstrip("/")
        self.account = account
        self.password = password
        self.timeout = timeout
        self.token = ""

    def login(self) -> None:
        response = self._request(
            "POST",
            "/auth/login",
            body={"account": self.account, "password": self.password},
            authenticated=False,
        )
        token = str(response.get("access_token", "")).strip()
        if not token:
            raise MercuryError("Mercury login response did not contain access_token")
        self.token = token

    def iter_items(
        self,
        path: str,
        params: dict[str, str | int] | None = None,
        *,
        page_size: int,
    ) -> Iterator[dict[str, Any]]:
        offset = 0
        while True:
            query = dict(params or {})
            query.update({"limit": page_size, "offset": offset})
            response = self._request("GET", path, params=query)
            items = response.get("items")
            if not isinstance(items, list):
                raise MercuryError(f"Mercury {path} response did not contain an items list")
            for item in items:
                if isinstance(item, dict):
                    yield item
            offset += len(items)
            total = response.get("total")
            if not items or (isinstance(total, int) and offset >= total):
                return

    def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, str | int] | None = None,
        body: dict[str, Any] | None = None,
        authenticated: bool = True,
    ) -> dict[str, Any]:
        url = self.base_url + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        payload = json.dumps(body).encode("utf-8") if body is not None else None
        headers = {"Accept": "application/json"}
        if payload is not None:
            headers["Content-Type"] = "application/json"
        if authenticated:
            if not self.token:
                raise MercuryError("Mercury client is not authenticated")
            headers["Authorization"] = "Bearer " + self.token
        request = urllib.request.Request(url, data=payload, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                decoded = json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read(512).decode("utf-8", errors="replace")
            raise MercuryError(f"Mercury {method} {path} returned HTTP {error.code}: {detail}") from error
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
            raise MercuryError(f"Mercury {method} {path} failed: {error}") from error
        if not isinstance(decoded, dict):
            raise MercuryError(f"Mercury {method} {path} returned a non-object response")
        return decoded


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path, help="source inventory CSV")
    parser.add_argument("output", type=Path, help="new CSV with the 数采员 column")
    parser.add_argument(
        "--base-url",
        default=os.getenv("MERCURY_BASE_URL", DEFAULT_BASE_URL),
        help=f"Mercury API base URL (default: {DEFAULT_BASE_URL})",
    )
    parser.add_argument("--account", default=os.getenv("MERCURY_ACCOUNT"))
    parser.add_argument("--password", default=os.getenv("MERCURY_PASSWORD"))
    parser.add_argument("--timeout", type=float, default=30.0)
    return parser.parse_args()


def read_inventory(path: Path) -> tuple[list[str], list[dict[str, str]]]:
    with path.open("r", newline="", encoding="utf-8-sig") as input_file:
        reader = csv.DictReader(input_file)
        if not reader.fieldnames:
            raise ValueError("input CSV has no header")
        if "keystone_task_id" not in reader.fieldnames:
            raise ValueError("input CSV is missing required keystone_task_id column")
        return list(reader.fieldnames), list(reader)


def task_plan_index(
    client: MercuryClient,
    wanted_task_ids: set[str],
) -> dict[str, tuple[int, int]]:
    result: dict[str, tuple[int, int]] = {}
    for task in client.iter_items("/tasks", page_size=1000):
        task_id = str(task.get("task_id", "")).strip()
        if task_id not in wanted_task_ids:
            continue
        dc_plan_id = task.get("dc_plan_id")
        workspace_id = task.get("workspace_id")
        if isinstance(dc_plan_id, int) and isinstance(workspace_id, int):
            result[task_id] = (workspace_id, dc_plan_id)
    return result


def plan_operator_index(
    client: MercuryClient,
    workspace_ids: set[int],
) -> dict[tuple[int, int], str]:
    result: dict[tuple[int, int], str] = {}
    for workspace_id in sorted(workspace_ids):
        for plan in client.iter_items(
            "/dc-plans",
            {"workspace_id": workspace_id},
            page_size=100,
        ):
            plan_id = plan.get("id")
            operator = str(plan.get("operator", "")).strip()
            if isinstance(plan_id, int) and operator:
                result[(workspace_id, plan_id)] = operator
    return result


def collector_name_index(client: MercuryClient) -> dict[str, str]:
    result: dict[str, str] = {}
    for collector in client.iter_items("/data_collectors", page_size=100):
        operator = str(collector.get("operator_id", "")).strip()
        name = str(collector.get("name", "")).strip()
        if operator:
            result[operator] = name or operator
    return result


def write_enriched_csv(
    output: Path,
    fieldnames: list[str],
    rows: list[dict[str, str]],
    task_plans: dict[str, tuple[int, int]],
    plan_operators: dict[tuple[int, int], str],
    collector_names: dict[str, str],
) -> tuple[int, list[str]]:
    output_fields = [field for field in fieldnames if field not in DERIVED_COLUMNS]
    output_fields.extend(DERIVED_COLUMNS)
    missing_task_ids: list[str] = []
    resolved = 0
    with output.open("w", newline="", encoding="utf-8-sig") as output_file:
        writer = csv.DictWriter(output_file, fieldnames=output_fields, extrasaction="ignore")
        writer.writeheader()
        for row in rows:
            task_id = row.get("keystone_task_id", "").strip()
            collector_name = ""
            plan_key = task_plans.get(task_id)
            workspace_id = "4"
            dc_plan_id = ""
            if plan_key:
                workspace_id = str(plan_key[0])
                dc_plan_id = str(plan_key[1])
                operator = plan_operators.get(plan_key, "")
                if operator:
                    collector_name = collector_names.get(operator, operator)
            if collector_name:
                resolved += 1
            else:
                missing_task_ids.append(task_id or "<empty>")
            row["workspace_id"] = workspace_id
            row["file"] = row.get("minio_path", "")
            row["dc_plan_id"] = dc_plan_id
            row["task_id"] = task_id
            row[COLLECTOR_COLUMN] = collector_name
            writer.writerow(row)
    return resolved, missing_task_ids


def main() -> int:
    args = parse_args()
    if not args.account or not args.password:
        print("error: set MERCURY_ACCOUNT and MERCURY_PASSWORD", file=sys.stderr)
        return 2
    if args.input.resolve() == args.output.resolve():
        print("error: output must differ from input; the source CSV is never overwritten", file=sys.stderr)
        return 2
    try:
        fieldnames, rows = read_inventory(args.input)
        task_ids = {row["keystone_task_id"].strip() for row in rows if row["keystone_task_id"].strip()}
        client = MercuryClient(args.base_url, args.account, args.password, args.timeout)
        client.login()
        task_plans = task_plan_index(client, task_ids)
        workspace_ids = {workspace_id for workspace_id, _ in task_plans.values()}
        plan_operators = plan_operator_index(client, workspace_ids)
        collector_names = collector_name_index(client)
        resolved, missing = write_enriched_csv(
            args.output,
            fieldnames,
            rows,
            task_plans,
            plan_operators,
            collector_names,
        )
    except (OSError, ValueError, MercuryError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1

    print(f"wrote {len(rows)} rows to {args.output}", file=sys.stderr)
    print(f"resolved collectors: {resolved}/{len(rows)}", file=sys.stderr)
    if missing:
        examples = ", ".join(sorted(set(missing))[:10])
        print(f"warning: {len(missing)} rows have no collector; examples: {examples}", file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
