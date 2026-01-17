.PHONY: all build build-cli build-server test clean docker-build docker-push deploy generate-certs

# Variables
BINARY_CLI := krelay
BINARY_SERVER := krelay-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
IMAGE_NAME := knight42/krelay-server
IMAGE_TAG ?= dev

# Go settings
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

all: build

# Build both binaries
build: build-cli build-server

# Build CLI
build-cli:
	@echo "Building $(BINARY_CLI)..."
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(BINARY_CLI) ./cmd/krelay

# Build server
build-server:
	@echo "Building $(BINARY_SERVER)..."
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(BINARY_SERVER) ./cmd/krelay-server

# Cross-compile for Linux (for Docker)
build-linux:
	@echo "Building for Linux..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_SERVER)-linux-amd64 ./cmd/krelay-server

# Run tests
test:
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf bin/
	rm -f tls.crt tls.key ca.crt ca.key

# Install CLI locally
install: build-cli
	cp bin/$(BINARY_CLI) $(GOPATH)/bin/

# Docker build
docker-build: build-linux
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

# Docker push
docker-push:
	docker push $(IMAGE_NAME):$(IMAGE_TAG)

# Generate self-signed TLS certificates for development
generate-certs:
	@echo "Generating self-signed certificates..."
	@openssl genrsa -out ca.key 2048
	@openssl req -x509 -new -nodes -key ca.key -sha256 -days 365 -out ca.crt \
		-subj "/CN=krelay-ca"
	@openssl genrsa -out tls.key 2048
	@echo "subjectAltName=DNS:krelay-server,DNS:krelay-server.krelay-system,DNS:krelay-server.krelay-system.svc,DNS:krelay-server.krelay-system.svc.cluster.local" > extfile.cnf
	@openssl req -new -key tls.key -out server.csr \
		-subj "/CN=krelay-server.krelay-system.svc"
	@openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
		-out tls.crt -days 365 -sha256 -extfile extfile.cnf
	@rm -f extfile.cnf server.csr ca.srl
	@echo "Certificates generated: ca.crt, tls.crt, tls.key"

# Create TLS secret in Kubernetes
create-tls-secret: generate-certs
	kubectl create namespace krelay-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret tls krelay-server-tls \
		--cert=tls.crt \
		--key=tls.key \
		-n krelay-system \
		--dry-run=client -o yaml | kubectl apply -f -

# Deploy to Kubernetes
deploy: docker-build create-tls-secret
	kubectl apply -k deploy/

# Undeploy from Kubernetes
undeploy:
	kubectl delete -k deploy/ --ignore-not-found

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Update dependencies
deps:
	go mod tidy

# Show help
help:
	@echo "Available targets:"
	@echo "  build          - Build both CLI and server binaries"
	@echo "  build-cli      - Build CLI binary only"
	@echo "  build-server   - Build server binary only"
	@echo "  build-linux    - Cross-compile for Linux"
	@echo "  test           - Run tests"
	@echo "  clean          - Remove build artifacts"
	@echo "  install        - Install CLI to GOPATH/bin"
	@echo "  docker-build   - Build Docker image"
	@echo "  docker-push    - Push Docker image to registry"
	@echo "  generate-certs - Generate self-signed TLS certificates"
	@echo "  create-tls-secret - Create TLS secret in Kubernetes"
	@echo "  deploy         - Deploy to Kubernetes"
	@echo "  undeploy       - Remove from Kubernetes"
	@echo "  fmt            - Format code"
	@echo "  lint           - Run linter"
	@echo "  deps           - Update dependencies"
