#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PUBLIC_ENDPOINTS_RESOURCE="${DGW_PUBLIC_ENDPOINTS_RESOURCE:-${SDK_DIR}/Sources/DataGatewayClient/Resources/PublicEndpoints.json}"
MARKER_BEGIN="# archebase-swift-sdk-public-dns begin"
MARKER_END="# archebase-swift-sdk-public-dns end"
LOCAL_IP="${DGW_PUBLIC_DNS_LOCAL_IP:-127.0.0.1}"

read_public_endpoint_field() {
  python3 - "$PUBLIC_ENDPOINTS_RESOURCE" "$1" "$2" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
service = sys.argv[2]
field = sys.argv[3]
payload = json.loads(path.read_text())
endpoint = payload[service]
value = endpoint.get(field)
if field == "scheme" and value is None:
    value = endpoint.get("schema")
if value is None:
    raise SystemExit(f"missing {service}.{field} in {path}")
if field == "scheme":
    value = str(value).lower()
print(value)
PY
}

AUTH_SCHEME=""
GATEWAY_SCHEME=""
INIT_SCHEME=""
AUTH_DOMAIN=""
GATEWAY_DOMAIN=""
INIT_DOMAIN=""
AUTH_PORT=""
GATEWAY_PORT=""
INIT_PORT=""
AUTH_TARGET="${DGW_LOCAL_AUTH_ENDPOINT:-127.0.0.1:15055}"
GATEWAY_TARGET="${DGW_LOCAL_GATEWAY_ENDPOINT:-127.0.0.1:15053}"
INIT_TARGET="${DGW_LOCAL_INIT_ENDPOINT:-127.0.0.1:15057}"
CERT_DIR="${DGW_PUBLIC_DNS_CERT_DIR:-${SDK_DIR}/.public-dns}"
CERT_FILE="${CERT_DIR}/archebase-public-domains.crt"
KEY_FILE="${CERT_DIR}/archebase-public-domains.key"
PID_DIR="${CERT_DIR}/pids"

usage() {
  cat <<'USAGE'
Usage: Scripts/public_dns_path_test.sh <command>

Commands:
  prepare-hosts   Add marked /etc/hosts entries for Archebase public SDK domains.
  start-proxies   Start local TCP proxies for auth, gateway, and device init gRPC targets.
  run-tests       Run gated Swift tests through the resource-defined public endpoint SDK path.
  cleanup         Stop proxies and remove marked /etc/hosts entries.

Environment:
  DGW_PUBLIC_DNS_RUN=1 is required for prepare-hosts, start-proxies, and run-tests.
  DGW_PUBLIC_ENDPOINTS_RESOURCE can point to an alternate PublicEndpoints.json.
  DGW_LOCAL_AUTH_ENDPOINT, DGW_LOCAL_GATEWAY_ENDPOINT, and DGW_LOCAL_INIT_ENDPOINT point to local plaintext gRPC targets.
  DGW_LOCAL_CREDENTIAL_BASE64, DGW_LOCAL_DEVICE_ID, and DGW_LOCAL_PERSIST_ROOT are passed through to integration tests.

Notes:
  This script is intentionally gated and does not affect normal swift test runs.
  Endpoint hosts and ports are read from PublicEndpoints.json.
  prepare-hosts may require sudo because it edits /etc/hosts.
  start-proxies requires openssl and socat.
USAGE
}

require_gated() {
  if [[ "${DGW_PUBLIC_DNS_RUN:-}" != "1" ]]; then
    echo "DGW_PUBLIC_DNS_RUN=1 is required for this command" >&2
    exit 2
  fi
}

load_public_endpoints() {
  AUTH_SCHEME="$(read_public_endpoint_field auth scheme)"
  GATEWAY_SCHEME="$(read_public_endpoint_field gateway scheme)"
  INIT_SCHEME="$(read_public_endpoint_field deviceInit scheme)"
  AUTH_DOMAIN="$(read_public_endpoint_field auth host)"
  GATEWAY_DOMAIN="$(read_public_endpoint_field gateway host)"
  INIT_DOMAIN="$(read_public_endpoint_field deviceInit host)"
  AUTH_PORT="$(read_public_endpoint_field auth port)"
  GATEWAY_PORT="$(read_public_endpoint_field gateway port)"
  INIT_PORT="$(read_public_endpoint_field deviceInit port)"
}

normalize_target() {
  local value="$1"
  value="${value#http://}"
  value="${value#https://}"
  printf '%s\n' "$value"
}

ensure_cert() {
  mkdir -p "$CERT_DIR" "$PID_DIR"
  if [[ -f "$CERT_FILE" && -f "$KEY_FILE" ]]; then
    return
  fi
  openssl req -x509 -newkey rsa:2048 -nodes -days 7 \
    -keyout "$KEY_FILE" \
    -out "$CERT_FILE" \
    -subj "/CN=${AUTH_DOMAIN}" \
    -addext "subjectAltName=DNS:${AUTH_DOMAIN},DNS:${GATEWAY_DOMAIN},DNS:${INIT_DOMAIN}"
}

prepare_hosts() {
  require_gated
  load_public_endpoints
  local block
  block="${MARKER_BEGIN}
${LOCAL_IP} ${AUTH_DOMAIN}
${LOCAL_IP} ${GATEWAY_DOMAIN}
${LOCAL_IP} ${INIT_DOMAIN}
${MARKER_END}"
  local current
  current="$(mktemp)"
  cp /etc/hosts "$current"
  python3 - "$current" "$block" <<'PY'
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
block = sys.argv[2]
text = path.read_text()
begin = "# archebase-swift-sdk-public-dns begin"
end = "# archebase-swift-sdk-public-dns end"
while begin in text and end in text:
    start = text.index(begin)
    stop = text.index(end, start) + len(end)
    text = text[:start] + text[stop:]
text = text.rstrip() + "\n" + block + "\n"
path.write_text(text)
PY
  sudo cp "$current" /etc/hosts
  rm -f "$current"
  dscacheutil -flushcache >/dev/null 2>&1 || true
  echo "Installed marked hosts entries for Archebase public SDK domains."
}

