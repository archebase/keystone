# SPDX-FileCopyrightText: 2026 ArcheBase
#
# SPDX-License-Identifier: MulanPSL-2.0

.PHONY: help build check-cr push run test clean lint helm-lint license proto
.PHONY: stereo-split-image stereo-split-container-smoke stereo-split-test
.PHONY: e2-multimodal-conversion-image e2-multimodal-conversion-container-smoke e2-multimodal-conversion-test
.PHONY: calibration-placeholder-image calibration-placeholder-container-smoke calibration-placeholder-test
.PHONY: calibration-image calibration-container-smoke calibration-test

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
	@echo "  make e2-multimodal-conversion-image - Build the E2 conversion Job image"
	@echo "  make e2-multimodal-conversion-container-smoke - Smoke-test the E2 conversion image"
	@echo "  make e2-multimodal-conversion-test - Test the E2 conversion Job contract"
	@echo "  make calibration-placeholder-image - Build the Orbit placeholder calibration image"
	@echo "  make calibration-placeholder-container-smoke - Smoke-test the placeholder image"
	@echo "  make calibration-placeholder-test - Test the placeholder Python job"
	@echo "  make calibration-image - Build the production calibration Job image"
	@echo "  make calibration-container-smoke - Build and smoke-test the calibration image"
	@echo "  make calibration-test - Test the calibration Job entrypoint and preprocessing"

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
E2_MULTIMODAL_CONVERSION_IMAGE ?= e2-multimodal-conversion:dev
E2_MULTIMODAL_CONVERSION_PLATFORM ?= linux/amd64
E2_MULTIMODAL_CONVERSION_DEBIAN_MIRROR ?= http://mirrors.ustc.edu.cn/debian
E2_MULTIMODAL_CONVERSION_DEBIAN_SECURITY_MIRROR ?= http://mirrors.ustc.edu.cn/debian-security
E2_MULTIMODAL_CONVERSION_PYPI_INDEX_URL ?= https://pypi.tuna.tsinghua.edu.cn/simple
CALIBRATION_PLACEHOLDER_IMAGE ?= calibration-placeholder:dev
CALIBRATION_PLACEHOLDER_PLATFORM ?= linux/amd64
CALIBRATION_IMAGE ?= archebase-calibration:dev
CALIBRATION_PLATFORM ?= linux/amd64
CALIBRATION_BUILD_JOBS ?= 4
CALIBRATION_PYPI_INDEX_URL ?= https://mirrors.aliyun.com/pypi/simple/
ARCHEBASE_CALIB_REPOSITORY ?= git@gitlab.archebase.cn:robotics-and-ai/archebase_calib.git
ARCHEBASE_CALIB_REF ?= refs/heads/main

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

e2-multimodal-conversion-image:
	docker build \
		--platform $(E2_MULTIMODAL_CONVERSION_PLATFORM) \
		--build-arg DEBIAN_MIRROR=$(E2_MULTIMODAL_CONVERSION_DEBIAN_MIRROR) \
		--build-arg DEBIAN_SECURITY_MIRROR=$(E2_MULTIMODAL_CONVERSION_DEBIAN_SECURITY_MIRROR) \
		--build-arg PYPI_INDEX_URL=$(E2_MULTIMODAL_CONVERSION_PYPI_INDEX_URL) \
		--file jobs/e2-multimodal-conversion/Dockerfile \
		--tag $(E2_MULTIMODAL_CONVERSION_IMAGE) \
		.

e2-multimodal-conversion-container-smoke: e2-multimodal-conversion-image
	docker run --rm $(E2_MULTIMODAL_CONVERSION_IMAGE) --help > /dev/null

e2-multimodal-conversion-test: e2-multimodal-conversion-image
	docker run --rm \
		--entrypoint python3 \
		-v $(CURDIR)/jobs/e2-multimodal-conversion:/app \
		-w /app \
		$(E2_MULTIMODAL_CONVERSION_IMAGE) \
		-m unittest discover -s tests -p 'test_*.py' -v

stereo-split-container-smoke: stereo-split-image
	docker run --rm $(STEREO_SPLIT_IMAGE) --help > /dev/null

stereo-split-test:
	python3 -m unittest discover \
		-s jobs/stereo-split/tests \
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

calibration-image:
	@set -eu; \
		revision="$$(git ls-remote --exit-code \
			"$(ARCHEBASE_CALIB_REPOSITORY)" "$(ARCHEBASE_CALIB_REF)" \
			| awk 'NR == 1 { print $$1 }')"; \
		test -n "$$revision" || \
			(echo "Cannot resolve ARCHEBASE_CALIB_REF=$(ARCHEBASE_CALIB_REF)"; exit 1); \
		echo "Building with archebase_calib@$$revision"; \
		docker build \
			--platform $(CALIBRATION_PLATFORM) \
			--ssh default \
			--build-context "archebase_calib=$(ARCHEBASE_CALIB_REPOSITORY)#$$revision" \
			--build-arg ARCHEBASE_CALIB_REVISION="$$revision" \
			--build-arg BUILD_JOBS="$(CALIBRATION_BUILD_JOBS)" \
			--build-arg PYPI_INDEX_URL="$(CALIBRATION_PYPI_INDEX_URL)" \
			--file jobs/calibration/Dockerfile \
			--tag $(CALIBRATION_IMAGE) \
			.

calibration-container-smoke: calibration-image
	docker run --rm $(CALIBRATION_IMAGE) --help > /dev/null
	docker run --rm --entrypoint /bin/bash $(CALIBRATION_IMAGE) -lc \
		'python3 /workspace/calib_cli/calibrate.py --help > /dev/null && \
		source /workspace/kalibr/scripts/kalibr_no_ros_env.sh && \
		python3 /workspace/kalibr/aslam_offline_calibration/kalibr/python/kalibr_calibrate_cameras_mcap --help > /dev/null'

calibration-test:
	python3 -m unittest discover \
		-s jobs/calibration/tests \
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
	helm install smoke deploy/helm/keystone-stack \
		--namespace archebase-system \
		--dry-run=client \
		--set ingress.grpc.annotations=null \
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
