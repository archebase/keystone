#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
resolver="$script_dir/resolve-staging-deploy.sh"
workflow="$script_dir/../workflows/deploy-keystone-stack-staging.yml"
test_sha="0123456789abcdef0123456789abcdef01234567"
synapse_sha="89abcdef0123456789abcdef0123456789abcdef"

if grep -Fq "releaseName:" "$workflow" || grep -Fq "inputs.releaseName" "$workflow"; then
  echo "staging workflow must not accept a releaseName input" >&2
  exit 1
fi
grep -Fxq "  group: keystone-staging" "$workflow"

run_resolver() {
  local output_file="$1"
  shift

  env \
    GITHUB_OUTPUT="$output_file" \
    KEYSTONE_IMAGE_TAG_INPUT="$test_sha" \
    SYNAPSE_IMAGE_TAG="$synapse_sha" \
    DNS_CNAME_READY=true \
    CONFIRM_STAGING=deploy-staging \
    VOLCENGINE_CR_ENDPOINT=registry.example.com \
    KEYSTONE_IMAGE_REPOSITORY=prod/keystone \
    SYNAPSE_IMAGE_REPOSITORY=prod/synapse \
    STAGING_ALB_DNS_NAME=staging-alb.example.com \
    STAGING_INGRESS_CLASS=keystone-staging \
    STAGING_TOS_BUCKET=keystone-staging-bucket \
    STAGING_TOS_UPLOAD_ROLE_TRN=trn:iam::1234567890:role/keystone-staging-upload \
    STAGING_TOS_READ_ROLE_TRN=trn:iam::1234567890:role/keystone-staging-read \
    STAGING_IRSA_ROLE_TRN=trn:iam::1234567890:role/archebase-staging-keystone-tos-irsa \
    STAGING_GRPC_LISTENER_PORT=50053 \
    STAGING_HILBERT_BASE_URL=https://hilbert-staging.example.com/hilbert-be-backend \
    "$@" \
    bash "$resolver" >/dev/null
}

valid_output="$(mktemp)"
trap 'rm -f "$valid_output" "${case_output:-}"' EXIT
run_resolver "$valid_output" RELEASE_NAME=legacy-release

grep -Fxq "release_name=keystone-staging" "$valid_output"
grep -Fxq "host=keystone-staging.archebase.cn" "$valid_output"
grep -Fxq "keystone_image=registry.example.com/prod/keystone:$test_sha" "$valid_output"
grep -Fxq "synapse_image=registry.example.com/prod/synapse:$synapse_sha" "$valid_output"

expect_failure() {
  local name="$1"
  shift

  case_output="$(mktemp)"
  if run_resolver "$case_output" "$@" 2>/dev/null; then
    echo "$name unexpectedly succeeded" >&2
    exit 1
  fi
  rm -f "$case_output"
  case_output=""
}

expect_failure "missing confirmation" CONFIRM_STAGING=wrong
expect_failure "missing DNS confirmation" DNS_CNAME_READY=false
expect_failure "mutable image tag" KEYSTONE_IMAGE_TAG_INPUT=latest
expect_failure "shared HTTPS and gRPC listener" STAGING_GRPC_LISTENER_PORT=443
expect_failure "non-staging ingress class" STAGING_INGRESS_CLASS=keystone-prod
expect_failure "non-staging bucket" STAGING_TOS_BUCKET=archebase-prod-keystone
expect_failure "non-staging upload role" STAGING_TOS_UPLOAD_ROLE_TRN=trn:iam::1234567890:role/keystone-upload
expect_failure "non-staging read role" STAGING_TOS_READ_ROLE_TRN=trn:iam::1234567890:role/keystone-read
expect_failure "non-staging IRSA role" STAGING_IRSA_ROLE_TRN=trn:iam::1234567890:role/archebase-prod-keystone-tos-irsa
expect_failure "non-staging Hilbert" STAGING_HILBERT_BASE_URL=https://hilbert.example.com/hilbert-be-backend

echo "staging deployment resolution tests passed"
