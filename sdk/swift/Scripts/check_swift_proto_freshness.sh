#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PACKAGE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$PACKAGE_ROOT/../.." && pwd)"

"$SCRIPT_DIR/gen_swift_proto.sh"

if ! git -C "$REPO_ROOT" diff --exit-code -- sdk/swift/Sources/DGWProto/Generated; then
  echo "Swift proto generated sources are stale. Run Scripts/gen_swift_proto.sh and commit the updated files." >&2
  exit 1
fi

echo "Swift proto generated sources are fresh."
