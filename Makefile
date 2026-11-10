IMAGE ?= aisix-controller:dev
AISIX_TAG ?= $(shell cat third_party/aisix/VERSION)
ENVTEST_K8S_VERSION ?= 1.36.2
LOCALBIN ?= $(CURDIR)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

CONTROLLER_TOOLS_VERSION ?= v0.20.0
SETUP_ENVTEST_VERSION ?= release-0.24
GOLANGCI_LINT_VERSION ?= v2.7.1

.PHONY: build test test-aisix lint generate crds crds-check sync-schemas image e2e

build:
	go build ./...

## Unit tests plus the envtest suites.
test: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./...

## Unit tests against the real aisix binary, in a Linux container.
test-aisix:
	hack/test-with-aisix.sh ./internal/...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

## DeepCopy methods for the API types.
generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

## CRDs from the vendored aisix schemas.
crds:
	go run ./cmd/crdgen -schemas third_party/aisix -out config/crd

crds-check: generate crds
	git diff --exit-code api config/crd

## Vendor another aisix release's schemas: make sync-schemas AISIX_TAG=v1.10.0
sync-schemas:
	hack/sync-schemas.sh $(AISIX_TAG)

image:
	docker build --build-arg AISIX_VERSION=$(patsubst v%,%,$(AISIX_TAG)) -t $(IMAGE) .

e2e:
	IMAGE=$(IMAGE) test/e2e/run.sh

$(CONTROLLER_GEN):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
