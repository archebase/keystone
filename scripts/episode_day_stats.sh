#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# Episode 统计：当天(STATS_TZ)0 点～当前 created_at 区间 + 全量；可选 POST 飞书自动化 Webhook。
# 部分线上 Keystone 会忽略 list 的 created_at_* 查询参数，故默认「全量分页拉取后在本地按 created_at 过滤」再汇总 partial。
# 逻辑在下方 Python；本文件仅注入环境变量。
#
#   KEYSTONE_BASE=http://127.0.0.1:9999 TOKEN=… ./scripts/episode_day_stats.sh
#   ./scripts/episode_day_stats.sh 'https://www.feishu.cn/flow/api/trigger-webhook/…'
# 不传参数则只打印统计、不发飞书（忽略父 shell 里的 FEISHU_WEBHOOK_URL）。飞书 JSON 含今日/总计及昨日 last_* 字段。
#
# Requires: python3 (stdlib only)
set -euo pipefail

unset FEISHU_WEBHOOK_URL 2>/dev/null || true
HOOK=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h)
      echo "Usage: $(basename "$0") [WEBHOOK_URL]"
      echo "  WEBHOOK_URL         Optional. https://… 飞书流程 Webhook；不传则不发飞书。"
      echo "  --help, -h          Show this help."
      exit 0
      ;;
    http://* | https://*)
      if [[ -n "${HOOK}" ]]; then
        echo "error: only one WEBHOOK_URL argument allowed" >&2
        exit 1
      fi
      HOOK="$1"
      shift
      ;;
    *)
      echo "error: unknown argument: $1 (expected https://… webhook or --help)" >&2
      exit 1
      ;;
  esac
done

export KEYSTONE_BASE="${KEYSTONE_BASE:-http://127.0.0.1:9999}"
export TOKEN="${TOKEN:-}"
export STATS_TZ="${STATS_TZ:-Asia/Shanghai}"
export FEISHU_WEBHOOK_URL="${HOOK}"

exec python3 - <<'PY'
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from typing import Dict, List, Optional
from zoneinfo import ZoneInfo

LIMIT = 100


def rfc3339_z(dt_utc: datetime) -> str:
    s = dt_utc.strftime("%Y-%m-%dT%H:%M:%S")
    if dt_utc.microsecond:
        s += ".%06d" % dt_utc.microsecond
    return s + "Z"


def fmt_size_gb2(b: int) -> str:
    """Decimal GB (1e9 bytes), two fractional digits."""
    return "%.2fGB" % (int(b) / 1_000_000_000.0)


def fmt_hours2(sec: float) -> str:
    """Hours from seconds, two fractional digits."""
    return "%.2fh" % (max(0.0, float(sec)) / 3600.0)


def fmt_json_size(b: int) -> str:
    """Same text as terminal: raw bytes + GB in existing string field."""
    return "%d 字节 (%s)" % (int(b), fmt_size_gb2(b))


def fmt_json_duration(sec: float) -> str:
    """Same text as terminal: raw seconds + hours in existing string field."""
    return "%.2f秒 (%s)" % (float(sec), fmt_hours2(sec))


def feishu_webhook_url() -> str:
    """Set by shell wrapper from optional CLI argument only (not for manual export)."""
    return os.environ.get("FEISHU_WEBHOOK_URL", "").strip()


def http_get_json(url: str, headers: Dict[str, str]) -> dict:
    req = urllib.request.Request(url, headers=headers, method="GET")
    with urllib.request.urlopen(req, timeout=120) as resp:
        return json.loads(resp.read().decode())


def parse_created_at_utc(raw: object) -> Optional[datetime]:
    if raw is None or not isinstance(raw, str):
        return None
    s = raw.strip()
    if not s:
        return None
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(s)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def fetch_all_episodes(base: str, token: str) -> List[dict]:
    """GET /episodes with no created_at filter; paginate until exhausted."""
    base = base.rstrip("/")
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token

    out: List[dict] = []
    offset = 0
    total = 0

    sys.stderr.write(
        "[episode_day_stats] fetch all pages (server filter NOT used)\n"
    )

    while True:
        q = urllib.parse.urlencode({"limit": str(LIMIT), "offset": str(offset)})
        url = base + "/api/v1/episodes?" + q
        if offset == 0:
            sys.stderr.write("[episode_day_stats] GET %s\n" % url)
        data = http_get_json(url, headers)
        if data.get("error"):
            sys.stderr.write("API error: %s\n" % data.get("error"))
            sys.exit(1)
        items = data.get("items") or []
        total = int(data.get("total", 0))
        out.extend(items)
        n = len(items)
        offset += n
        if n == 0 or offset >= total:
            break

    sys.stderr.write(
        "[episode_day_stats] fetch done total=%d rows=%d\n" % (total, len(out))
    )
    return out


