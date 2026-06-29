#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0
#
# Backfill episodes.workstation_id from episodes.mcap_path.
#
# Default mode is dry-run. Use --apply to create historical workstation rows and
# move safe episodes to those rows.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_ENV_FILE="$(cd "${SCRIPT_DIR}/.." && pwd)/.env"

ENV_FILE="${KEYSTONE_ENV_FILE:-${DEFAULT_ENV_FILE}}"
MYSQL_CONTAINER="${KEYSTONE_MYSQL_CONTAINER:-}"
USE_SUDO_DOCKER="${KEYSTONE_DOCKER_SUDO:-0}"
APPLY=0
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
Usage: backfill_episode_workstations_from_mcap.sh [options]

Options:
  --device-id DEVICE_ID  Limit dry-run/apply to one parsed device ID
  --apply                Apply safe backfill changes; default is dry-run only
  --csv                  Print tab-separated output suitable for import
  --env-file PATH        Load Keystone env vars from PATH (default: keystone/.env)
  --docker-container NAME
                       Run mysql inside this Docker container
  --sudo-docker          Use sudo docker for Docker fallback
  -h, --help             Show this help

Backfill behavior:
  - Parses device ID from episodes.mcap_path.
  - Keeps the original episode's collector from its current workstation.
  - Creates/reuses non-current historical workstations for safe mismatches.
  - Updates only episodes, not tasks or batches.
  - Blocks ambiguous rows such as missing workstation, missing robot, or factory mismatch.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --device-id)
      if [[ $# -lt 2 ]]; then
        echo "error: --device-id requires a value" >&2
        exit 1
      fi
      DEVICE_ID="$2"
      shift 2
      ;;
    --apply)
      APPLY=1
      shift
      ;;
    --csv)
      CSV=1
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
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -n "${DEVICE_ID}" && ! "${DEVICE_ID}" =~ ^[A-Za-z0-9_.:-]+$ ]]; then
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

DEVICE_FILTER_SQL="NULL"
if [[ -n "${DEVICE_ID}" ]]; then
  DEVICE_FILTER_SQL="'${DEVICE_ID}'"
fi

read -r -d '' PREPARE_SQL <<SQL || true
SET @device_filter := ${DEVICE_FILTER_SQL};

DROP TEMPORARY TABLE IF EXISTS tmp_backfill_parsed;
DROP TEMPORARY TABLE IF EXISTS tmp_backfill_candidates;
DROP TEMPORARY TABLE IF EXISTS tmp_backfill_target_bindings;

CREATE TEMPORARY TABLE tmp_backfill_parsed AS
WITH normalized_paths AS (
  SELECT
    e.id,
    e.episode_id,
    e.task_id,
    e.workstation_id,
    e.mcap_path,
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
    *,
    CASE
      WHEN object_path = '' THEN 0
      ELSE LENGTH(object_path) - LENGTH(REPLACE(object_path, '/', '')) + 1
    END AS segment_count
  FROM normalized_paths
)
SELECT
  *,
  CASE
    WHEN segment_count >= 5 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
      SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 3), '/', -1)
    WHEN segment_count = 4 AND LOWER(SUBSTRING_INDEX(object_path, '/', -1)) LIKE '%.mcap' THEN
      SUBSTRING_INDEX(SUBSTRING_INDEX(object_path, '/', 2), '/', -1)
    ELSE NULL
  END AS parsed_device_id
FROM path_segments;

CREATE TEMPORARY TABLE tmp_backfill_candidates AS
SELECT
  pe.id AS episode_pk,
  pe.episode_id,
  pe.task_id,
  pe.created_at AS episode_created_at,
  pe.mcap_path,
  pe.parsed_device_id,
  pe.workstation_id AS source_workstation_id,
  ws.robot_id AS current_robot_id,
  ws.robot_serial AS current_robot_serial,
  ws.data_collector_id,
  ws.collector_name,
  ws.collector_operator_id,
  ws.factory_id,
  ws.organization_id,
  ws.name AS source_workstation_name,
  ws.status AS source_workstation_status,
  ws.metadata AS source_workstation_metadata,
  tr.id AS target_robot_id,
  tr.device_id AS target_device_id,
  tr.factory_id AS target_factory_id,
  COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), '') AS target_robot_name,
  CASE
    WHEN pe.parsed_device_id IS NULL THEN 'blocked_unparsed_path'
    WHEN ws.id IS NULL THEN 'blocked_missing_workstation'
    WHEN tr.id IS NULL THEN 'blocked_missing_robot'
    WHEN ws.robot_id = tr.id THEN 'noop_already_correct'
    WHEN ws.factory_id <> tr.factory_id THEN 'blocked_factory_mismatch'
    ELSE 'auto_safe'
  END AS backfill_status
FROM tmp_backfill_parsed pe
LEFT JOIN workstations ws
  ON ws.id = pe.workstation_id
 AND ws.deleted_at IS NULL
LEFT JOIN robots tr
  ON tr.device_id = pe.parsed_device_id
 AND tr.deleted_at IS NULL
LEFT JOIN robot_types rt
  ON rt.id = tr.robot_type_id
 AND rt.deleted_at IS NULL
WHERE (@device_filter IS NULL OR pe.parsed_device_id = @device_filter);
SQL

read -r -d '' REPORT_SQL <<'SQL' || true
SELECT
  backfill_status,
  COUNT(*) AS episode_count
FROM tmp_backfill_candidates
GROUP BY backfill_status
ORDER BY episode_count DESC, backfill_status ASC;

SELECT
  parsed_device_id,
  source_workstation_id,
  current_robot_serial,
  target_device_id,
  data_collector_id,
  collector_operator_id,
  collector_name,
  COUNT(*) AS episode_count,
  MIN(episode_created_at) AS first_episode_at,
  MAX(episode_created_at) AS last_episode_at
