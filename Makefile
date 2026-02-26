.PHONY: help build build-image push run test clean

# Default target
help:
	@echo "Available targets:"
	@echo "  make build       - Build Docker image"
	@echo "  make run         - Run container locally"
	@echo "  make test        - Test health endpoint"
	@echo "  make clean       - Remove built image"
	@echo "  make push        - Push to registry (set REGISTRY variable)"

# Build variables
IMAGE_NAME ?= keystone-edge
IMAGE_TAG ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "latest")
REGISTRY ?= localhost:5000
FULL_IMAGE = $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

# Build Docker image
build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) -t $(IMAGE_NAME):latest .
	@echo "Built $(IMAGE_NAME):$(IMAGE_TAG)"

# Run container
run: build
	docker run -d --name keystone-edge \
		-p 8080:8080 \
		--env KEYSTONE_BIND_ADDR=:8080 \
		$(IMAGE_NAME):latest

# Test health endpoint
test:
	@curl -f http://localhost:8080/api/v1/health || echo "Health check failed"

# Clean up
clean:
	-docker rm -f keystone-edge 2>/dev/null || true
	-docker rmi $(IMAGE_NAME):latest $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true

# Push to registry
push: build
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(FULL_IMAGE)
	docker push $(FULL_IMAGE)
	@echo "Pushed $(FULL_IMAGE)"
