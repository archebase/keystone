#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# Count episodes per device by parsing episodes.mcap_path.
#
# Usage:
#   cd keystone
#   ./scripts/episode_device_counts.sh
#   ./scripts/episode_device_counts.sh --csv
#   ./scripts/episode_device_counts.sh --unparsed-details
#
# The script loads MySQL settings from keystone/.env by default. Environment
# variables already exported by the shell take precedence after loading. If the
# local mysql client is missing, it falls back to a running Docker MySQL container.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_ENV_FILE="$(cd "${SCRIPT_DIR}/.." && pwd)/.env"

ENV_FILE="${KEYSTONE_ENV_FILE:-${DEFAULT_ENV_FILE}}"
CSV=0
UNPARSED_DETAILS=0
MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"
USE_SUDO_DOCKER="${KEYSTONE_DOCKER_SUDO:-0}"
ENV_KEYSTONE_MYSQL_HOST="${KEYSTONE_MYSQL_HOST:-}"
ENV_KEYSTONE_MYSQL_PORT="${KEYSTONE_MYSQL_PORT:-}"
ENV_KEYSTONE_MYSQL_USER="${KEYSTONE_MYSQL_USER:-}"
ENV_KEYSTONE_MYSQL_PASSWORD="${KEYSTONE_MYSQL_PASSWORD:-}"
ENV_KEYSTONE_MYSQL_DATABASE="${KEYSTONE_MYSQL_DATABASE:-}"
ENV_KEYSTONE_MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"

usage() {
  cat <<'EOF'
Usage: episode_device_counts.sh [options]

Options:
  --env-file PATH        Load Keystone env vars from PATH (default: keystone/.env)
  --csv                  Print tab-separated output suitable for CSV import
  --unparsed-details     Also list episodes whose device ID could not be parsed
  --docker-container NAME
                       Run mysql inside this Docker container
  --sudo-docker          Use sudo docker for Docker fallback
  -h, --help             Show this help

MySQL env vars:
  KEYSTONE_MYSQL_HOST      default: localhost
  KEYSTONE_MYSQL_PORT      default: 3306
  KEYSTONE_MYSQL_USER      default: keystone
  KEYSTONE_MYSQL_PASSWORD  default: empty
  KEYSTONE_MYSQL_DATABASE  default: keystone
  KEYSTONE_MYSQL_CONTAINER optional Docker container override
  KEYSTONE_DOCKER_SUDO     set to 1 to use sudo docker
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      if [[ $# -lt 2 ]]; then
        echo "error: --env-file requires a path" >&2
        exit 1
      fi
      ENV_FILE="$2"
      shift 2
      ;;
    --csv)
      CSV=1
      shift
      ;;
    --unparsed-details)
      UNPARSED_DETAILS=1
      shift
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
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

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
)

DOCKER_MYSQL_ARGS=(
  --host="127.0.0.1"
  --port="3306"
  --user="${KEYSTONE_MYSQL_USER}"
  --database="${KEYSTONE_MYSQL_DATABASE}"
  --default-character-set=utf8mb4
)

if [[ "${CSV}" -eq 1 ]]; then
  MYSQL_ARGS+=(--batch --raw)
  DOCKER_MYSQL_ARGS+=(--batch --raw)
else
  MYSQL_ARGS+=(--table)
  DOCKER_MYSQL_ARGS+=(--table)
fi

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
    echo "hint: start Docker MySQL, or pass --docker-container NAME, or set KEYSTONE_MYSQL_CONTAINER=NAME" >&2
    exit 1
  fi

  docker_cmd exec -e MYSQL_PWD="${KEYSTONE_MYSQL_PASSWORD}" "${container}" \
    mysql "${DOCKER_MYSQL_ARGS[@]}" --execute="${sql}"
}

