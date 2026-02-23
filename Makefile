IMAGE ?= quay.io/tiraboschi/kubevirt-io-latency-exporter
TAG   ?= latest

.PHONY: build image push deploy undeploy fmt vet tidy lint test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/exporter ./cmd/exporter

image:
	podman build -t $(IMAGE):$(TAG) .

push: image
	podman push $(IMAGE):$(TAG)


##@ Linting

GOLANGCI_LINT ?= $(shell which golangci-lint)
ifeq ($(GOLANGCI_LINT),)
GOLANGCI_LINT = $(shell go env GOPATH)/bin/golangci-lint
endif

lint: ## Run golangci-lint
	@if ! command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		echo "golangci-lint v2 not found. Installing..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; \
	fi
	$(GOLANGCI_LINT) run


# Deploy to the currently active OCP cluster.
deploy:
	oc apply -f deploy/scc.yaml
	oc apply -f deploy/rbac.yaml
	oc apply -f deploy/daemonset.yaml
	oc apply -f deploy/service.yaml
	oc apply -f deploy/servicemonitor.yaml

undeploy:
	oc delete -f deploy/servicemonitor.yaml --ignore-not-found
	oc delete -f deploy/service.yaml        --ignore-not-found
	oc delete -f deploy/daemonset.yaml      --ignore-not-found
	oc delete -f deploy/rbac.yaml           --ignore-not-found
	oc delete -f deploy/scc.yaml            --ignore-not-found

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy
