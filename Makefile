# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

.PHONY: help build check-cr push run test clean lint helm-lint license proto
.PHONY: stereo-split-image stereo-split-container-smoke stereo-split-test
.PHONY: stereo-split-convert-image stereo-split-convert-container-smoke stereo-split-convert-test
.PHONY: calibration-placeholder-image calibration-placeholder-container-smoke calibration-placeholder-test

# Default target
help:
	@echo "Available targets:"
	@echo "  make build       - Build Docker image"
	@echo "  make run         - Run container locally"
	@echo "  make test        - Test health endpoint"
	@echo "  make clean       - Remove built image"
	@echo "  make push        - Push to Volcengine CR (set CR_NAMESPACE)"
	@echo "  make lint        - Run Go linter (golangci-lint)"
	@echo "  make helm-lint   - Lint Keystone Helm chart"
	@echo "  make license     - Run REUSE license compliance check"
	@echo "  make proto       - Regenerate Go bindings from .proto sources"
	@echo "  make stereo-split-image - Build the stereo split Job image"
	@echo "  make stereo-split-container-smoke - Build and smoke-test the Job image"
	@echo "  make stereo-split-test  - Test the stereo split Job entrypoint"
	@echo "  make stereo-split-convert-image - Build the stereo H.264 conversion Job image"
	@echo "  make stereo-split-convert-container-smoke - Smoke-test the conversion Job image"
	@echo "  make stereo-split-convert-test - Test the stereo H.264 conversion Job"
	@echo "  make calibration-placeholder-image - Build the Orbit placeholder calibration image"
	@echo "  make calibration-placeholder-container-smoke - Smoke-test the placeholder image"
	@echo "  make calibration-placeholder-test - Test the placeholder Python job"

# Build variables
IMAGE_NAME ?= keystone-edge
IMAGE_TAG ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "latest")
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CR_REGISTRY ?= archebase-cr-cn-beijing.cr.volces.com
CR_NAMESPACE ?=
CR_REPOSITORY ?= $(IMAGE_NAME)
FULL_IMAGE = $(CR_REGISTRY)/$(CR_NAMESPACE)/$(CR_REPOSITORY):$(IMAGE_TAG)
STEREO_SPLIT_IMAGE ?= stereo-split:dev
STEREO_SPLIT_PLATFORM ?= linux/amd64
STEREO_SPLIT_CONVERT_IMAGE ?= stereo-split-convert:dev
STEREO_SPLIT_CONVERT_PLATFORM ?= linux/amd64
CALIBRATION_PLACEHOLDER_IMAGE ?= calibration-placeholder:dev
CALIBRATION_PLACEHOLDER_PLATFORM ?= linux/amd64

# Build Docker image
build:
	docker build \
		--build-arg VERSION=$(IMAGE_TAG) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_NAME):latest \
		.
	@echo "Built $(IMAGE_NAME):$(IMAGE_TAG)"

stereo-split-image:
	docker build \
		--platform $(STEREO_SPLIT_PLATFORM) \
		--file jobs/stereo-split/Dockerfile \
		--tag $(STEREO_SPLIT_IMAGE) \
		.

stereo-split-container-smoke: stereo-split-image
	docker run --rm $(STEREO_SPLIT_IMAGE) --help > /dev/null

stereo-split-test:
	python3 -m unittest discover \
		-s jobs/stereo-split/tests \
		-p 'test_*.py' \
		-v

stereo-split-convert-image:
	docker build \
		--platform $(STEREO_SPLIT_CONVERT_PLATFORM) \
		--file jobs/stereo-split-convert/Dockerfile \
		--tag $(STEREO_SPLIT_CONVERT_IMAGE) \
		.

stereo-split-convert-container-smoke: stereo-split-convert-image
	docker run --rm $(STEREO_SPLIT_CONVERT_IMAGE) --help > /dev/null

stereo-split-convert-test:
	python3 -m unittest discover \
		-s jobs/stereo-split-convert/tests \
		-p 'test_*.py' \
		-v

calibration-placeholder-image:
	docker build \
		--platform $(CALIBRATION_PLACEHOLDER_PLATFORM) \
		--file jobs/calibration-placeholder/Dockerfile \
		--tag $(CALIBRATION_PLACEHOLDER_IMAGE) \
		.

calibration-placeholder-container-smoke: calibration-placeholder-image
	docker run --rm $(CALIBRATION_PLACEHOLDER_IMAGE) --help > /dev/null

calibration-placeholder-test:
	python3 -m unittest discover \
		-s jobs/calibration-placeholder/tests \
		-p 'test_*.py' \
		-v

# Run container
run: build
	docker run -d --name keystone-edge \
		-p 8080:8080 \
		-p 8090:8090 \
		-p 8091:8091 \
		-p 50053:50053 \
		--env KEYSTONE_BIND_ADDR=:8080 \
		$(IMAGE_NAME):latest

# Test health endpoint
test:
	@curl -f http://localhost:8080/api/v1/health || echo "Health check failed"

# Clean up
clean:
	-docker rm -f keystone-edge 2>/dev/null || true
	-docker rmi $(IMAGE_NAME):latest $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true

# Validate Volcengine CR coordinates before building or pushing.
check-cr:
	@test -n "$(CR_REGISTRY)" || (echo "CR_REGISTRY is required"; exit 1)
	@test -n "$(CR_NAMESPACE)" || (echo "CR_NAMESPACE is required"; exit 1)

# Push to Volcengine Container Registry (CR)
push: check-cr build
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(FULL_IMAGE)
	docker push $(FULL_IMAGE)
	@echo "Pushed $(FULL_IMAGE)"

# Run Go linter
lint:
	@if command -v golangci-lint &> /dev/null; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found, skipping..."; \
	fi

helm-lint:
	helm lint deploy/helm/keystone-stack \
		--namespace archebase-system \
		--set keystone.image.tag=e062c27a6617 \
		--set synapse.image.tag=4f294f56bfb1 \
		--set credentials.hilbertAccessKey=lint-ak \
		--set credentials.hilbertSecretKey=lint-sk
	helm template smoke deploy/helm/keystone-stack \
		--namespace archebase-system \
		--set keystone.image.tag=e062c27a6617 \
		--set synapse.image.tag=4f294f56bfb1 \
		--set credentials.hilbertAccessKey=lint-ak \
		--set credentials.hilbertSecretKey=lint-sk >/dev/null

# Regenerate gRPC/protobuf Go bindings from .proto source files.
# Requires: protoc (v5.x), protoc-gen-go, protoc-gen-go-grpc on PATH.
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
proto:
	go generate ./internal/cloud/cloudpb/

# REUSE license compliance check
license:
	@if command -v reuse &> /dev/null; then \
		reuse lint; \
	else \
		echo "reuse not found, installing..."; \
		pip install reuse; \
		reuse lint; \
	fi