read -r -d '' DEVICE_COUNTS_SQL <<'SQL' || true
WITH normalized_paths AS (
  SELECT
    e.id,
    e.episode_id,
    e.file_size_bytes,
    e.created_at,
    TRIM(
      BOTH '/' FROM
      CASE
        WHEN LOCATE('://', SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)) > 0 THEN
          SUBSTRING(
            SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1),
            LOCATE('://', SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)) + 3
          )
        ELSE SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)
      END
    ) AS object_path
  FROM episodes e
  WHERE e.deleted_at IS NULL
),
path_segments AS (
  SELECT
    id,
    episode_id,
    file_size_bytes,
    created_at,
    object_path,
    CASE
      WHEN object_path = '' THEN 0
      ELSE LENGTH(object_path) - LENGTH(REPLACE(object_path, '/', '')) + 1
    END AS segment_count
  FROM normalized_paths
),
parsed_episodes AS (
  SELECT
    id,
    episode_id,
    file_size_bytes,
    created_at,
    object_path,
    segment_count,
    CASE
      WHEN segment_count >= 5 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
        SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 3), '/', -1)
      WHEN segment_count = 4 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
        SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 2), '/', -1)
      ELSE NULL
    END AS parsed_device_id
  FROM path_segments
),
device_rows AS (
  SELECT
    COALESCE(parsed_device_id, '__UNPARSED__') AS device_id,
    file_size_bytes,
    created_at
  FROM parsed_episodes
)
SELECT
  dr.device_id AS device_id,
  COUNT(*) AS episode_count,
  COALESCE(SUM(dr.file_size_bytes), 0) AS total_bytes,
  ROUND(COALESCE(SUM(dr.file_size_bytes), 0) / 1024 / 1024 / 1024, 2) AS total_gib,
  MIN(dr.created_at) AS first_episode_at,
  MAX(dr.created_at) AS last_episode_at,
  COALESCE(CAST(r.id AS CHAR), '') AS current_robot_id,
  COALESCE(CAST(r.robot_type_id AS CHAR), '') AS current_robot_type_id,
  COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), '') AS current_robot_type
FROM device_rows dr
LEFT JOIN robots r
  ON r.device_id = dr.device_id
 AND r.deleted_at IS NULL
LEFT JOIN robot_types rt
  ON rt.id = r.robot_type_id
 AND rt.deleted_at IS NULL
GROUP BY
  dr.device_id,
  r.id,
  r.robot_type_id,
  COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), '')
ORDER BY
  CASE WHEN dr.device_id = '__UNPARSED__' THEN 1 ELSE 0 END,
  episode_count DESC,
  dr.device_id ASC;
SQL

mysql_exec "${DEVICE_COUNTS_SQL}"

if [[ "${UNPARSED_DETAILS}" -eq 1 ]]; then
  read -r -d '' UNPARSED_SQL <<'SQL' || true
WITH normalized_paths AS (
  SELECT
    e.id,
    e.episode_id,
    TRIM(COALESCE(e.mcap_path, '')) AS raw_mcap_path,
    TRIM(
      BOTH '/' FROM
      CASE
        WHEN LOCATE('://', SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)) > 0 THEN
          SUBSTRING(
            SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1),
            LOCATE('://', SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)) + 3
          )
        ELSE SUBSTRING_INDEX(TRIM(COALESCE(e.mcap_path, '')), '?', 1)
      END
    ) AS object_path
  FROM episodes e
  WHERE e.deleted_at IS NULL
),
path_segments AS (
  SELECT
    id,
    episode_id,
    raw_mcap_path,
    object_path,
    CASE
      WHEN object_path = '' THEN 0
      ELSE LENGTH(object_path) - LENGTH(REPLACE(object_path, '/', '')) + 1
    END AS segment_count
  FROM normalized_paths
)
SELECT
  id,
  episode_id,
  segment_count,
  raw_mcap_path
FROM path_segments
WHERE NOT (
  (segment_count >= 5 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap')
  OR (segment_count = 4 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap')
)
ORDER BY id ASC;
SQL

  echo
  echo "Unparsed episode paths:"
  mysql_exec "${UNPARSED_SQL}"
fi
