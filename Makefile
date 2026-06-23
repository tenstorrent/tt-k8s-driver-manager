# tt-k8s-driver-manager development targets.

CONTROLLER_GEN  ?= $(shell go env GOPATH)/bin/controller-gen
CONTROLLER_IMG  ?= ghcr.io/tenstorrent/tt-k8s-driver-manager-controller:dev
TOOLS_IMG       ?= ghcr.io/tenstorrent/tt-k8s-driver-manager-tools:dev
PLATFORMS       ?= linux/amd64

.PHONY: all
all: build

##@ Generate

.PHONY: controller-gen
controller-gen:
	@command -v $(CONTROLLER_GEN) >/dev/null || (echo "installing controller-gen" && go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5)

.PHONY: generate
generate: controller-gen
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd  paths=./api/...        output:crd:dir=./config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=tt-k8s-driver-manager paths=./internal/... output:rbac:dir=./config/rbac
	# Mirror the generated CRDs into the chart's crds/ dir. Helm packages
	# this directory verbatim — without the copy, the published chart
	# ships a stale CRD that silently drops new spec fields. Drift here
	# is easy to miss, so the sync is part of `make generate`.
	cp config/crd/bases/*.yaml charts/tt-k8s-driver-manager/crds/

##@ Build / test

.PHONY: build
build:
	go build -o bin/manager ./cmd/manager

.PHONY: test
test:
	go test ./... -race -count=1

.PHONY: vet
vet:
	go vet ./...

.PHONY: controller-image
controller-image:
	docker build -t $(CONTROLLER_IMG) .

.PHONY: tools-image
tools-image:
	docker build -t $(TOOLS_IMG) images/tools

##@ Helm

.PHONY: helm-lint
helm-lint:
	helm lint charts/tt-k8s-driver-manager

.PHONY: helm-install
helm-install:
	helm upgrade --install tt-k8s-driver-manager charts/tt-k8s-driver-manager \
		--namespace tt-k8s-driver-manager-system --create-namespace \
		--set controller.image=$(CONTROLLER_IMG) \
		--set tools.image=$(TOOLS_IMG)

##@ Plugins

PLUGIN_DIR ?= $(HOME)/.local/bin

.PHONY: install-plugins
install-plugins:
	@mkdir -p $(PLUGIN_DIR)
	install -m 0755 hack/plugins/kubectl-tt-driver $(PLUGIN_DIR)/kubectl-tt-driver
	install -m 0755 hack/plugins/kubectl-tt-fw     $(PLUGIN_DIR)/kubectl-tt-fw
	@echo "Installed kubectl-tt-driver, kubectl-tt-fw to $(PLUGIN_DIR)"

.PHONY: uninstall-plugins
uninstall-plugins:
	rm -f $(PLUGIN_DIR)/kubectl-tt-driver $(PLUGIN_DIR)/kubectl-tt-fw
