#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
: "${SYNAPSE_IMAGE_TAG:?SYNAPSE_IMAGE_TAG is required}"
: "${DNS_CNAME_READY:?DNS_CNAME_READY is required}"
: "${CONFIRM_STAGING:?CONFIRM_STAGING is required}"
: "${VOLCENGINE_CR_ENDPOINT:?VOLCENGINE_CR_ENDPOINT is required}"
: "${KEYSTONE_IMAGE_REPOSITORY:?KEYSTONE_IMAGE_REPOSITORY is required}"
: "${SYNAPSE_IMAGE_REPOSITORY:?SYNAPSE_IMAGE_REPOSITORY is required}"
: "${STAGING_ALB_DNS_NAME:?STAGING_KEYSTONE_ALB_DNS_NAME repository or staging environment variable is required}"
: "${STAGING_INGRESS_CLASS:?STAGING_KEYSTONE_INGRESS_CLASS repository or staging environment variable is required}"
: "${STAGING_TOS_BUCKET:?STAGING_KEYSTONE_TOS_BUCKET repository or staging environment variable is required}"
: "${STAGING_TOS_UPLOAD_ROLE_TRN:?STAGING_KEYSTONE_TOS_UPLOAD_ROLE_TRN repository or staging environment variable is required}"
: "${STAGING_TOS_READ_ROLE_TRN:?STAGING_KEYSTONE_TOS_READ_ROLE_TRN repository or staging environment variable is required}"
: "${STAGING_IRSA_ROLE_TRN:?STAGING_KEYSTONE_IRSA_ROLE_TRN repository or staging environment variable is required}"
: "${STAGING_GRPC_LISTENER_PORT:?STAGING_KEYSTONE_GRPC_LISTENER_PORT repository or staging environment variable is required}"
: "${STAGING_HILBERT_BASE_URL:?STAGING_KEYSTONE_HILBERT_BASE_URL repository or staging environment variable is required}"

release_name="keystone-staging"
host="${release_name}.archebase.cn"

keystone_image_tag="${KEYSTONE_IMAGE_TAG_INPUT:-}"
if [[ -z "$keystone_image_tag" ]]; then
  keystone_image_tag="$(git rev-parse HEAD)"
fi

if [[ "$CONFIRM_STAGING" != "deploy-staging" ]]; then
  echo "confirmStaging must be exactly deploy-staging" >&2
  exit 1
fi

if [[ "$DNS_CNAME_READY" != "true" ]]; then
  echo "dnsCnameReady must be true before staging deployment" >&2
  echo "Expected CNAME: ${host} -> ${STAGING_ALB_DNS_NAME}" >&2
  exit 1
fi

if ! [[ "$keystone_image_tag" =~ ^[0-9a-f]{40}$ ]]; then
  echo "keystoneImageTag must be a full 40-character lowercase commit SHA" >&2
  exit 1
fi

if ! [[ "$SYNAPSE_IMAGE_TAG" =~ ^[0-9a-f]{40}$ ]]; then
  echo "synapseImageTag must be a full 40-character lowercase commit SHA" >&2
  exit 1
fi

if ! [[ "$STAGING_INGRESS_CLASS" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]]; then
  echo "STAGING_KEYSTONE_INGRESS_CLASS must be a valid Kubernetes resource name" >&2
  exit 1
fi
if [[ "$STAGING_INGRESS_CLASS" != *staging* ]]; then
  echo "STAGING_KEYSTONE_INGRESS_CLASS must identify staging" >&2
  exit 1
fi

if ! [[ "$STAGING_GRPC_LISTENER_PORT" =~ ^[1-9][0-9]*$ ]]; then
  echo "STAGING_KEYSTONE_GRPC_LISTENER_PORT must be a dedicated port between 1 and 65535 other than 443" >&2
  exit 1
fi
grpc_listener_port_number=$((10#$STAGING_GRPC_LISTENER_PORT))
if ((grpc_listener_port_number > 65535 || grpc_listener_port_number == 443)); then
  echo "STAGING_KEYSTONE_GRPC_LISTENER_PORT must be a dedicated port between 1 and 65535 other than 443" >&2
  exit 1
fi

if [[ "$STAGING_TOS_BUCKET" != *staging* ]]; then
  echo "STAGING_KEYSTONE_TOS_BUCKET must identify staging" >&2
  exit 1
fi

if ! [[ "$STAGING_TOS_UPLOAD_ROLE_TRN" =~ ^trn:iam::[0-9]+:role/[^[:space:]]*staging[^[:space:]]*$ ]]; then
  echo "STAGING_KEYSTONE_TOS_UPLOAD_ROLE_TRN must be a staging Volcengine IAM role TRN" >&2
  exit 1
fi

if ! [[ "$STAGING_TOS_READ_ROLE_TRN" =~ ^trn:iam::[0-9]+:role/[^[:space:]]*staging[^[:space:]]*$ ]]; then
  echo "STAGING_KEYSTONE_TOS_READ_ROLE_TRN must be a staging Volcengine IAM role TRN" >&2
  exit 1
fi

if ! [[ "$STAGING_IRSA_ROLE_TRN" =~ ^trn:iam::[0-9]+:role/[^[:space:]]*staging[^[:space:]]*$ ]]; then
  echo "STAGING_KEYSTONE_IRSA_ROLE_TRN must be a staging Volcengine IAM role TRN" >&2
  exit 1
fi

if ! [[ "$STAGING_HILBERT_BASE_URL" =~ ^https://[^/]*staging[^/]*/.+ ]]; then
  echo "STAGING_KEYSTONE_HILBERT_BASE_URL must be a staging HTTPS URL with a path" >&2
  exit 1
fi

keystone_image="${VOLCENGINE_CR_ENDPOINT}/${KEYSTONE_IMAGE_REPOSITORY}:${keystone_image_tag}"
synapse_image="${VOLCENGINE_CR_ENDPOINT}/${SYNAPSE_IMAGE_REPOSITORY}:${SYNAPSE_IMAGE_TAG}"

{
  echo "release_name=${release_name}"
  echo "host=${host}"
  echo "keystone_image_tag=${keystone_image_tag}"
  echo "synapse_image_tag=${SYNAPSE_IMAGE_TAG}"
  echo "keystone_image=${keystone_image}"
  echo "synapse_image=${synapse_image}"
} >> "$GITHUB_OUTPUT"
