#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# Episode 统计：调用管理后台 summary 接口，按 STATS_TZ 计算今日/昨日/总计；可选 POST 飞书 Webhook。
# 逻辑在下方 Python；本文件仅注入环境变量。
#
#   KEYSTONE_BASE=http://127.0.0.1:9999 TOKEN=… ./scripts/episode_day_stats.sh
#   KEYSTONE_BASE=http://127.0.0.1:9999 ./scripts/episode_day_stats.sh
#   ./scripts/episode_day_stats.sh 'https://www.feishu.cn/flow/api/trigger-webhook/…'
#   ./scripts/episode_day_stats.sh 'https://open.feishu.cn/open-apis/bot/v2/hook/…'
# TOKEN 为空时会用 KEYSTONE_ADMIN_USERNAME/KEYSTONE_ADMIN_PASSWORD 登录，默认 admin/admin123。
# 不传参数则只打印统计、不发飞书（忽略父 shell 里的 FEISHU_WEBHOOK_URL）。
# Flow webhook 会收到字段 JSON；群机器人 webhook 会收到文本消息。
#
# Requires: python3 (stdlib only)
set -euo pipefail

unset FEISHU_WEBHOOK_URL 2>/dev/null || true
HOOK=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h)
      echo "Usage: $(basename "$0") [WEBHOOK_URL]"
      echo "  WEBHOOK_URL         Optional. 飞书 Flow 或群机器人 Webhook；不传则不发飞书。"
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
      echo "error: unknown argument: $1 (expected https://... webhook or --help)" >&2
      exit 1
      ;;
  esac
done

export KEYSTONE_BASE="${KEYSTONE_BASE:-http://127.0.0.1:9999}"
export TOKEN="${TOKEN:-}"
export KEYSTONE_ADMIN_USERNAME="${KEYSTONE_ADMIN_USERNAME:-admin}"
export KEYSTONE_ADMIN_PASSWORD="${KEYSTONE_ADMIN_PASSWORD:-admin123}"
export STATS_TZ="${STATS_TZ:-Asia/Shanghai}"
export STATS_TOTAL_START="${STATS_TOTAL_START:-1970-01-01T00:00:00Z}"
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
from datetime import datetime, timedelta
from typing import Dict, Tuple
from zoneinfo import ZoneInfo

SUMMARY_PATH = "/api/v1/admin/statistics/data-production/summary"
LOGIN_PATH = "/api/v1/auth/login"


def rfc3339_z(dt_utc: datetime) -> str:
    s = dt_utc.strftime("%Y-%m-%dT%H:%M:%S")
    if dt_utc.microsecond:
        s += ".%06d" % dt_utc.microsecond
    return s + "Z"


def fmt_size_binary(b: int) -> str:
    """1024-based size scaling, matching Synapse display semantics."""
    value = float(int(b))
    units = ["B", "KB", "MB", "GB", "TB", "PB"]
    unit = units[0]
    for unit in units:
        if abs(value) < 1024 or unit == units[-1]:
            break
        value /= 1024.0
    if unit == "B":
        return "%.0f%s" % (value, unit)
    return "%.2f%s" % (value, unit)


def fmt_hours2(sec: float) -> str:
    """Hours from seconds, two fractional digits."""
    return "%.2fh" % (max(0.0, float(sec)) / 3600.0)


def fmt_json_size(b: int) -> str:
    """Same text as terminal: raw bytes + 1024-based human size."""
    return "%d 字节 (%s)" % (int(b), fmt_size_binary(b))


def fmt_json_duration(sec: float) -> str:
    """Same text as terminal: raw seconds + hours in existing string field."""
    return "%.2f秒 (%s)" % (float(sec), fmt_hours2(sec))


def feishu_webhook_url() -> str:
    """Set by shell wrapper from optional CLI argument only (not for manual export)."""
    return os.environ.get("FEISHU_WEBHOOK_URL", "").strip()


def is_feishu_bot_webhook(url: str) -> bool:
    parsed = urllib.parse.urlparse(url)
    return parsed.netloc == "open.feishu.cn" and parsed.path.startswith(
        "/open-apis/bot/v2/hook/"
    )


def feishu_flow_body(
    p_count: int,
    p_bytes: int,
    p_dur: float,
    y_count: int,
    y_bytes: int,
    y_dur: float,
    t_count: int,
    t_bytes: int,
    t_dur: float,
) -> dict:
    return {
        "data_size": fmt_json_size(p_bytes),
        "data_duration": fmt_json_duration(p_dur),
        "count": p_count,
        "last_count": y_count,
        "last_data_size": fmt_json_size(y_bytes),
        "last_data_duration": fmt_json_duration(y_dur),
        "total_data_size": fmt_json_size(t_bytes),
        "total_data_duration": fmt_json_duration(t_dur),
        "total_count": t_count,
    }


