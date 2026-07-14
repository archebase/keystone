#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACKAGE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKSPACE_DIR="$(cd "${PACKAGE_DIR}/.." && pwd)"
DEFAULT_DATA_PLATFORM_ROOT="$(cd "${WORKSPACE_DIR}/data-platform" 2>/dev/null && pwd || true)"
DATA_PLATFORM_ROOT="${DATA_PLATFORM_ROOT:-$DEFAULT_DATA_PLATFORM_ROOT}"
DATA_PLATFORM_PROTO_ROOT="${DATA_PLATFORM_PROTO_ROOT:-${DATA_PLATFORM_ROOT}/common/proto}"

AUTH_ENDPOINT="${DGW_LOCAL_AUTH_ENDPOINT:-http://127.0.0.1:15055}"
AUTH_ADMIN_ENDPOINT="${DGW_LOCAL_AUTH_ADMIN_ENDPOINT:-http://127.0.0.1:15054}"
META_ENDPOINT="${DGW_LOCAL_META_ENDPOINT:-http://127.0.0.1:15052}"
GATEWAY_ENDPOINT="${DGW_LOCAL_GATEWAY_ENDPOINT:-http://127.0.0.1:15053}"
INIT_ENDPOINT="${DGW_LOCAL_INIT_ENDPOINT:-http://127.0.0.1:15057}"
GATEWAY_HTTP_BASE="${DGW_LOCAL_GATEWAY_HTTP_BASE:-http://127.0.0.1:18098}"
PERSIST_ROOT="${DGW_LOCAL_PERSIST_ROOT:-$(mktemp -d /tmp/swift-dgw-local.XXXXXX)}"
BOOTSTRAP_ORG="${DGW_LOCAL_BOOTSTRAP_ORGANIZATION:-system}"
BOOTSTRAP_ADMIN_USER="${DGW_LOCAL_BOOTSTRAP_ADMIN_USER:-admin}"
BOOTSTRAP_ADMIN_PASSWORD="${DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD:-${LOCAL_SYSTEM_ADMIN_PASSWORD:-}}"
BOOTSTRAP_RUN_SUFFIX="${DGW_LOCAL_BOOTSTRAP_RUN_SUFFIX:-$(date +%Y%m%d%H%M%S)-$$}"
BOOTSTRAP_SITE_NAME="${DGW_LOCAL_BOOTSTRAP_SITE_NAME:-swift-local-site-${BOOTSTRAP_RUN_SUFFIX}}"
BOOTSTRAP_SITE_STATUS="${DGW_LOCAL_BOOTSTRAP_SITE_STATUS:-1}"
BOOTSTRAP_DEVICE_DISPLAY_NAME="${DGW_LOCAL_BOOTSTRAP_DEVICE_DISPLAY_NAME:-swift-local-device-${BOOTSTRAP_RUN_SUFFIX}}"
BOOTSTRAP_DEVICE_DESCRIPTION="${DGW_LOCAL_BOOTSTRAP_DEVICE_DESCRIPTION:-Swift local gateway init device}"
BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME="${DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME:-swift-local-unbound-device-${BOOTSTRAP_RUN_SUFFIX}}"
BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION="${DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION:-Swift local unbound gateway init device}"
BOOTSTRAP_SUITE_DISPLAY_NAME="${DGW_LOCAL_BOOTSTRAP_SUITE_DISPLAY_NAME:-swift-local-suite-${BOOTSTRAP_RUN_SUFFIX}}"
BOOTSTRAP_SUITE_DESCRIPTION="${DGW_LOCAL_BOOTSTRAP_SUITE_DESCRIPTION:-Swift local gateway init suite}"
BOOTSTRAP_API_KEY_SUFFIX="${BOOTSTRAP_RUN_SUFFIX//[^[:alnum:]-]/-}"
BOOTSTRAP_API_KEY_SUFFIX="${BOOTSTRAP_API_KEY_SUFFIX:0:26}"
BOOTSTRAP_API_KEY_ID="${DGW_LOCAL_BOOTSTRAP_API_KEY_ID:-swift-key-${BOOTSTRAP_API_KEY_SUFFIX}}"
BOOTSTRAP_API_KEY_PREFIX="${DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX:-swift-local-${BOOTSTRAP_API_KEY_SUFFIX}}"
BOOTSTRAP_API_KEY_NAME="${DGW_LOCAL_BOOTSTRAP_API_KEY_NAME:-$BOOTSTRAP_API_KEY_PREFIX}"
BOOTSTRAP_API_KEY_STATUS="${DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS:-1}"
BOOTSTRAP_CSRF_ORIGIN="${DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN:-$GATEWAY_HTTP_BASE}"
CURL_CONNECT_TIMEOUT_SECONDS="${DGW_LOCAL_BOOTSTRAP_CONNECT_TIMEOUT_SECONDS:-3}"
CURL_MAX_TIME_SECONDS="${DGW_LOCAL_BOOTSTRAP_MAX_TIME_SECONDS:-10}"
OPERATION_API_BASE="${DGW_LOCAL_OPERATION_API_BASE:-/api/operation/v1}"

