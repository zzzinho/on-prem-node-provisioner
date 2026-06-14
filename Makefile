# ONP — On-Prem Node Provisioner
#
# controller-gen runs via Go's `tool` directive (go.mod), so it is pinned with
# the rest of the build and needs no separate install step.

.DEFAULT_GOAL := build

CONTROLLER_GEN := go tool controller-gen
CRD_DIR := config/crd/bases
RBAC_DIR := config/rbac

# Image coordinates: <REGISTRY>/<component>:<TAG>. TAG tracks the chart's
# appVersion (charts/onp/Chart.yaml) so a chart release and its images move
# together. Override either: make docker-push REGISTRY=... TAG=...
REGISTRY ?= ghcr.io/zzzinho
TAG ?= 0.6.0
PLATFORM ?= linux/amd64

# Per-image buildx flags. shutdown-agent needs the alpine runtime-privileged
# stage for nsenter; the other two take the default distroless stage.
define BUILDX
docker buildx build --platform $(PLATFORM) --build-arg BIN=$(1) $(2) -t $(REGISTRY)/$(1):$(TAG)
endef

.PHONY: generate
generate: ## Generate DeepCopy methods (zz_generated.deepcopy.go).
	$(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: ## Generate CRD manifests and the controller RBAC Role.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=$(CRD_DIR)
	$(CONTROLLER_GEN) rbac:roleName=onp-controller paths=./internal/controller/... output:rbac:dir=$(RBAC_DIR)

.PHONY: build
build: ## Build all binaries.
	go build ./...

.PHONY: test
test: ## Run all tests.
	go test ./...

.PHONY: fmt
fmt: ## Format all Go source.
	gofmt -w .

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: docker-build
docker-build: ## Build all three images locally (buildx --load).
	$(call BUILDX,onp-controller,) --load .
	$(call BUILDX,onp-wol-agent,) --load .
	$(call BUILDX,onp-shutdown-agent,--target runtime-privileged) --load .

.PHONY: docker-push
docker-push: ## Build and push all three images (buildx --push).
	$(call BUILDX,onp-controller,) --push .
	$(call BUILDX,onp-wol-agent,) --push .
	$(call BUILDX,onp-shutdown-agent,--target runtime-privileged) --push .
