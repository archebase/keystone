#!/bin/bash
# run-dev.sh - Quick development runner for Keystone Edge
# Usage: ./run-dev.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "[INFO] Starting Keystone Edge in development mode..."
echo ""

# Load .env and export variables
if [ -f .env ]; then
	export $(cat .env | grep -v '^#' | xargs)
fi

# Run the application
exec ./bin/keystone-edge "$@"
