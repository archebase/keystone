#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PACKAGE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PROTO_ROOT="$PACKAGE_ROOT/protos"
OUT_DIR="$PACKAGE_ROOT/Sources/DGWProto/Generated"
TOOL_BIN="$PACKAGE_ROOT/.local/toolchains/swift-proto/bin"

SWIFT_PLUGIN="$TOOL_BIN/protoc-gen-swift"
GRPC_PLUGIN="$TOOL_BIN/protoc-gen-grpc-swift-2"

if [[ ! -x "$SWIFT_PLUGIN" ]]; then
  echo "missing executable: $SWIFT_PLUGIN" >&2
  echo "run Scripts/bootstrap_swift_proto_toolchain.sh first" >&2
  exit 1
fi

if [[ ! -x "$GRPC_PLUGIN" ]]; then
  echo "missing executable: $GRPC_PLUGIN" >&2
  echo "run Scripts/bootstrap_swift_proto_toolchain.sh first" >&2
  exit 1
fi

mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/*.swift

protoc \
  --proto_path="$PROTO_ROOT" \
  --plugin=protoc-gen-swift="$SWIFT_PLUGIN" \
  --plugin=protoc-gen-grpc-swift-2="$GRPC_PLUGIN" \
  --swift_out="$OUT_DIR" \
  --swift_opt=Visibility=Public \
  --grpc-swift-2_out="$OUT_DIR" \
  --grpc-swift-2_opt=Visibility=Public,Client=True,Server=False \
  "$PROTO_ROOT/common.proto" \
  "$PROTO_ROOT/auth.proto" \
  "$PROTO_ROOT/data_gateway.proto"

# protoc-gen-grpc-swift can emit trailing spaces on blank quoted doc-comment lines.
perl -pi -e 's/[ \t]+$//' "$OUT_DIR"/*.swift
