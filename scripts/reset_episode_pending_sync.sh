#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# Move queued episodes back to unsynced by deleting latest pending sync_logs.
# Default mode is dry-run. Use --apply to mutate the database.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_ENV_FILE="$(cd "${SCRIPT_DIR}/.." && pwd)/.env"

ENV_FILE="${KEYSTONE_ENV_FILE:-${DEFAULT_ENV_FILE}}"
MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"
USE_SUDO_DOCKER="${KEYSTONE_DOCKER_SUDO:-0}"
APPLY=0
ALL=0
EPISODE_ID=""

ENV_KEYSTONE_MYSQL_HOST="${KEYSTONE_MYSQL_HOST:-}"
ENV_KEYSTONE_MYSQL_PORT="${KEYSTONE_MYSQL_PORT:-}"
ENV_KEYSTONE_MYSQL_USER="${KEYSTONE_MYSQL_USER:-}"
ENV_KEYSTONE_MYSQL_PASSWORD="${KEYSTONE_MYSQL_PASSWORD:-}"
ENV_KEYSTONE_MYSQL_DATABASE="${KEYSTONE_MYSQL_DATABASE:-}"
ENV_KEYSTONE_MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"

usage() {
  cat <<'EOF'
Usage: reset_episode_pending_sync.sh [options] EPISODE_ID
       reset_episode_pending_sync.sh [options] --all

Options:
  --apply                Delete the latest pending sync_log; default is dry-run only
  --all                  Delete latest pending sync_logs for all unsynced episodes
  --env-file PATH        Load Keystone env vars from PATH (default: keystone/.env)
  --docker-container NAME
                         Run mysql inside this Docker container
  --sudo-docker          Use sudo docker for Docker fallback
  -h, --help             Show this help

Behavior:
  - EPISODE_ID is episodes.episode_id, not the numeric episodes.id.
  - Only deletes the latest sync_log when its status is exactly 'pending'.
  - Refuses to touch in_progress, completed, failed, synced, missing, or deleted episodes.
  - Does not delete cloud objects and does not change QA state.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply)
      APPLY=1
      shift
      ;;
    --all)
      ALL=1
      shift
      ;;
    --env-file)
      if [[ $# -lt 2 ]]; then
        echo "error: --env-file requires a path" >&2
        exit 1
      fi
      ENV_FILE="$2"
      shift 2
      ;;
    --docker-container)
      if [[ $# -lt 2 ]]; then
        echo "error: --docker-container requires a container name" >&2
        exit 1
      fi
      MYSQL_CONTAINER="$2"
      shift 2
      ;;
    --sudo-docker)
      USE_SUDO_DOCKER=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
    *)
      if [[ -n "${EPISODE_ID}" ]]; then
        echo "error: only one EPISODE_ID argument is allowed" >&2
        exit 1
      fi
      EPISODE_ID="$1"
      shift
      ;;
  esac
done

if [[ "${ALL}" -eq 1 && -n "${EPISODE_ID}" ]]; then
  echo "error: --all cannot be combined with EPISODE_ID" >&2
  usage >&2
  exit 1
fi

if [[ "${ALL}" -ne 1 && -z "${EPISODE_ID}" ]]; then
  echo "error: EPISODE_ID is required unless --all is used" >&2
  usage >&2
  exit 1
fi

if [[ -n "${EPISODE_ID}" && ! "${EPISODE_ID}" =~ ^[A-Za-z0-9_.:-]+$ ]]; then
  echo "error: EPISODE_ID contains unsupported characters: ${EPISODE_ID}" >&2
  exit 1
fi

if [[ -f "${ENV_FILE}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
fi

[[ -n "${ENV_KEYSTONE_MYSQL_HOST}" ]] && KEYSTONE_MYSQL_HOST="${ENV_KEYSTONE_MYSQL_HOST}"
[[ -n "${ENV_KEYSTONE_MYSQL_PORT}" ]] && KEYSTONE_MYSQL_PORT="${ENV_KEYSTONE_MYSQL_PORT}"
[[ -n "${ENV_KEYSTONE_MYSQL_USER}" ]] && KEYSTONE_MYSQL_USER="${ENV_KEYSTONE_MYSQL_USER}"
[[ -n "${ENV_KEYSTONE_MYSQL_PASSWORD}" ]] && KEYSTONE_MYSQL_PASSWORD="${ENV_KEYSTONE_MYSQL_PASSWORD}"
[[ -n "${ENV_KEYSTONE_MYSQL_DATABASE}" ]] && KEYSTONE_MYSQL_DATABASE="${ENV_KEYSTONE_MYSQL_DATABASE}"
[[ -n "${ENV_KEYSTONE_MYSQL_CONTAINER}" ]] && MYSQL_CONTAINER="${ENV_KEYSTONE_MYSQL_CONTAINER}"

KEYSTONE_MYSQL_HOST="${KEYSTONE_MYSQL_HOST:-localhost}"
KEYSTONE_MYSQL_PORT="${KEYSTONE_MYSQL_PORT:-3306}"
KEYSTONE_MYSQL_USER="${KEYSTONE_MYSQL_USER:-keystone}"
KEYSTONE_MYSQL_PASSWORD="${KEYSTONE_MYSQL_PASSWORD:-}"
KEYSTONE_MYSQL_DATABASE="${KEYSTONE_MYSQL_DATABASE:-keystone}"

MYSQL_ARGS=(
  --host="${KEYSTONE_MYSQL_HOST}"
  --port="${KEYSTONE_MYSQL_PORT}"
  --user="${KEYSTONE_MYSQL_USER}"
  --database="${KEYSTONE_MYSQL_DATABASE}"
  --default-character-set=utf8mb4
  --table
)

DOCKER_MYSQL_ARGS=(
  --host="127.0.0.1"
  --port="3306"
  --user="${KEYSTONE_MYSQL_USER}"
  --database="${KEYSTONE_MYSQL_DATABASE}"
  --default-character-set=utf8mb4
  --table
)

export MYSQL_PWD="${KEYSTONE_MYSQL_PASSWORD}"

docker_cmd() {
  if [[ "${USE_SUDO_DOCKER}" == "1" || "${USE_SUDO_DOCKER}" == "true" ]]; then
    sudo docker "$@"
  else
    docker "$@"
  fi
}

find_mysql_container() {
  if [[ -n "${MYSQL_CONTAINER}" ]]; then
    echo "${MYSQL_CONTAINER}"
    return 0
  fi

  if ! command -v docker >/dev/null 2>&1; then
    return 1
  fi

  local candidate
  for candidate in keystone-mysql keystone-mysql-dev keystone-mysql-test; do
    if docker_cmd inspect -f '{{.State.Running}}' "${candidate}" 2>/dev/null | grep -q '^true$'; then
      echo "${candidate}"
      return 0
    fi
  done

  local name image
  while IFS=$'\t' read -r name image; do
    if [[ -n "${name}" && "${image,,}" == *mysql* ]]; then
      echo "${name}"
      return 0
    fi
  done < <(docker_cmd ps --format '{{.Names}}\t{{.Image}}' 2>/dev/null)

  return 1
}

mysql_exec() {
  local sql="$1"

  if command -v mysql >/dev/null 2>&1; then
    mysql "${MYSQL_ARGS[@]}" --execute="${sql}"
    return
  fi

  local container
  if ! container="$(find_mysql_container)"; then
    echo "error: mysql client not found in PATH and no running Keystone MySQL Docker container was found" >&2
    echo "hint: pass --sudo-docker, --docker-container NAME, or install mysql client" >&2
    exit 1
  fi

  docker_cmd exec -e MYSQL_PWD="${KEYSTONE_MYSQL_PASSWORD}" "${container}" \
    mysql "${DOCKER_MYSQL_ARGS[@]}" --execute="${sql}"
}

if [[ "${ALL}" -eq 1 ]]; then
  read -r -d '' STATUS_SQL <<SQL || true
SELECT
  e.id AS episode_db_id,
  e.episode_id,
  e.qa_status,
  e.cloud_synced,
  latest.id AS latest_sync_log_id,
  latest.status AS latest_sync_status,
  latest.started_at AS latest_sync_started_at
FROM episodes e
INNER JOIN (
  SELECT sl.*
  FROM sync_logs sl
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl.episode_id AND t.latest_id = sl.id
) latest ON latest.episode_id = e.id
WHERE e.deleted_at IS NULL
  AND e.cloud_synced = FALSE
  AND latest.status = 'pending'
ORDER BY e.created_at ASC, e.id ASC;
SQL
else
  read -r -d '' STATUS_SQL <<SQL || true
SET @episode_public_id := '${EPISODE_ID}';

SELECT
  e.id AS episode_db_id,
  e.episode_id,
  e.qa_status,
  e.cloud_synced,
  e.cloud_synced_at,
  latest.id AS latest_sync_log_id,
  latest.status AS latest_sync_status,
  latest.started_at AS latest_sync_started_at,
  latest.completed_at AS latest_sync_completed_at
FROM episodes e
LEFT JOIN (
  SELECT sl.*
  FROM sync_logs sl
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl.episode_id AND t.latest_id = sl.id
) latest ON latest.episode_id = e.id
WHERE e.episode_id = @episode_public_id
  AND e.deleted_at IS NULL;
SQL
fi

echo "[reset_episode_pending_sync] Current pending target state:"
mysql_exec "${STATUS_SQL}"

if [[ "${APPLY}" -ne 1 ]]; then
  echo
  echo "[reset_episode_pending_sync] Dry-run only. Re-run with --apply to delete latest pending sync_log rows."
  exit 0
fi

if [[ "${ALL}" -eq 1 ]]; then
  read -r -d '' APPLY_SQL <<SQL || true
START TRANSACTION;

DROP TEMPORARY TABLE IF EXISTS tmp_pending_sync_log_ids;
CREATE TEMPORARY TABLE tmp_pending_sync_log_ids AS
SELECT latest.id
FROM episodes e
INNER JOIN (
  SELECT sl_latest.*
  FROM sync_logs sl_latest
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl_latest.episode_id AND t.latest_id = sl_latest.id
) latest ON latest.episode_id = e.id
WHERE e.deleted_at IS NULL
  AND e.cloud_synced = FALSE
  AND latest.status = 'pending';

DELETE sl
FROM sync_logs sl
INNER JOIN tmp_pending_sync_log_ids target ON target.id = sl.id;

SELECT ROW_COUNT() AS deleted_pending_sync_logs;

DROP TEMPORARY TABLE IF EXISTS tmp_pending_sync_log_ids;

COMMIT;

SELECT
  COUNT(1) AS remaining_pending_sync_logs
FROM episodes e
INNER JOIN (
  SELECT sl.*
  FROM sync_logs sl
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl.episode_id AND t.latest_id = sl.id
) latest ON latest.episode_id = e.id
WHERE e.deleted_at IS NULL
  AND e.cloud_synced = FALSE
  AND latest.status = 'pending';
SQL
else
  read -r -d '' APPLY_SQL <<SQL || true
SET @episode_public_id := '${EPISODE_ID}';

START TRANSACTION;

DROP TEMPORARY TABLE IF EXISTS tmp_pending_sync_log_ids;
CREATE TEMPORARY TABLE tmp_pending_sync_log_ids AS
SELECT latest.id
FROM episodes e
INNER JOIN (
  SELECT sl_latest.*
  FROM sync_logs sl_latest
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl_latest.episode_id AND t.latest_id = sl_latest.id
) latest ON latest.episode_id = e.id
WHERE e.episode_id = @episode_public_id
  AND e.deleted_at IS NULL
  AND e.cloud_synced = FALSE
  AND latest.status = 'pending'
LIMIT 1;

DELETE sl
FROM sync_logs sl
INNER JOIN tmp_pending_sync_log_ids target ON target.id = sl.id;

SELECT ROW_COUNT() AS deleted_pending_sync_logs;

DROP TEMPORARY TABLE IF EXISTS tmp_pending_sync_log_ids;

COMMIT;

SELECT
  e.id AS episode_db_id,
  e.episode_id,
  e.cloud_synced,
  latest.id AS latest_sync_log_id,
  COALESCE(latest.status, 'not_started') AS latest_sync_status
FROM episodes e
LEFT JOIN (
  SELECT sl.*
  FROM sync_logs sl
  INNER JOIN (
    SELECT episode_id, MAX(id) AS latest_id
    FROM sync_logs
    GROUP BY episode_id
  ) t ON t.episode_id = sl.episode_id AND t.latest_id = sl.id
) latest ON latest.episode_id = e.id
WHERE e.episode_id = @episode_public_id
  AND e.deleted_at IS NULL;
SQL
fi

echo
echo "[reset_episode_pending_sync] Applying reset..."
mysql_exec "${APPLY_SQL}"
