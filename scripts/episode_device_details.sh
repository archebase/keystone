#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# List episode details for a device by parsing episodes.mcap_path.
#
# Usage:
#   cd keystone
#   ./scripts/episode_device_details.sh AB-F0001-T0004-000001
#   ./scripts/episode_device_details.sh --sudo-docker AB-F0001-T0004-000001

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_ENV_FILE="$(cd "${SCRIPT_DIR}/.." && pwd)/.env"

ENV_FILE="${KEYSTONE_ENV_FILE:-${DEFAULT_ENV_FILE}}"
MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"
USE_SUDO_DOCKER="${KEYSTONE_DOCKER_SUDO:-0}"
CSV=0
DEVICE_ID=""

ENV_KEYSTONE_MYSQL_HOST="${KEYSTONE_MYSQL_HOST:-}"
ENV_KEYSTONE_MYSQL_PORT="${KEYSTONE_MYSQL_PORT:-}"
ENV_KEYSTONE_MYSQL_USER="${KEYSTONE_MYSQL_USER:-}"
ENV_KEYSTONE_MYSQL_PASSWORD="${KEYSTONE_MYSQL_PASSWORD:-}"
ENV_KEYSTONE_MYSQL_DATABASE="${KEYSTONE_MYSQL_DATABASE:-}"
ENV_KEYSTONE_MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"

usage() {
  cat <<'EOF'
Usage: episode_device_details.sh [options] [DEVICE_ID]

Options:
  --env-file PATH        Load Keystone env vars from PATH (default: keystone/.env)
  --csv                  Print tab-separated output suitable for import
  --docker-container NAME
                       Run mysql inside this Docker container
  --sudo-docker          Use sudo docker for Docker fallback
  -h, --help             Show this help

If DEVICE_ID is omitted, the script uses AB-F0001-T0004-000001.
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
      if [[ -n "${DEVICE_ID}" ]]; then
        echo "error: only one DEVICE_ID is allowed" >&2
        exit 1
      fi
      DEVICE_ID="$1"
      shift
      ;;
  esac
done

DEVICE_ID="${DEVICE_ID:-AB-F0001-T0004-000001}"
if [[ ! "${DEVICE_ID}" =~ ^[A-Za-z0-9_.:-]+$ ]]; then
  echo "error: DEVICE_ID contains unsupported characters: ${DEVICE_ID}" >&2
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
    echo "hint: pass --sudo-docker, --docker-container NAME, or install mysql client" >&2
    exit 1
  fi

  docker_cmd exec -e MYSQL_PWD="${KEYSTONE_MYSQL_PASSWORD}" "${container}" \
    mysql "${DOCKER_MYSQL_ARGS[@]}" --execute="${sql}"
}

read -r -d '' DETAILS_SQL <<SQL || true
WITH normalized_paths AS (
  SELECT
    e.id,
    e.episode_id,
    e.task_id,
    e.batch_id,
    e.order_id,
    e.scene_id,
    e.scene_name,
    e.workstation_id,
    e.sop_id,
    e.mcap_path,
    e.sidecar_path,
    e.file_size_bytes,
    e.duration_sec,
    e.qa_status,
    e.cloud_synced,
    e.cloud_synced_at,
    e.dataset_id,
    e.quality_flag,
    e.created_at,
    e.updated_at,
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
    *,
    CASE
      WHEN object_path = '' THEN 0
      ELSE LENGTH(object_path) - LENGTH(REPLACE(object_path, '/', '')) + 1
    END AS segment_count
  FROM normalized_paths
),
parsed_episodes AS (
  SELECT
    *,
    CASE
      WHEN segment_count >= 5 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
        SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 3), '/', -1)
      WHEN segment_count = 4 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
        SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 2), '/', -1)
      ELSE NULL
    END AS parsed_device_id,
    SUBSTRING_INDEX(object_path, '/', -1) AS mcap_filename
  FROM path_segments
)
SELECT
  pe.id,
  pe.episode_id,
  pe.parsed_device_id,
  pe.task_id AS task_pk,
  COALESCE(t.task_id, '') AS task_public_id,
  pe.batch_id,
  pe.order_id,
  pe.scene_id,
  COALESCE(NULLIF(pe.scene_name, ''), NULLIF(t.scene_name, '')) AS scene_name,
  pe.workstation_id,
  pe.sop_id,
  pe.file_size_bytes,
  ROUND(COALESCE(pe.file_size_bytes, 0) / 1024 / 1024 / 1024, 2) AS file_gib,
  pe.duration_sec,
  pe.qa_status,
  pe.cloud_synced,
  pe.cloud_synced_at,
  pe.dataset_id,
  pe.quality_flag,
  pe.created_at,
  pe.updated_at,
  COALESCE(CAST(r.id AS CHAR), '') AS current_robot_id,
  COALESCE(CAST(r.robot_type_id AS CHAR), '') AS current_robot_type_id,
  COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), '') AS current_robot_type,
  pe.mcap_filename,
  pe.mcap_path,
  pe.sidecar_path
FROM parsed_episodes pe
LEFT JOIN tasks t
  ON t.id = pe.task_id
 AND t.deleted_at IS NULL
LEFT JOIN robots r
  ON r.device_id = pe.parsed_device_id
 AND r.deleted_at IS NULL
LEFT JOIN robot_types rt
  ON rt.id = r.robot_type_id
 AND rt.deleted_at IS NULL
WHERE pe.parsed_device_id = '${DEVICE_ID}'
ORDER BY pe.created_at ASC, pe.id ASC;
SQL

mysql_exec "${DETAILS_SQL}"
