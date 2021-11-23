.PHONY: server-image push-server-image
server-image:
	docker build -t knight42/krelay-server:latest -f manifests/Dockerfile .
push-server-image:
	docker push knight42/krelay-server:latest

.PHONY: lint
lint:
	golangci-lint run

.PHONY: krelay
krelay:
	CGO_ENABLED=0 go build -trimpath -o krelay ./cmd/client