def sum_episodes(rows: List[dict]) -> tuple[int, int, float]:
    b = 0
    d = 0.0
    for i in rows:
        fs = i.get("file_size_bytes")
        b += int(fs) if fs is not None else 0
        dv = i.get("duration_sec")
        d += float(dv) if dv is not None else 0.0
    return len(rows), b, d


def main() -> None:
    base = os.environ.get("KEYSTONE_BASE", "http://127.0.0.1:9999").strip()
    token = os.environ.get("TOKEN", "").strip()
    raw_tz = (os.environ.get("STATS_TZ") or "Asia/Shanghai").strip()
    tz_name = raw_tz or "Asia/Shanghai"
    try:
        tz = ZoneInfo(tz_name)
    except Exception as e:
        sys.stderr.write("invalid STATS_TZ %r: %s\n" % (tz_name, e))
        sys.exit(1)

    utc = ZoneInfo("UTC")
    now_local = datetime.now(tz)
    day = now_local.date()
    start_local = datetime(day.year, day.month, day.day, 0, 0, 0, 0, tzinfo=tz)
    from_z = rfc3339_z(start_local.astimezone(utc))
    to_z = rfc3339_z(now_local.astimezone(utc))
    from_dt = start_local.astimezone(timezone.utc)
    to_dt = now_local.astimezone(timezone.utc)

    sys.stderr.write(
        "[episode_day_stats] partial window (UTC): %s .. %s\n" % (from_z, to_z)
    )

    all_eps = fetch_all_episodes(base, token)

    partial_eps: List[dict] = []
    for i in all_eps:
        ct = parse_created_at_utc(i.get("created_at"))
        if ct is None:
            continue
        if from_dt <= ct <= to_dt:
            partial_eps.append(i)

    yesterday = day - timedelta(days=1)
    start_y = datetime(
        yesterday.year, yesterday.month, yesterday.day, 0, 0, 0, 0, tzinfo=tz
    )
    end_y = start_y + timedelta(days=1) - timedelta(microseconds=1)
    from_y = start_y.astimezone(timezone.utc)
    to_y = end_y.astimezone(timezone.utc)

    yesterday_eps: List[dict] = []
    for i in all_eps:
        ct = parse_created_at_utc(i.get("created_at"))
        if ct is None:
            continue
        if from_y <= ct <= to_y:
            yesterday_eps.append(i)

    p_count, p_bytes, p_dur = sum_episodes(partial_eps)
    y_count, y_bytes, y_dur = sum_episodes(yesterday_eps)
    t_count, t_bytes, t_dur = sum_episodes(all_eps)

    print(">>>")
    print("今日数据量:   %d条" % p_count)
    print("今日数据大小： %s" % fmt_json_size(p_bytes))
    print("今日时长： %s" % fmt_json_duration(p_dur))
    print("")
    print(">>>")
    print("昨日数据量:   %d条" % y_count)
    print("昨日数据大小： %s" % fmt_json_size(y_bytes))
    print("昨日时长： %s" % fmt_json_duration(y_dur))
    print("")
    print(">>>")
    print("总数据量: %d条" % t_count)
    print("总计数据大小： %s" % fmt_json_size(t_bytes))
    print("总计时长： %s" % fmt_json_duration(t_dur))

    hook = feishu_webhook_url()
    if hook:
        body = json.dumps(
            {
                "data_size": fmt_json_size(p_bytes),
                "data_duration": fmt_json_duration(p_dur),
                "count": p_count,
                "last_count": y_count,
                "last_data_size": fmt_json_size(y_bytes),
                "last_data_duration": fmt_json_duration(y_dur),
                "total_data_size": fmt_json_size(t_bytes),
                "total_data_duration": fmt_json_duration(t_dur),
                "total_count": t_count,
            },
            ensure_ascii=False,
        ).encode("utf-8")

        req = urllib.request.Request(
            hook,
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                resp.read()
        except urllib.error.HTTPError as e:
            sys.stderr.write("feishu HTTP %s: %s\n" % (e.code, e.read().decode()[:500]))
            sys.exit(1)

        print("feishu flow:    sent OK")
    else:
        print("feishu flow:    skipped (no FEISHU_WEBHOOK_URL / no positional webhook)")


if __name__ == "__main__":
    main()
PY
