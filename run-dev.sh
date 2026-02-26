#!/bin/bash
# run-dev.sh - Quick development runner for Keystone Edge
# Usage: ./run-dev.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "[INFO] Starting Keystone Edge in development mode..."
echo "[INFO] API: http://localhost:8080"
echo "[INFO] Swagger: http://localhost:8080/swagger/index.html"
echo ""

# Load .env and export variables
if [ -f .env ]; then
	export $(cat .env | grep -v '^#' | xargs)
fi

# Run the application
exec ./bin/keystone-edge "$@"