cleanup_hosts() {
  local current
  current="$(mktemp)"
  cp /etc/hosts "$current"
  python3 - "$current" <<'PY'
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
text = path.read_text()
begin = "# archebase-swift-sdk-public-dns begin"
end = "# archebase-swift-sdk-public-dns end"
while begin in text and end in text:
    start = text.index(begin)
    stop = text.index(end, start) + len(end)
    text = text[:start] + text[stop:]
path.write_text(text.strip() + "\n")
PY
  sudo cp "$current" /etc/hosts
  rm -f "$current"
  dscacheutil -flushcache >/dev/null 2>&1 || true
}

start_proxy() {
  local name="$1"
  local scheme="$2"
  local listen_port="$3"
  local target="$4"
  local pid_file="${PID_DIR}/${name}.pid"
  if [[ -f "$pid_file" ]] && kill -0 "$(cat "$pid_file")" >/dev/null 2>&1; then
    echo "${name} proxy already running on ${listen_port}"
    return
  fi
  case "$scheme" in
    https)
      socat "OPENSSL-LISTEN:${listen_port},cert=${CERT_FILE},key=${KEY_FILE},reuseaddr,fork" "TCP:$(normalize_target "$target")" &
      ;;
    http)
      socat "TCP-LISTEN:${listen_port},reuseaddr,fork" "TCP:$(normalize_target "$target")" &
      ;;
    *)
      echo "Unsupported scheme for ${name}: ${scheme}" >&2
      exit 2
      ;;
  esac
  echo "$!" > "$pid_file"
  echo "Started ${name} ${scheme} proxy on ${listen_port} -> $(normalize_target "$target")"
}

start_proxies() {
  require_gated
  load_public_endpoints
  command -v socat >/dev/null || { echo "socat is required" >&2; exit 2; }
  if [[ "$AUTH_SCHEME" == "https" || "$GATEWAY_SCHEME" == "https" || "$INIT_SCHEME" == "https" ]]; then
    command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 2; }
    ensure_cert
  fi
  start_proxy auth "$AUTH_SCHEME" "$AUTH_PORT" "$AUTH_TARGET"
  start_proxy gateway "$GATEWAY_SCHEME" "$GATEWAY_PORT" "$GATEWAY_TARGET"
  start_proxy init "$INIT_SCHEME" "$INIT_PORT" "$INIT_TARGET"
  if [[ "$AUTH_SCHEME" == "https" || "$GATEWAY_SCHEME" == "https" || "$INIT_SCHEME" == "https" ]]; then
    echo "Trust ${CERT_FILE} locally before running TLS validation against these proxies."
  fi
}

stop_proxies() {
  if [[ -d "$PID_DIR" ]]; then
    for pid_file in "$PID_DIR"/*.pid; do
      [[ -f "$pid_file" ]] || continue
      local pid
      pid="$(cat "$pid_file")"
      kill "$pid" >/dev/null 2>&1 || true
      rm -f "$pid_file"
    done
  fi
}

run_tests() {
  require_gated
  export DGW_PUBLIC_DNS_INTEGRATION=1
  export DGW_REAL_RUNTIME_INTEGRATION=1
  export DGW_REAL_DEVICE_INIT_INTEGRATION=1
  export DGW_REAL_CREDENTIAL_BASE64="${DGW_LOCAL_CREDENTIAL_BASE64:-${DGW_REAL_CREDENTIAL_BASE64:-}}"
  export DGW_REAL_DEVICE_ID="${DGW_LOCAL_DEVICE_ID:-${DGW_REAL_DEVICE_ID:-}}"
  export DGW_REAL_PERSIST_ROOT="${DGW_LOCAL_PERSIST_ROOT:-${DGW_REAL_PERSIST_ROOT:-$(mktemp -d /tmp/swift-dgw-public-dns.XXXXXX)}}"
  export DGW_OSS_TEST_ENDPOINT="${DGW_OSS_TEST_ENDPOINT:-https://oss-cn-shanghai.aliyuncs.com}"
  export DGW_OSS_TEST_BUCKET="${DGW_OSS_TEST_BUCKET:-public-dns-placeholder}"
  export DGW_OSS_TEST_ACCESS_KEY_ID="${DGW_OSS_TEST_ACCESS_KEY_ID:-placeholder}"
  export DGW_OSS_TEST_ACCESS_KEY_SECRET="${DGW_OSS_TEST_ACCESS_KEY_SECRET:-placeholder}"
  export DGW_OSS_TEST_SECURITY_TOKEN="${DGW_OSS_TEST_SECURITY_TOKEN:-placeholder}"
  export DGW_OSS_TEST_OBJECT_PREFIX="${DGW_OSS_TEST_OBJECT_PREFIX:-swift-public-dns}"
  (cd "$SDK_DIR" && swift test --filter LocalStackHarnessTests)
}

case "${1:-}" in
  prepare-hosts) prepare_hosts ;;
  start-proxies) start_proxies ;;
  run-tests) run_tests ;;
  cleanup) stop_proxies; cleanup_hosts ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; exit 2 ;;
esac