usage() {
  cat <<'EOF'
Usage: Scripts/local_integration_bootstrap.sh [--start-stack] [--run-tests] [--print-env-only]

Options:
  --start-stack      Build and deploy the local Rust stack before bootstrapping credentials.
  --run-tests        Run `DATA_GATEWAY_CLIENT_USE_MOCK_OSS=1 DGW_LOCAL_RUNTIME_INTEGRATION=1 swift test --filter LocalStackHarnessTests` after exporting env.
  --print-env-only   Only print the resolved export commands and skip HTTP bootstrap.
  -h, --help         Show this help.

Environment overrides:
  DGW_LOCAL_AUTH_ENDPOINT
  DGW_LOCAL_AUTH_ADMIN_ENDPOINT
  DGW_LOCAL_META_ENDPOINT
  DGW_LOCAL_GATEWAY_ENDPOINT
  DGW_LOCAL_INIT_ENDPOINT
  DGW_LOCAL_GATEWAY_HTTP_BASE
  DGW_LOCAL_PERSIST_ROOT
  DGW_LOCAL_BOOTSTRAP_ORGANIZATION
  DGW_LOCAL_BOOTSTRAP_ADMIN_USER
  DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD
  DGW_LOCAL_BOOTSTRAP_RUN_SUFFIX
  DGW_LOCAL_BOOTSTRAP_SITE_NAME
  DGW_LOCAL_BOOTSTRAP_SITE_STATUS
  DGW_LOCAL_BOOTSTRAP_DEVICE_DISPLAY_NAME
  DGW_LOCAL_BOOTSTRAP_DEVICE_DESCRIPTION
  DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME
  DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION
  DGW_LOCAL_BOOTSTRAP_SUITE_DISPLAY_NAME
  DGW_LOCAL_BOOTSTRAP_SUITE_DESCRIPTION
  DGW_LOCAL_BOOTSTRAP_API_KEY_ID
  DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX
  DGW_LOCAL_BOOTSTRAP_API_KEY_NAME
  DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS
  DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN
  DGW_LOCAL_BOOTSTRAP_CONNECT_TIMEOUT_SECONDS
  DGW_LOCAL_BOOTSTRAP_MAX_TIME_SECONDS
  DGW_LOCAL_OPERATION_API_BASE
  DATA_PLATFORM_ROOT (required for --start-stack and grpcurl bootstrap; defaults to ../data-platform)
  DATA_PLATFORM_PROTO_ROOT (defaults to DATA_PLATFORM_ROOT/common/proto)

Notes:
  - The script uses the HTTP admin gateway when DGW_LOCAL_GATEWAY_HTTP_BASE points at data-platform-gateway.
  - When DGW_LOCAL_GATEWAY_HTTP_BASE points at data-gateway or is unavailable, the script falls back to grpcurl against AdminAuthService and DeviceManagementService.
  - The local stack should run with DATA_GATEWAY_USE_MOCK_STS=true so CreateLogicalUpload/ReissueUploadCredentials do not require real Aliyun STS.
EOF
}

START_STACK=0
RUN_TESTS=0
PRINT_ENV_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --start-stack)
      START_STACK=1
      ;;
    --run-tests)
      RUN_TESTS=1
      ;;
    --print-env-only)
      PRINT_ENV_ONLY=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
  shift
done

json_string() {
  python3 - <<'PY' "$1"
import json, sys
print(json.dumps(sys.argv[1]))
PY
}

