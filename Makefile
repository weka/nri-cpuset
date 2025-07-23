SHELL = /bin/bash

# Build variables
BINARY_NAME = weka-cpuset
BUILD_DIR = bin
PKG_DIR = ./...
GOFLAGS = -mod=vendor
LDFLAGS = -s -w
CGO_ENABLED = 0

# Image variables - now supports configurable registry
REGISTRY ?= weka
IMAGE_NAME ?= nri-cpuset
IMAGE_TAG ?= latest
FULL_IMAGE = $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

# Test variables
GINKGO = go run github.com/onsi/ginkgo/v2/ginkgo
TEST_TIMEOUT = 30m

.PHONY: help
help: ## Show this help message
	@awk '/^[a-zA-Z_-]+:.*##/ { \
		target = $$1; \
		sub(/:.*$$/, "", target); \
		desc = $$0; \
		sub(/^[^#]*## */, "", desc); \
		printf "\033[36m%-20s\033[0m %s\n", target, desc \
	}' $(MAKEFILE_LIST)

.PHONY: vendor
vendor: ## Download and vendor dependencies
	go mod tidy
	go mod vendor

.PHONY: build
build: vendor ## Build the binary
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/weka-cpuset

.PHONY: clean
clean: ## Clean build artifacts
	rm -rf $(BUILD_DIR)
	rm -rf vendor/

.PHONY: lint
lint: ## Run linters
	golangci-lint run --timeout=5m

.PHONY: fmt
fmt: ## Format code
	go fmt $(PKG_DIR)
	gofumpt -l -w .

.PHONY: test
test: ## Run unit tests
	go test -race -coverprofile=coverage.out ./pkg/... ./cmd/...

.PHONY: test-integration
test-integration: ## Run integration tests (no K8s required)
	$(GINKGO) -r --timeout=$(TEST_TIMEOUT) --cover --coverprofile=integration.out --label-filter="integration" ./test/

.PHONY: test-e2e-kind
test-e2e-kind: ## Run e2e tests with kind cluster (RECOMMENDED)
	./hack/kind-e2e.sh

.PHONY: test-e2e-live
test-e2e-live: ## Run e2e tests against live cluster
	./hack/e2e-live.sh

.PHONY: test-e2e
test-e2e: ## Run e2e tests (defaults to kind)
	$(MAKE) test-e2e-kind

.PHONY: kind-up
kind-up: ## Create kind cluster for testing
	./hack/kind-up.sh

.PHONY: kind-down
kind-down: ## Delete kind cluster
	kind delete cluster --name $${KIND_CLUSTER_NAME:-test}

.PHONY: image
image: ## Build container image (REGISTRY=my-registry.com IMAGE_NAME=weka-nri-cpuset IMAGE_TAG=v1.0.0)
	@echo "Building image: $(FULL_IMAGE)"
	docker build --platform linux/amd64 -t $(FULL_IMAGE) .

.PHONY: image-push
image-push: image ## Build and push container image
	@echo "Pushing image: $(FULL_IMAGE)"
	docker push $(FULL_IMAGE)

.PHONY: image-with-timestamp
image-with-timestamp: ## Build and push image with timestamp tag (used by build-and-deploy)
	$(MAKE) image-push IMAGE_TAG=$(shell date +%s)

.PHONY: chart
chart: ## Package Helm chart
	helm package deploy/helm/weka-nri-cpuset

.PHONY: install-tools
install-tools: ## Install development tools
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install mvdan.cc/gofumpt@latest
	go install github.com/onsi/ginkgo/v2/ginkgo@latest

.PHONY: generate
generate: ## Generate code
	go generate $(PKG_DIR)

.PHONY: verify
verify: fmt lint test ## Run all verification steps (unit tests only)

.PHONY: verify-all
verify-all: fmt lint test test-integration test-e2e-kind ## Run all verification steps including e2e