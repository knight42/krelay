IMAGE_TAG ?= latest

GO_LDFLAGS = '-w -s'
GOBUILD = CGO_ENABLED=0 go build -v -trimpath -ldflags $(GO_LDFLAGS)

.PHONY: server-image push-server-image
server-image:
	docker build -t ghcr.io/knight42/krelay-server:$(IMAGE_TAG) -f manifests/Dockerfile .
push-server-image: server-image
	docker push ghcr.io/knight42/krelay-server:$(IMAGE_TAG)

.PHONY: krelay-server
krelay-server:
	$(GOBUILD) -o krelay-server ./cmd/server

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