read_json_field() {
  python3 - <<'PY' "$1" "$2"
import json, sys
payload = json.loads(sys.argv[1])
value = payload
for key in sys.argv[2].split('.'):
    if not isinstance(value, dict):
        value = ""
        break
    value = value.get(key, "")
if value is None:
    value = ""
print(value)
PY
}

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

require_data_platform_root() {
  if [[ -z "$DATA_PLATFORM_ROOT" || ! -d "$DATA_PLATFORM_PROTO_ROOT" ]]; then
    echo "DATA_PLATFORM_ROOT must point to a data-platform checkout for local integration bootstrap." >&2
    exit 1
  fi
}

json_array_get() {
  python3 - <<'PY' "$1" "$2" "$3"
import json, sys
payload = json.loads(sys.argv[1])
items = payload
for key in sys.argv[2].split('.'):
    if isinstance(items, dict):
        items = items.get(key, [])
    else:
        items = []
        break
idx = int(sys.argv[3])
try:
    value = items[idx]
except Exception:
    value = ""
print(json.dumps(value) if isinstance(value, (dict, list)) else ("" if value is None else value))
PY
}

http_post() {
  local url="$1"
  local body="$2"
  shift 2
  curl -sS -X POST "$url" \
    --connect-timeout "$CURL_CONNECT_TIMEOUT_SECONDS" \
    --max-time "$CURL_MAX_TIME_SECONDS" \
    -H 'Content-Type: application/json' \
    "$@" \
    --data "$body"
}

grpc_plain_target() {
  local endpoint="$1"
  endpoint="${endpoint#http://}"
  endpoint="${endpoint#https://}"
  printf '%s\n' "$endpoint"
}

grpc_call() {
  require_data_platform_root
  local endpoint="$1"
  local method="$2"
  local body="$3"
  shift 3
  grpcurl -plaintext \
    -import-path "${DATA_PLATFORM_PROTO_ROOT}" \
    -proto common.proto \
    -proto auth.proto \
    -proto dataplatform/device.proto \
    "$@" \
    -d "$body" \
    "$(grpc_plain_target "$endpoint")" \
    "$method"
}

admin_bearer_token() {
  local body response token
  body=$(cat <<EOF
{"organization":$(json_string "$BOOTSTRAP_ORG"),"userName":$(json_string "$BOOTSTRAP_ADMIN_USER"),"password":$(json_string "$BOOTSTRAP_ADMIN_PASSWORD")}
EOF
)
  response=$(grpc_call "$AUTH_ENDPOINT" archebase.auth.v1.AuthService/Login "$body")
  token=$(read_json_field "$response" "accessToken")
  if [[ -z "$token" ]]; then
    token=$(read_json_field "$response" "access_token")
  fi
  if [[ -z "$token" ]]; then
    echo "failed to login admin through auth grpc: $response" >&2
    exit 1
  fi
  printf '%s\n' "$token"
}

bootstrap_devices_via_grpc() {
  if ! command_exists grpcurl; then
    echo "grpcurl is required when HTTP device routes are unavailable" >&2
    exit 1
  fi

  local credential_base64="$1"
  local site_id="$2"
  local admin_token suite_body suite_response suite_name
  local device_body device_response device_name device_id
  local unbound_body unbound_response unbound_name unbound_device_id
  local remove_device_body remove_device_response
  admin_token=$(admin_bearer_token)

  suite_body=$(cat <<EOF
{"siteId":$(json_string "$site_id"),"displayName":$(json_string "$BOOTSTRAP_SUITE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_SUITE_DESCRIPTION")}
EOF
)
  suite_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/CreateDeviceSuite "$suite_body" -H "Authorization: Bearer ${admin_token}")
  suite_name=$(read_json_field "$suite_response" "name")
  if [[ -z "$suite_name" ]]; then
    echo "failed to create device suite through grpc: $suite_response" >&2
    exit 1
  fi

  device_body=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_DEVICE_DESCRIPTION"),"suite":$(json_string "$suite_name")}
