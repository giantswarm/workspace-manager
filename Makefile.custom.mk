##@ Development

BINARY := workspace-manager

.PHONY: build-linux-amd64
build-linux-amd64: ## Build the linux/amd64 binary the Dockerfile expects; version and commit come from the Go build info (the tag at HEAD, else dev).
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(BINARY)-linux-amd64 .

.PHONY: docker-build
docker-build: build-linux-amd64 ## Build a local dev image (TAG=workspace-manager:dev).
	docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t $(or $(TAG),workspace-manager:dev) .

##@ Helm

HELM_UNITTEST_VERSION := 1.0.3

.PHONY: helm-test
helm-test: helm-lint helm-unittest ## Run every chart check (what the chart-test CI job runs).

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint helm/workspace-manager

.PHONY: helm-unittest
helm-unittest: helm-plugin-unittest ## Run the helm-unittest suites in helm/workspace-manager/tests/.
	helm unittest helm/workspace-manager

.PHONY: helm-plugin-unittest
helm-plugin-unittest:
	@helm plugin list | grep -q '^unittest' || helm plugin install https://github.com/helm-unittest/helm-unittest --version $(HELM_UNITTEST_VERSION)

##@ Testing

ENVTEST_K8S_VERSION := 1.37.0
SETUP_ENVTEST := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25

.PHONY: test-envtest
test-envtest: ## Run the tests that need a real API server (build tag envtest) against envtest's kube-apiserver and etcd.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test -race -count=1 -tags envtest ./...
