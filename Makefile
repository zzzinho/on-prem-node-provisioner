# ONP — On-Prem Node Provisioner
#
# controller-gen runs via Go's `tool` directive (go.mod), so it is pinned with
# the rest of the build and needs no separate install step.

.DEFAULT_GOAL := build

CONTROLLER_GEN := go tool controller-gen
CRD_DIR := config/crd/bases
RBAC_DIR := config/rbac

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