EOF
)
  device_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RegisterDevice "$device_body" -H "Authorization: Bearer ${admin_token}")
  device_name=$(read_json_field "$device_response" "name")
  if [[ -z "$device_name" ]]; then
    echo "failed to register device through grpc: $device_response" >&2
    exit 1
  fi
  device_id="${device_name#devices/}"

  unbound_body=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION"),"suite":$(json_string "$suite_name")}
EOF
)
  unbound_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RegisterDevice "$unbound_body" -H "Authorization: Bearer ${admin_token}")
  unbound_name=$(read_json_field "$unbound_response" "name")
  if [[ -z "$unbound_name" ]]; then
    echo "failed to register unbound device through grpc: $unbound_response" >&2
    exit 1
  fi
  unbound_device_id="${unbound_name#devices/}"

  remove_device_body=$(cat <<EOF
{"suite":$(json_string "$suite_name"),"device":$(json_string "$unbound_name")}
EOF
)
  remove_device_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RemoveDeviceFromSuite "$remove_device_body" -H "Authorization: Bearer ${admin_token}")
  if [[ -n "$remove_device_response" && "$remove_device_response" != "{}" ]]; then
    echo "failed to unbind device from suite through grpc: $remove_device_response" >&2
    exit 1
  fi

  emit_exports "$credential_base64" "$device_id" "$unbound_device_id"
}

bootstrap_via_grpc() {
  if ! command_exists grpcurl; then
    echo "grpcurl is required when HTTP admin gateway is unavailable" >&2
    exit 1
  fi

  local admin_token site_body site_response site_id api_key_body api_key_response credential_base64
  local suite_body suite_response suite_name
  local device_body device_response device_name device_id unbound_body unbound_response unbound_name unbound_device_id
  local remove_device_body remove_device_response
  admin_token=$(admin_bearer_token)

  site_body=$(cat <<EOF
{"name":$(json_string "$BOOTSTRAP_SITE_NAME"),"status":${BOOTSTRAP_SITE_STATUS}}
EOF
)
  site_response=$(grpc_call "$AUTH_ADMIN_ENDPOINT" archebase.auth.v1.AdminAuthService/CreateSite "$site_body" -H "Authorization: Bearer ${admin_token}")
  site_id=$(read_json_field "$site_response" "site.siteId")
  if [[ -z "$site_id" ]]; then
    site_id=$(read_json_field "$site_response" "site.site_id")
  fi
  if [[ -z "$site_id" ]]; then
    echo "failed to create site through grpc: $site_response" >&2
    exit 1
  fi

  api_key_body=$(cat <<EOF
{"siteId":$(json_string "$site_id"),"keyName":$(json_string "$BOOTSTRAP_API_KEY_NAME"),"status":${BOOTSTRAP_API_KEY_STATUS}}
EOF
)
  api_key_response=$(grpc_call "$AUTH_ADMIN_ENDPOINT" archebase.auth.v1.AdminAuthService/CreateSiteApiKey "$api_key_body" -H "Authorization: Bearer ${admin_token}")
  credential_base64=$(read_json_field "$api_key_response" "credential")
  if [[ -z "$credential_base64" ]]; then
    credential_base64=$(read_json_field "$api_key_response" "credentialBase64")
  fi
  if [[ -z "$credential_base64" ]]; then
    credential_base64=$(read_json_field "$api_key_response" "credential_base64")
  fi
  if [[ -z "$credential_base64" ]]; then
    echo "failed to create site api key through grpc: $api_key_response" >&2
    exit 1
  fi

  suite_body=$(cat <<EOF
{"siteId":$(json_string "$site_id"),"displayName":$(json_string "$BOOTSTRAP_SUITE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_SUITE_DESCRIPTION")}
EOF
)
  suite_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/CreateDeviceSuite "$suite_body" -H "Authorization: Bearer ${admin_token}")
  suite_name=$(read_json_field "$suite_response" "name")
  if [[ -z "$suite_name" ]]; then
    echo "failed to create device suite through grpc: $suite_response" >&2
    exit 1
  fi

  device_body=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_DEVICE_DESCRIPTION"),"suite":$(json_string "$suite_name")}
EOF
)
  device_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RegisterDevice "$device_body" -H "Authorization: Bearer ${admin_token}")
  device_name=$(read_json_field "$device_response" "name")
  if [[ -z "$device_name" ]]; then
    echo "failed to register device through grpc: $device_response" >&2
    exit 1
  fi
  device_id="${device_name#devices/}"

  unbound_body=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION"),"suite":$(json_string "$suite_name")}