def feishu_bot_body(
    p_count: int,
    p_bytes: int,
    p_dur: float,
    y_count: int,
    y_bytes: int,
    y_dur: float,
    t_count: int,
    t_bytes: int,
    t_dur: float,
) -> dict:
    text = "\n".join(
        [
            "Episode 数据日报",
            "",
            "今日数据量: %d条" % p_count,
            "今日数据大小: %s" % fmt_json_size(p_bytes),
            "今日时长: %s" % fmt_json_duration(p_dur),
            "",
            "昨日数据量: %d条" % y_count,
            "昨日数据大小: %s" % fmt_json_size(y_bytes),
            "昨日时长: %s" % fmt_json_duration(y_dur),
            "",
            "总数据量: %d条" % t_count,
            "总计数据大小: %s" % fmt_json_size(t_bytes),
            "总计时长: %s" % fmt_json_duration(t_dur),
        ]
    )
    return {"msg_type": "text", "content": {"text": text}}


def http_get_json(url: str, headers: Dict[str, str]) -> dict:
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")[:500]
        sys.stderr.write("HTTP %s GET %s: %s\n" % (e.code, url, body))
        sys.exit(1)


def http_post_json(url: str, body: dict, headers: Dict[str, str]) -> dict:
    payload = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=payload,
        headers={**headers, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body_text = e.read().decode(errors="replace")[:500]
        sys.stderr.write("HTTP %s POST %s: %s\n" % (e.code, url, body_text))
        sys.exit(1)


def resolve_token(base: str, token: str) -> str:
    token = token.strip()
    if token:
        return token

    account = os.environ.get("KEYSTONE_ADMIN_USERNAME", "admin").strip()
    password = os.environ.get("KEYSTONE_ADMIN_PASSWORD", "admin123")
    if not account or not password:
        sys.stderr.write("TOKEN is empty and admin login credentials are incomplete\n")
        sys.exit(1)

    url = base.rstrip("/") + LOGIN_PATH
    sys.stderr.write("[episode_day_stats] TOKEN empty; logging in as %s\n" % account)
    data = http_post_json(
        url,
        {"account": account, "password": password},
        {"Accept": "application/json"},
    )
    if data.get("error"):
        sys.stderr.write("login error: %s\n" % data.get("error"))
        sys.exit(1)
    if data.get("role") != "admin":
        sys.stderr.write("login role is %r, expected admin\n" % data.get("role"))
        sys.exit(1)
    access_token = str(data.get("access_token") or "").strip()
    if not access_token:
        sys.stderr.write("login response missing access_token\n")
        sys.exit(1)
    return access_token


def fetch_summary(base: str, token: str, label: str, start_z: str, end_z: str) -> Tuple[int, int, float]:
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token

    params = urllib.parse.urlencode({"start_time": start_z, "end_time": end_z})
    url = base.rstrip("/") + SUMMARY_PATH + "?" + params
    sys.stderr.write(
        "[episode_day_stats] GET summary %s window (UTC): %s .. %s\n"
        % (label, start_z, end_z)
    )
    data = http_get_json(url, headers)
    if data.get("error"):
        sys.stderr.write("API error: %s\n" % data.get("error"))
        sys.exit(1)

    count = int((data.get("count") or {}).get("total") or 0)
    total_bytes = int((data.get("size") or {}).get("total_bytes") or 0)
    total_ms = float((data.get("duration") or {}).get("total_ms") or 0)
    return count, total_bytes, total_ms / 1000.0



def main() -> None:
    base = os.environ.get("KEYSTONE_BASE", "http://127.0.0.1:9999").strip()
    token = os.environ.get("TOKEN", "").strip()
    total_start_z = os.environ.get("STATS_TOTAL_START", "1970-01-01T00:00:00Z").strip()
    raw_tz = (os.environ.get("STATS_TZ") or "Asia/Shanghai").strip()
    tz_name = raw_tz or "Asia/Shanghai"
    try:
        tz = ZoneInfo(tz_name)
    except Exception as e:
        sys.stderr.write("invalid STATS_TZ %r: %s\n" % (tz_name, e))
        sys.exit(1)

    token = resolve_token(base, token)

    utc = ZoneInfo("UTC")
    now_local = datetime.now(tz)
    day = now_local.date()
    start_local = datetime(day.year, day.month, day.day, 0, 0, 0, 0, tzinfo=tz)
    from_z = rfc3339_z(start_local.astimezone(utc))
    to_z = rfc3339_z(now_local.astimezone(utc))

    yesterday = day - timedelta(days=1)
    start_y = datetime(
        yesterday.year, yesterday.month, yesterday.day, 0, 0, 0, 0, tzinfo=tz
    )
    from_y_z = rfc3339_z(start_y.astimezone(utc))
    to_y_z = from_z

    p_count, p_bytes, p_dur = fetch_summary(base, token, "today", from_z, to_z)
    y_count, y_bytes, y_dur = fetch_summary(base, token, "yesterday", from_y_z, to_y_z)
    t_count, t_bytes, t_dur = fetch_summary(base, token, "total", total_start_z, to_z)

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
        if is_feishu_bot_webhook(hook):
            payload = feishu_bot_body(
                p_count, p_bytes, p_dur, y_count, y_bytes, y_dur, t_count, t_bytes, t_dur
            )
            target = "bot"
        else:
            payload = feishu_flow_body(
                p_count, p_bytes, p_dur, y_count, y_bytes, y_dur, t_count, t_bytes, t_dur
            )
            target = "flow"

        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")

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

        print("feishu %s:    sent OK" % target)
    else:
        print("feishu flow:    skipped (no FEISHU_WEBHOOK_URL / no positional webhook)")


if __name__ == "__main__":
    main()
PY
