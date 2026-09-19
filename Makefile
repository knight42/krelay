IMAGE ?= ghcr.io/knight42/krelay-server:v2
PLATFORMS ?= linux/amd64,linux/arm64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: krelay
krelay:
	go build -trimpath -ldflags '$(LDFLAGS)' -o krelay ./cmd/krelay

.PHONY: krelay-server
krelay-server:
	go build -trimpath -ldflags '$(LDFLAGS)' -o krelay-server ./cmd/krelay-server

.PHONY: install
install:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(shell go env GOPATH)/bin/kubectl-relay ./cmd/krelay

.PHONY: test
test:
	go test -race ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: server-image
# OCI archives preserve both architectures without requiring a multi-platform
# local Docker image store. Override PLATFORMS to build a single architecture.
server-image:
	mkdir -p dist
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE) --output type=oci,dest=dist/krelay-server.tar .

.PHONY: push-server-image
push-server-image:
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE) --push .
