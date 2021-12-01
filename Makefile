IMAGE_TAG ?= latest

NAME := kubectl-relay
VERSION ?= $(shell git describe --tags || echo "unknown")
GO_LDFLAGS = "-w -s -X github.com/knight42/krelay/pkg/constants.ClientVersion=$(VERSION)"
GOBUILD = CGO_ENABLED=0 go build -trimpath -ldflags $(GO_LDFLAGS)

.PHONY: server-image push-server-image
server-image:
	docker build -t ghcr.io/knight42/krelay-server:$(IMAGE_TAG) -f manifests/Dockerfile .
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

# Release
PLATFORM_LIST = \
        darwin-amd64 \
        darwin-arm64 \
        linux-amd64 \
        linux-arm64
darwin-%:
	GOARCH=$* GOOS=darwin $(GOBUILD) -o $(NAME)_$(VERSION)_$@/$(NAME) ./cmd/client

linux-%:
	GOARCH=$* GOOS=linux $(GOBUILD) -o $(NAME)_$(VERSION)_$@/$(NAME) ./cmd/client

gz_releases=$(addsuffix .tar.gz, $(PLATFORM_LIST))
$(gz_releases): %.tar.gz : %
	tar czf $(NAME)_$(VERSION)_$@ -C $(NAME)_$(VERSION)_$</ ../LICENSE $(NAME)
	sha256sum $(NAME)_$(VERSION)_$@ > $(NAME)_$(VERSION)_$@.sha256

.PHONY: releases
releases: $(gz_releases)
