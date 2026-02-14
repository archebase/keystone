#!/bin/bash
# Test script for Keystone Edge

set -e

echo "========================================="
echo "Keystone Edge Test Suite"
echo "========================================="
echo ""

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Test counter
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# Run test function
run_test() {
    local name="$1"
    local cmd="$2"

    TESTS_RUN=$((TESTS_RUN + 1))
    echo "[$TESTS_RUN] Testing: $name"

    if eval "$cmd" > /dev/null 2>&1; then
        echo -e "  ${GREEN}PASSED${NC}"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    else
        echo -e "  ${RED}FAILED${NC}"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
}

echo "Starting services..."
docker compose -f docker/docker-compose.test.yml up -d --build

echo "Waiting for services to be ready..."
sleep 15

echo ""
echo "Running tests..."
echo ""

# Health check test
run_test "Health check endpoint" \
    "curl -f http://localhost:8080/api/v1/health"

# Swagger docs test
run_test "Swagger documentation" \
    "curl -f http://localhost:8080/swagger/doc.json"

# Swagger UI test
run_test "Swagger UI" \
    "curl -f http://localhost:8080/swagger/index.html"

# API response content test
run_test "Health response valid JSON" \
    "curl -s http://localhost:8080/api/v1/health | grep -q '\"status\"'"

echo ""
echo "========================================="
echo "Test Results"
echo "========================================="
echo "Total:  $TESTS_RUN"
echo -e "Passed: ${GREEN}$TESTS_PASSED${NC}"
if [ $TESTS_FAILED -gt 0 ]; then
    echo -e "Failed: ${RED}$TESTS_FAILED${NC}"
    EXIT_CODE=1
else
    echo "Failed: $TESTS_FAILED"
    EXIT_CODE=0
fi
echo "========================================="

echo ""
echo "Collecting logs..."
docker compose -f docker/docker-compose.test.yml logs > test-results.log 2>&1

echo "Stopping services..."
docker compose -f docker/docker-compose.test.yml down -v

echo ""
if [ $EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}All tests passed!${NC}"
else
    echo -e "${RED}Some tests failed! Check test-results.log for details.${NC}"
fi

exit $EXIT_CODE
