# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

.PHONY: help build check-cr push run test clean lint helm-lint license proto

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

# Build variables
IMAGE_NAME ?= keystone-edge
IMAGE_TAG ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "latest")
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CR_REGISTRY ?= archebase-cr-cn-beijing.cr.volces.com
CR_NAMESPACE ?=
CR_REPOSITORY ?= $(IMAGE_NAME)
FULL_IMAGE = $(CR_REGISTRY)/$(CR_NAMESPACE)/$(CR_REPOSITORY):$(IMAGE_TAG)

# Build Docker image
build:
	docker build \
		--build-arg VERSION=$(IMAGE_TAG) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_NAME):latest \
		.
	@echo "Built $(IMAGE_NAME):$(IMAGE_TAG)"

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