EOF
)
  unbound_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RegisterDevice "$unbound_body" -H "Authorization: Bearer ${admin_token}")
  unbound_name=$(read_json_field "$unbound_response" "name")
  if [[ -z "$unbound_name" ]]; then
    echo "failed to register unbound device through grpc: $unbound_response" >&2
    exit 1
  fi
  unbound_device_id="${unbound_name#devices/}"

  remove_device_body=$(cat <<EOF
{"suite":$(json_string "$suite_name"),"device":$(json_string "$unbound_name")}
EOF
)
  remove_device_response=$(grpc_call "$META_ENDPOINT" archebase.meta.v1.DeviceManagementService/RemoveDeviceFromSuite "$remove_device_body" -H "Authorization: Bearer ${admin_token}")
  if [[ -n "$remove_device_response" && "$remove_device_response" != "{}" ]]; then
    echo "failed to unbind device from suite through grpc: $remove_device_response" >&2
    exit 1
  fi

  emit_exports "$credential_base64" "$device_id" "$unbound_device_id"
}

emit_exports() {
  local credential_base64="$1"
  local device_id="$2"
  local unbound_device_id="$3"
  cat <<EOF
Swift Data Gateway Client local integration test bootstrap completed

export DGW_LOCAL_AUTH_ENDPOINT='${AUTH_ENDPOINT}'
export DGW_LOCAL_AUTH_ADMIN_ENDPOINT='${AUTH_ADMIN_ENDPOINT}'
export DGW_LOCAL_META_ENDPOINT='${META_ENDPOINT}'
export DGW_LOCAL_GATEWAY_ENDPOINT='${GATEWAY_ENDPOINT}'
export DGW_LOCAL_INIT_ENDPOINT='${INIT_ENDPOINT}'
export DGW_LOCAL_GATEWAY_HTTP_BASE='${GATEWAY_HTTP_BASE}'
export DGW_LOCAL_CREDENTIAL_BASE64='${credential_base64}'
export DGW_LOCAL_DEVICE_ID='${device_id}'
export DGW_LOCAL_UNBOUND_DEVICE_ID='${unbound_device_id}'
export DGW_LOCAL_PERSIST_ROOT='${PERSIST_ROOT}'

# optional overrides retained for repeatable CI/bootstrap
export DGW_LOCAL_BOOTSTRAP_ORGANIZATION='${BOOTSTRAP_ORG}'
export DGW_LOCAL_BOOTSTRAP_ADMIN_USER='${BOOTSTRAP_ADMIN_USER}'
export DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD='${BOOTSTRAP_ADMIN_PASSWORD}'
export DGW_LOCAL_BOOTSTRAP_RUN_SUFFIX='${BOOTSTRAP_RUN_SUFFIX}'
export DGW_LOCAL_BOOTSTRAP_SITE_NAME='${BOOTSTRAP_SITE_NAME}'
export DGW_LOCAL_BOOTSTRAP_SITE_STATUS='${BOOTSTRAP_SITE_STATUS}'
export DGW_LOCAL_BOOTSTRAP_DEVICE_DISPLAY_NAME='${BOOTSTRAP_DEVICE_DISPLAY_NAME}'
export DGW_LOCAL_BOOTSTRAP_DEVICE_DESCRIPTION='${BOOTSTRAP_DEVICE_DESCRIPTION}'
export DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME='${BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME}'
export DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION='${BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION}'
export DGW_LOCAL_BOOTSTRAP_SUITE_DISPLAY_NAME='${BOOTSTRAP_SUITE_DISPLAY_NAME}'
export DGW_LOCAL_BOOTSTRAP_SUITE_DESCRIPTION='${BOOTSTRAP_SUITE_DESCRIPTION}'
export DGW_LOCAL_BOOTSTRAP_API_KEY_ID='${BOOTSTRAP_API_KEY_ID}'
export DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX='${BOOTSTRAP_API_KEY_PREFIX}'
export DGW_LOCAL_BOOTSTRAP_API_KEY_NAME='${BOOTSTRAP_API_KEY_NAME}'
export DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS='${BOOTSTRAP_API_KEY_STATUS}'
export DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN='${BOOTSTRAP_CSRF_ORIGIN}'

# local stack note
export DATA_GATEWAY_USE_MOCK_STS='${DATA_GATEWAY_USE_MOCK_STS:-true}'

# init endpoint note
# For manual macOS resolver tests, temporarily point init-device.platform.archebase.ai at localhost, then remove the resolver entry and flush the DNS cache.

# stack bootstrap command
export LOCAL_SYSTEM_ADMIN_PASSWORD='${BOOTSTRAP_ADMIN_PASSWORD}'
export DATA_PLATFORM_ROOT='${DATA_PLATFORM_ROOT}'
"\$DATA_PLATFORM_ROOT"/scripts/local_run.sh --build debug --deploy --reset-db

# test command
cd '${PACKAGE_DIR}'
DATA_GATEWAY_CLIENT_USE_MOCK_OSS=1 DGW_LOCAL_RUNTIME_INTEGRATION=1 swift test --filter LocalStackHarnessTests
EOF

  if [[ $RUN_TESTS -eq 1 ]]; then
    export DGW_LOCAL_AUTH_ENDPOINT="$AUTH_ENDPOINT"
    export DGW_LOCAL_AUTH_ADMIN_ENDPOINT="$AUTH_ADMIN_ENDPOINT"
    export DGW_LOCAL_META_ENDPOINT="$META_ENDPOINT"
    export DGW_LOCAL_GATEWAY_ENDPOINT="$GATEWAY_ENDPOINT"
    export DGW_LOCAL_INIT_ENDPOINT="$INIT_ENDPOINT"
    export DGW_LOCAL_GATEWAY_HTTP_BASE="$GATEWAY_HTTP_BASE"
    export DGW_LOCAL_CREDENTIAL_BASE64="$credential_base64"
    export DGW_LOCAL_DEVICE_ID="$device_id"
    export DGW_LOCAL_UNBOUND_DEVICE_ID="$unbound_device_id"
    export DGW_LOCAL_PERSIST_ROOT="$PERSIST_ROOT"
    export DGW_LOCAL_BOOTSTRAP_ORGANIZATION="$BOOTSTRAP_ORG"
    export DGW_LOCAL_BOOTSTRAP_ADMIN_USER="$BOOTSTRAP_ADMIN_USER"
    export DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD="$BOOTSTRAP_ADMIN_PASSWORD"
    export DGW_LOCAL_BOOTSTRAP_RUN_SUFFIX="$BOOTSTRAP_RUN_SUFFIX"
    export DGW_LOCAL_BOOTSTRAP_SITE_NAME="$BOOTSTRAP_SITE_NAME"
    export DGW_LOCAL_BOOTSTRAP_SITE_STATUS="$BOOTSTRAP_SITE_STATUS"
    export DGW_LOCAL_BOOTSTRAP_DEVICE_DISPLAY_NAME="$BOOTSTRAP_DEVICE_DISPLAY_NAME"
    export DGW_LOCAL_BOOTSTRAP_DEVICE_DESCRIPTION="$BOOTSTRAP_DEVICE_DESCRIPTION"
    export DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME="$BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME"
    export DGW_LOCAL_BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION="$BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION"
    export DGW_LOCAL_BOOTSTRAP_SUITE_DISPLAY_NAME="$BOOTSTRAP_SUITE_DISPLAY_NAME"
    export DGW_LOCAL_BOOTSTRAP_SUITE_DESCRIPTION="$BOOTSTRAP_SUITE_DESCRIPTION"
    export DGW_LOCAL_BOOTSTRAP_API_KEY_ID="$BOOTSTRAP_API_KEY_ID"
    export DGW_LOCAL_BOOTSTRAP_API_KEY_PREFIX="$BOOTSTRAP_API_KEY_PREFIX"
    export DGW_LOCAL_BOOTSTRAP_API_KEY_NAME="$BOOTSTRAP_API_KEY_NAME"
    export DGW_LOCAL_BOOTSTRAP_API_KEY_STATUS="$BOOTSTRAP_API_KEY_STATUS"
    export DGW_LOCAL_BOOTSTRAP_CSRF_ORIGIN="$BOOTSTRAP_CSRF_ORIGIN"
    DATA_GATEWAY_CLIENT_USE_MOCK_OSS=1 DGW_LOCAL_RUNTIME_INTEGRATION=1 swift test --filter LocalStackHarnessTests --package-path "${PACKAGE_DIR}"
  fi
}

