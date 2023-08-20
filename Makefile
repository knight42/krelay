IMAGE_TAG ?= latest

NAME := kubectl-relay
GOBUILD = CGO_ENABLED=0 go build -trimpath

.PHONY: server-image push-server-image
server-image:
	docker build -t ghcr.io/knight42/krelay-server:$(IMAGE_TAG) -f manifests/Dockerfile-server .
push-server-image: server-image
	docker push ghcr.io/knight42/krelay-server:$(IMAGE_TAG)

.PHONY: krelay
krelay:
	$(GOBUILD) -o krelay ./cmd/client

.PHONY: lint
lint:
	golangci-lint run

.PHONY: test
test:
	go test -race -v ./...

.PHONY: coverage
coverage:
	go test -race -v -coverprofile=cover.out ./...
	go tool cover -html cover.out
	rm cover.out

.PHONY: clean
clean:
	rm -rf krelay*
	rm -rf kubectl-relay*
