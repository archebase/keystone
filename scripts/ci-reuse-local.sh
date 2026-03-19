#!/bin/bash

# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

# =============================================================================
# Local CI Script for REUSE Compliance
# =============================================================================
# Checks REUSE license compliance for Keystone project.
#
# Usage:
#   ./scripts/ci-reuse-local.sh [--sbom]
#
# Options:
#   --sbom    Generate SPDX SBOM (Software Bill of Materials)
#   --help    Show this help message
#
# Prerequisites:
#   - Python 3.11+
#   - fsfe-reuse (pip install reuse)
# =============================================================================

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default settings
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GENERATE_SBOM=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --sbom)
            GENERATE_SBOM=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 [--sbom] [--help]"
            echo ""
            echo "Options:"
            echo "  --sbom    Generate SPDX SBOM (Software Bill of Materials)"
            echo "  --help    Show this help message"
            echo ""
            echo "This script checks REUSE license compliance for Keystone."
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            exit 1
            ;;
    esac
done

echo -e "${BLUE}=============================================${NC}"
echo -e "${BLUE}REUSE Compliance Local CI (Keystone)${NC}"
echo -e "${BLUE}=============================================${NC}"
echo ""

cd "${PROJECT_ROOT}"

# =============================================================================
# Check prerequisites
# =============================================================================
echo -e "${YELLOW}Checking prerequisites...${NC}"

# Check Python
if ! command -v python3 &> /dev/null; then
    echo -e "${RED}Error: Python 3 is not installed${NC}"
    exit 1
fi

PYTHON_VERSION=$(python3 --version 2>&1 | grep -oP '\d+\.\d+' | head -1)
echo -e "Python version: ${GREEN}${PYTHON_VERSION}${NC}"

# Check reuse
if ! command -v reuse &> /dev/null; then
    echo -e "${YELLOW}reuse not found, installing...${NC}"
    pip install reuse
fi

REUSE_VERSION=$(reuse --version 2>&1 | head -1)
echo -e "reuse version: ${GREEN}${REUSE_VERSION}${NC}"

echo ""

# =============================================================================
# Run REUSE compliance check
# =============================================================================
echo -e "${YELLOW}Running REUSE compliance check...${NC}"
echo ""

if reuse lint; then
    echo ""
    echo -e "${GREEN}✓ REUSE compliance check passed!${NC}"
else
    echo ""
    echo -e "${RED}✗ REUSE compliance check failed!${NC}"
    echo ""
    echo -e "${YELLOW}Tips to fix licensing issues:${NC}"
    echo "  1. Add SPDX headers to files:"
    echo "     reuse annotate --year 2026 --copyright 'ArcheBase' --license 'MulanPSL-2.0' --style go <file>"
    echo ""
    echo "  2. Check REUSE.toml for global rules"
    echo "  3. Run 'reuse lint' for detailed error messages"
    exit 1
fi

# =============================================================================
# Generate SPDX SBOM (optional)
# =============================================================================
if [ "$GENERATE_SBOM" = true ]; then
    echo ""
    echo -e "${YELLOW}Generating SPDX SBOM...${NC}"

    reuse spdx --output reuse.spdx
    echo -e "${GREEN}✓ SPDX SBOM generated: ${PROJECT_ROOT}/reuse.spdx${NC}"

    # Show summary
    echo ""
    echo -e "${BLUE}SBOM Summary:${NC}"
    echo "  File: reuse.spdx"
    echo "  Size: $(wc -c < reuse.spdx) bytes"
    echo "  Lines: $(wc -l < reuse.spdx)"
fi

echo ""
echo -e "${GREEN}=============================================${NC}"
echo -e "${GREEN}Local CI completed successfully!${NC}"
echo -e "${GREEN}=============================================${NC}"