if [[ $START_STACK -eq 1 ]]; then
  require_data_platform_root
  export DATA_GATEWAY_USE_MOCK_STS="${DATA_GATEWAY_USE_MOCK_STS:-true}"
  export LOCAL_SYSTEM_ADMIN_PASSWORD="${LOCAL_SYSTEM_ADMIN_PASSWORD:-${BOOTSTRAP_ADMIN_PASSWORD:-LocalAdminPass123!}}"
  "${DATA_PLATFORM_ROOT}/scripts/local_run.sh" --build debug --deploy --reset-db
fi

if [[ -z "$BOOTSTRAP_ADMIN_PASSWORD" ]]; then
  echo "DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD or LOCAL_SYSTEM_ADMIN_PASSWORD is required" >&2
  exit 1
fi

if [[ $PRINT_ENV_ONLY -eq 1 ]]; then
  cat <<EOF
export DGW_LOCAL_AUTH_ENDPOINT='${AUTH_ENDPOINT}'
export DGW_LOCAL_AUTH_ADMIN_ENDPOINT='${AUTH_ADMIN_ENDPOINT}'
export DGW_LOCAL_META_ENDPOINT='${META_ENDPOINT}'
export DGW_LOCAL_GATEWAY_ENDPOINT='${GATEWAY_ENDPOINT}'
export DGW_LOCAL_INIT_ENDPOINT='${INIT_ENDPOINT}'
export DGW_LOCAL_GATEWAY_HTTP_BASE='${GATEWAY_HTTP_BASE}'
export DGW_LOCAL_PERSIST_ROOT='${PERSIST_ROOT}'
export DGW_LOCAL_BOOTSTRAP_ADMIN_PASSWORD='${BOOTSTRAP_ADMIN_PASSWORD}'
EOF
  exit 0
