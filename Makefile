IMG ?= pod-nsg-controller:latest

LOCALBIN ?= $(PWD)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_GEN_VERSION ?= v0.16.5
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
SETUP_ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.x

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint against code.
	golangci-lint run ./...

.PHONY: test
test: generate manifests fmt vet setup-envtest ## Run tests.
	go test $$(go list ./... | grep -v -E '(api/v1alpha1$$|test/integration/)') -coverprofile cover-unit.out
	KUBEBUILDER_ASSETS="$$(cd "$$( $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path )" && pwd)" \
	go test -p 1 ./api/... ./test/integration/... -coverprofile cover-envtest.out
	@echo "mode: set" > cover.out
	@grep -hv '^mode:' cover-unit.out cover-envtest.out >> cover.out
	@rm -f cover-unit.out cover-envtest.out

.PHONY: test-phase9-integration
test-phase9-integration: generate manifests setup-envtest ## Run phase9 integration tests.
	KUBEBUILDER_ASSETS="$$(cd "$$( $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path )" && pwd)" \
	go test -p 1 -count=1 -timeout 180s ./test/integration/phase9/...

.PHONY: test-phase9-e2e
test-phase9-e2e: ## Run phase9 E2E tests (requires AZURE_E2E=true and live cluster).
	go test -count=1 -timeout 1200s -tags=e2e ./test/e2e/...

.PHONY: test-coverage
test-coverage: test ## Run tests with coverage report.
	go tool cover -html=cover.out -o coverage.html

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ CNI (transparent-tunnel)

# Repository-native build/packaging of the transparent-tunnel CNI artifact
# (EPIC-009 / FR-018). These targets WRAP scripts/e2e/build-cni.sh, which pins
# the external azure-container-networking transparent-tunnel source (CON-011),
# verifies a recorded checksum, asserts the conflist declares
# "mode": "transparent-tunnel", and packages a digest-addressable OCI artifact.
# The source pin lives in build-cni.sh (CNI_SOURCE_REPO/REF); EPIC-008 records
# how the exact ref/checksum is obtained and updated (ASSUMPTION-005). For an
# offline dry run: `make cni-artifact CNI_SOURCE_MODE=fixture \
# CNI_FIXTURE_DIR=<dir> BUILD_PUSH=false`.

.PHONY: cni-build
cni-build: ## Acquire+verify transparent-tunnel azure-vnet + conflist (pinned; asserts mode).
	bash scripts/e2e/build-cni.sh build

.PHONY: cni-package
cni-package: cni-build ## Package the exact CNI bytes and compute a content digest.
	bash scripts/e2e/build-cni.sh package

.PHONY: cni-artifact
cni-artifact: cni-package ## Push the digest-addressable CNI OCI artifact by digest (+SBOM, +manifest).
	bash scripts/e2e/build-cni.sh artifact

##@ Deployment

.PHONY: deploy
deploy: ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/rbac/
	cat config/manager/*.yaml | sed 's|pod-nsg-controller:latest|${IMG}|g' | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	kubectl delete -f config/manager/
	kubectl delete -f config/rbac/

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate code (deepcopy).
	$(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate Kubernetes manifests (CRD YAML).
	mkdir -p config/crd
	rm -f config/crd/podasgmapping.yaml config/crd/networking.azure.com_podasgmappings.yaml
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd
	test -f config/crd/networking.azure.com_podasgmappings.yaml
	mv config/crd/networking.azure.com_podasgmappings.yaml config/crd/podasgmapping.yaml

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST) ## Download envtest helper and Kubernetes test assets.
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path >/dev/null

$(CONTROLLER_GEN):
	mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(SETUP_ENVTEST):
	mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/ cover.out coverage.html
