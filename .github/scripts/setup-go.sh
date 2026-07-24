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
go_path="$(go env GOPATH)"
go_path_bin="${go_path}/bin"
mkdir -p "${go_path_bin}"
export PATH="${go_path_bin}:${PATH}"
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "${go_bin_dir}" >> "${GITHUB_PATH}"
  echo "${go_path_bin}" >> "${GITHUB_PATH}"
fi

if [ "${GO_INSTALL_RACE_DEPS:-0}" = "1" ] && ! command -v gcc > /dev/null 2>&1; then
  if [ "$(id -u)" != "0" ]; then
    echo "GO_INSTALL_RACE_DEPS=1 requires root when gcc is missing" >&2
    exit 1
  fi
  if ! command -v apt-get > /dev/null 2>&1; then
    echo "apt-get is required to install Go race detector dependencies" >&2
    exit 1
  fi
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gcc libc6-dev
  apt-get clean
  rm -rf /var/lib/apt/lists/*
fi

go version
