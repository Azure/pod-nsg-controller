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
	KUBEBUILDER_ASSETS="$$(cd "$$( $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path )" && pwd)" \
	go test -p 1 ./... -coverprofile cover.out

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