FROM tmp_backfill_candidates
WHERE backfill_status = 'auto_safe'
GROUP BY
  parsed_device_id,
  source_workstation_id,
  current_robot_serial,
  target_device_id,
  data_collector_id,
  collector_operator_id,
  collector_name
ORDER BY episode_count DESC, parsed_device_id ASC, source_workstation_id ASC;

SELECT
  backfill_status,
  episode_pk,
  episode_id,
  parsed_device_id,
  source_workstation_id,
  current_robot_serial,
  target_device_id,
  data_collector_id,
  collector_operator_id,
  mcap_path
FROM tmp_backfill_candidates
WHERE backfill_status NOT IN ('auto_safe', 'noop_already_correct')
ORDER BY backfill_status ASC, episode_pk ASC
LIMIT 200;
SQL

if [[ "${APPLY}" -eq 0 ]]; then
  echo "[dry-run] No database rows will be modified. Re-run with --apply to update safe rows." >&2
  mysql_exec "${PREPARE_SQL}${REPORT_SQL}"
  exit 0
fi

read -r -d '' APPLY_SQL <<'SQL' || true
START TRANSACTION;

CREATE TEMPORARY TABLE tmp_backfill_target_bindings AS
SELECT
  source_workstation_id,
  target_robot_id,
  target_device_id,
  target_robot_name,
  data_collector_id,
  collector_name,
  collector_operator_id,
  factory_id,
  organization_id,
  source_workstation_name,
  source_workstation_status,
  source_workstation_metadata,
  MIN(episode_created_at) AS first_episode_at,
  MAX(episode_created_at) AS last_episode_at,
  CAST(NULL AS SIGNED) AS workstation_id
FROM tmp_backfill_candidates
WHERE backfill_status = 'auto_safe'
GROUP BY
  source_workstation_id,
  target_robot_id,
  target_device_id,
  target_robot_name,
  data_collector_id,
  collector_name,
  collector_operator_id,
  factory_id,
  organization_id,
  source_workstation_name,
  source_workstation_status,
  source_workstation_metadata;

UPDATE tmp_backfill_target_bindings tb
JOIN workstations ws
  ON ws.robot_id = tb.target_robot_id
 AND ws.data_collector_id = tb.data_collector_id
 AND ws.factory_id = tb.factory_id
 AND ws.organization_id = tb.organization_id
 AND ws.is_current = FALSE
 AND ws.deleted_at IS NULL
SET tb.workstation_id = ws.id;

INSERT INTO workstations (
  robot_id,
  robot_name,
  robot_serial,
  data_collector_id,
  collector_name,
  collector_operator_id,
  factory_id,
  organization_id,
  name,
  status,
  is_current,
  metadata,
  created_at,
  updated_at
)
SELECT
  tb.target_robot_id,
  tb.target_robot_name,
  tb.target_device_id,
  tb.data_collector_id,
  tb.collector_name,
  tb.collector_operator_id,
  tb.factory_id,
  tb.organization_id,
  LEFT(CONCAT(COALESCE(NULLIF(tb.source_workstation_name, ''), 'ws'), '-hist-', tb.target_device_id), 255),
  'inactive',
  FALSE,
  tb.source_workstation_metadata,
  NOW(),
  NOW()
FROM tmp_backfill_target_bindings tb
WHERE tb.workstation_id IS NULL;

UPDATE tmp_backfill_target_bindings tb
JOIN workstations ws
  ON ws.robot_id = tb.target_robot_id
 AND ws.data_collector_id = tb.data_collector_id
 AND ws.factory_id = tb.factory_id
 AND ws.organization_id = tb.organization_id
 AND ws.is_current = FALSE
 AND ws.deleted_at IS NULL
SET tb.workstation_id = ws.id
WHERE tb.workstation_id IS NULL;

UPDATE episodes e
JOIN tmp_backfill_candidates c
  ON c.episode_pk = e.id
JOIN tmp_backfill_target_bindings tb
  ON tb.source_workstation_id = c.source_workstation_id
 AND tb.target_robot_id = c.target_robot_id
 AND tb.data_collector_id = c.data_collector_id
 AND tb.factory_id = c.factory_id
 AND tb.organization_id = c.organization_id
SET
  e.workstation_id = tb.workstation_id,
  e.updated_at = NOW()
WHERE c.backfill_status = 'auto_safe'
  AND tb.workstation_id IS NOT NULL
  AND e.deleted_at IS NULL;

SELECT
  COUNT(*) AS updated_episode_count
FROM tmp_backfill_candidates
WHERE backfill_status = 'auto_safe';

SELECT
  tb.workstation_id,
  tb.target_device_id,
  tb.data_collector_id,
  tb.collector_operator_id,
  COUNT(*) AS moved_episode_count
FROM tmp_backfill_candidates c
JOIN tmp_backfill_target_bindings tb
  ON tb.source_workstation_id = c.source_workstation_id
 AND tb.target_robot_id = c.target_robot_id
 AND tb.data_collector_id = c.data_collector_id
 AND tb.factory_id = c.factory_id
 AND tb.organization_id = c.organization_id
WHERE c.backfill_status = 'auto_safe'
GROUP BY
  tb.workstation_id,
  tb.target_device_id,
  tb.data_collector_id,
  tb.collector_operator_id
ORDER BY moved_episode_count DESC, tb.target_device_id ASC;

COMMIT;
SQL

echo "[apply] Updating safe episode rows. Blocked rows will not be modified." >&2
mysql_exec "${PREPARE_SQL}${REPORT_SQL}${APPLY_SQL}"