fi

curl -fsS \
  --connect-timeout "$CURL_CONNECT_TIMEOUT_SECONDS" \
  --max-time "$CURL_MAX_TIME_SECONDS" \
  "${GATEWAY_HTTP_BASE%/}/healthz" >/dev/null

LOGIN_ROUTE_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' \
  --connect-timeout "$CURL_CONNECT_TIMEOUT_SECONDS" \
  --max-time "$CURL_MAX_TIME_SECONDS" \
  "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/auth/login" || true)
case "$LOGIN_ROUTE_STATUS" in
  200|204|400|401|403|405)
    ;;
  *)
    echo "HTTP admin gateway routes are unavailable at ${GATEWAY_HTTP_BASE}; falling back to grpc bootstrap" >&2
    bootstrap_via_grpc
    exit 0
    ;;
esac

LOGIN_BODY=$(cat <<EOF
{"organization":$(json_string "$BOOTSTRAP_ORG"),"userName":$(json_string "$BOOTSTRAP_ADMIN_USER"),"password":$(json_string "$BOOTSTRAP_ADMIN_PASSWORD")}
EOF
)

COOKIE_JAR="$(mktemp /tmp/swift-dgw-cookie.XXXXXX)"
trap 'rm -f "$COOKIE_JAR"' EXIT

LOGIN_RESPONSE=$(curl -sS -c "$COOKIE_JAR" -X POST "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/auth/login" \
  --connect-timeout "$CURL_CONNECT_TIMEOUT_SECONDS" \
  --max-time "$CURL_MAX_TIME_SECONDS" \
  -H 'Content-Type: application/json' \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  --data "$LOGIN_BODY")

TOKEN_TYPE=$(read_json_field "$LOGIN_RESPONSE" "tokenType")
if [[ "$TOKEN_TYPE" != "Bearer" ]]; then
  echo "bootstrap login failed: $LOGIN_RESPONSE" >&2
  echo "falling back to grpc bootstrap" >&2
  bootstrap_via_grpc
  exit 0
fi

