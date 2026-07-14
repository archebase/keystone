#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PACKAGE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

"$SCRIPT_DIR/gen_swift_proto.sh"

if ! git -C "$PACKAGE_ROOT" diff --exit-code -- Sources/DGWProto/Generated; then
  echo "Swift proto generated sources are stale. Run Scripts/gen_swift_proto.sh and commit the updated files." >&2
  exit 1
fi

echo "Swift proto generated sources are fresh."
