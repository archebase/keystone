#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

compose_version="${DOCKER_COMPOSE_VERSION:-v2.29.7}"
compose_os="${DOCKER_COMPOSE_OS:-linux}"
compose_arch="${DOCKER_COMPOSE_ARCH:-x86_64}"
asset="docker-compose-${compose_os}-${compose_arch}"
download_url="https://github.com/docker/compose/releases/download/${compose_version}/${asset}"
checksum_url="${download_url}.sha256"
bin_dir="${RUNNER_TEMP:-/tmp}/bin"
compose_bin="${bin_dir}/docker-compose"
tmp_asset="${RUNNER_TEMP:-/tmp}/${asset}"
tmp_checksum="${tmp_asset}.sha256"

mkdir -p "${bin_dir}"
curl --fail --silent --show-error --location \
  --connect-timeout 30 \
  --max-time 300 \
  --retry 3 \
  --retry-delay 2 \
  "${download_url}" \
  -o "${tmp_asset}"
curl --fail --silent --show-error --location \
  --connect-timeout 30 \
  --max-time 60 \
  --retry 3 \
  --retry-delay 2 \
  "${checksum_url}" \
  -o "${tmp_checksum}"

expected_sha="$(awk '{print $1}' "${tmp_checksum}")"
echo "${expected_sha}  ${tmp_asset}" | sha256sum -c -
install -m 0755 "${tmp_asset}" "${compose_bin}"
rm -f "${tmp_asset}" "${tmp_checksum}"

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "${bin_dir}" >> "${GITHUB_PATH}"
fi

"${compose_bin}" version