SITE_BODY=$(cat <<EOF
{"name":$(json_string "$BOOTSTRAP_SITE_NAME"),"status":${BOOTSTRAP_SITE_STATUS}}
EOF
)
SITE_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/sites" "$SITE_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
SITE_ID=$(read_json_field "$SITE_RESPONSE" "site.siteId")
if [[ -z "$SITE_ID" ]]; then
  echo "failed to create site: $SITE_RESPONSE" >&2
  exit 1
fi

API_KEY_BODY=$(cat <<EOF
{"keyName":$(json_string "$BOOTSTRAP_API_KEY_NAME"),"status":${BOOTSTRAP_API_KEY_STATUS}}
EOF
)
API_KEY_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/sites/${SITE_ID}/api-keys" "$API_KEY_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
CREDENTIAL_BASE64=$(read_json_field "$API_KEY_RESPONSE" "credential")
if [[ -z "$CREDENTIAL_BASE64" ]]; then
  CREDENTIAL_BASE64=$(read_json_field "$API_KEY_RESPONSE" "credentialBase64")
fi
if [[ -z "$CREDENTIAL_BASE64" ]]; then
  echo "failed to create api key: $API_KEY_RESPONSE" >&2
  exit 1
fi

SUITE_BODY=$(cat <<EOF
{"siteId":$(json_string "$SITE_ID"),"displayName":$(json_string "$BOOTSTRAP_SUITE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_SUITE_DESCRIPTION")}
EOF
)
SUITE_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/deviceSuites" "$SUITE_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
SUITE_NAME=$(read_json_field "$SUITE_RESPONSE" "name")
if [[ -z "$SUITE_NAME" ]]; then
  echo "failed to create device suite: $SUITE_RESPONSE" >&2
  exit 1
fi

DEVICE_BODY=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_DEVICE_DESCRIPTION"),"suite":$(json_string "$SUITE_NAME")}
EOF
)
DEVICE_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/devices:register" "$DEVICE_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
DEVICE_NAME=$(read_json_field "$DEVICE_RESPONSE" "name")
if [[ -z "$DEVICE_NAME" ]]; then
  DEVICE_ERROR_CODE=$(read_json_field "$DEVICE_RESPONSE" "code")
  if [[ "$DEVICE_ERROR_CODE" == "5" ]]; then
    echo "HTTP device routes are unavailable at ${GATEWAY_HTTP_BASE}; falling back to grpc device bootstrap" >&2
    bootstrap_devices_via_grpc "$CREDENTIAL_BASE64" "$SITE_ID"
    exit 0
  fi
  echo "failed to register device: $DEVICE_RESPONSE" >&2
  exit 1
fi
DEVICE_ID="${DEVICE_NAME#devices/}"

UNBOUND_DEVICE_BODY=$(cat <<EOF
{"displayName":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DISPLAY_NAME"),"description":$(json_string "$BOOTSTRAP_UNBOUND_DEVICE_DESCRIPTION"),"suite":$(json_string "$SUITE_NAME")}
EOF
)
UNBOUND_DEVICE_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/devices:register" "$UNBOUND_DEVICE_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
UNBOUND_DEVICE_NAME=$(read_json_field "$UNBOUND_DEVICE_RESPONSE" "name")
if [[ -z "$UNBOUND_DEVICE_NAME" ]]; then
  echo "failed to register unbound device: $UNBOUND_DEVICE_RESPONSE" >&2
  exit 1
fi
UNBOUND_DEVICE_ID="${UNBOUND_DEVICE_NAME#devices/}"

REMOVE_DEVICE_BODY=$(cat <<EOF
{"device":$(json_string "$UNBOUND_DEVICE_NAME")}
EOF
)
REMOVE_DEVICE_RESPONSE=$(http_post "${GATEWAY_HTTP_BASE%/}${OPERATION_API_BASE}/${SUITE_NAME}:removeDevice" "$REMOVE_DEVICE_BODY" \
  -H "Origin: ${BOOTSTRAP_CSRF_ORIGIN}" \
  -b "$COOKIE_JAR")
if [[ -n "$REMOVE_DEVICE_RESPONSE" && "$REMOVE_DEVICE_RESPONSE" != "{}" ]]; then
  echo "failed to unbind device from suite: $REMOVE_DEVICE_RESPONSE" >&2
  exit 1
fi

emit_exports "$CREDENTIAL_BASE64" "$DEVICE_ID" "$UNBOUND_DEVICE_ID"
