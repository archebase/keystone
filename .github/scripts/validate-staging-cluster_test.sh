#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
validator="$script_dir/validate-staging-cluster.py"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT

namespace_json="$fixture_dir/namespace.json"
service_account_json="$fixture_dir/service-account.json"
ingress_class_json="$fixture_dir/ingress-class.json"
alb_instance_json="$fixture_dir/alb-instance.json"
error_output="$fixture_dir/error"

write_namespace() {
  local injection_label="$1"

  printf '%s\n' \
    "{\"metadata\":{\"name\":\"archebase-system\",\"labels\":{\"vke.volcengine.com/pod-identity-injection-enabled\":\"${injection_label}\"}}}" \
    > "$namespace_json"
}

write_service_account() {
  local role_trn="$1"

  printf '%s\n' \
    "{\"metadata\":{\"name\":\"keystone\",\"namespace\":\"archebase-system\",\"annotations\":{\"vke.volcengine.com/role-trn\":\"${role_trn}\"},\"labels\":{\"archebase.io/environment\":\"staging\"}}}" \
    > "$service_account_json"
}

run_resource_validation() {
  python3 "$validator" resources \
    --namespace-json "$namespace_json" \
    --service-account-json "$service_account_json" \
    --ingress-class-json "$ingress_class_json" \
    --alb-instance-json "$alb_instance_json" \
    --namespace archebase-system \
    --service-account keystone \
    --ingress-class keystone-staging \
    --alb-dns-name keystone-staging.example.com \
    --grpc-port 50053 \
    --expected-irsa-role-trn "trn:iam::2117611051:role/archebase-staging-keystone-tos-irsa"
}

expect_failure() {
  local name="$1"
  local expected_error="$2"
  shift 2

  if "$@" 2> "$error_output"; then
    echo "$name unexpectedly succeeded" >&2
    exit 1
  fi
  if ! grep -Fq "$expected_error" "$error_output"; then
    echo "$name returned an unexpected error:" >&2
    sed 's/^/  /' "$error_output" >&2
    exit 1
  fi
}

python3 "$validator" target --context volcano-staging --cluster volcano-staging
expect_failure \
  "staging context with production cluster" \
  "kubeconfig cluster must identify staging" \
  python3 "$validator" target --context volcano-staging --cluster volcano-prod
expect_failure \
  "production context with staging cluster" \
  "kubeconfig context must identify staging" \
  python3 "$validator" target --context volcano-prod --cluster volcano-staging

write_namespace true
write_service_account "trn:iam::2117611051:role/archebase-staging-keystone-tos-irsa"
printf '%s\n' \
  '{"metadata":{"name":"keystone-staging"},"spec":{"controller":"ingress.vke.volcengine.com/alb","parameters":{"apiGroup":"loadbalancer.vke.volcengine.com","kind":"ALBInstance","scope":"Cluster","name":"keystone-staging"}}}' \
  > "$ingress_class_json"
printf '%s\n' \
  '{"metadata":{"name":"keystone-staging"},"spec":{"listeners":[{"port":443,"protocol":"HTTPS","enableHTTP2":true},{"port":50053,"protocol":"HTTPS","enableHTTP2":true}]},"status":{"phase":"Running","edition":"Standard","ingress":{"hostname":"keystone-staging.example.com"}}}' \
  > "$alb_instance_json"

test "$(python3 "$validator" ingress-alb-name --ingress-class-json "$ingress_class_json" --expected-name keystone-staging)" = "keystone-staging"
run_resource_validation

write_namespace false
expect_failure \
  "namespace without pod identity injection" \
  "staging namespace must enable VKE pod identity injection" \
  run_resource_validation

write_namespace true
write_service_account "trn:iam::2117611051:role/archebase-prod-keystone-tos-irsa"
expect_failure \
  "production IRSA role" \
  "staging Keystone ServiceAccount IRSA role does not match STAGING_KEYSTONE_IRSA_ROLE_TRN" \
  run_resource_validation

write_service_account "trn:iam::2117611051:role/archebase-staging-keystone-upload"
expect_failure \
  "non-base staging IRSA role" \
  "staging Keystone ServiceAccount IRSA role does not match STAGING_KEYSTONE_IRSA_ROLE_TRN" \
  run_resource_validation

write_service_account "trn:iam::9999999999:role/archebase-staging-keystone-tos-irsa"
expect_failure \
  "staging IRSA role from the wrong account" \
  "staging Keystone ServiceAccount IRSA role does not match STAGING_KEYSTONE_IRSA_ROLE_TRN" \
  run_resource_validation

echo "staging cluster preflight tests passed"
