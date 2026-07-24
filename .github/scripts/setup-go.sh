#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

set -euo pipefail

go_version="${GO_VERSION:-1.24.13}"
go_os="${GO_OS:-linux}"
go_arch="${GO_ARCH:-amd64}"
download_base_url="${GO_DOWNLOAD_BASE_URL:-https://mirrors.aliyun.com/golang}"
archive="go${go_version}.${go_os}-${go_arch}.tar.gz"
install_root="${RUNNER_TEMP:-/tmp}/go-${go_version}-${go_os}-${go_arch}"
install_dir="${install_root}/go"

if command -v go >/dev/null 2>&1 && [ "$(go env GOVERSION 2>/dev/null || true)" = "go${go_version}" ]; then
  go_bin_dir="$(dirname "$(command -v go)")"
else
  tmp_archive="${RUNNER_TEMP:-/tmp}/${archive}"
  rm -rf "${install_root}" "${tmp_archive}"
  mkdir -p "${install_root}"
  curl --fail --silent --show-error --location \
    --connect-timeout 30 \
    --max-time 300 \
    --retry 3 \
    --retry-delay 2 \
    "${download_base_url}/${archive}" \
    -o "${tmp_archive}"
  tar -C "${install_root}" -xzf "${tmp_archive}"
  rm -f "${tmp_archive}"
  go_bin_dir="${install_dir}/bin"
fi

export PATH="${go_bin_dir}:${PATH}"
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "${go_bin_dir}" >> "${GITHUB_PATH}"
fi

go version
