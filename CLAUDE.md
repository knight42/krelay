# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`krelay` is a `kubectl port-forward` replacement (installed as `kubectl-relay`) with UDP, multi-target, and in-cluster IP/hostname support. See `README.md` for user-facing docs and `docs/ARCHITECTURE.md` for internals.

## Commands

- `make krelay` — build client. Install as `$GOPATH/bin/kubectl-relay` so `kubectl relay` picks it up.
- `make test` — `go test -race -v ./...`. Single test: `go test -race -run TestName ./pkg/xnet/...`.
- `make lint` — `golangci-lint run`.
- `make server-image` — build `ghcr.io/knight42/krelay-server` from `manifests/Dockerfile-server`.
- `make test-e2e` — run e2e tests against a live k8s cluster (`go test -count=1 -tags e2e ./test/e2e/`). Set `KRELAY_SERVER_IMAGE` to override the server image.

Go 1.26. Release via GoReleaser + Krew (`.goreleaser.yaml`, `.krew.yaml`).

## Orientation

Two binaries cooperate over a single Kubernetes port-forward stream: **client** (`cmd/client`) runs locally, **server** (`cmd/server`) runs as a pod the client creates/deletes in the user's cluster. Each local connection becomes a multiplexed stream framed with a custom header (`pkg/xnet/header.go`) that tells the server the real destination.

- Wire protocol + proxy: `pkg/xnet`
- Destination resolution (static / dynamic pod watch): `pkg/remoteaddr`
- Port parsing: `pkg/ports`
- Server `batch/v1.Job` lifecycle and SPDY/websocket dialer: `pkg/kube`

## Testing policy

Every new user-facing feature must include corresponding e2e tests in `test/e2e/`. E2e tests use the `//go:build e2e` tag so `make test` skips them. Each test must be independent (no ordering dependencies), start its own krelay process, and clean up after itself.

## Gotchas

- `pkg/slog/` is exempt from `revive` var-naming (`.golangci.yaml`).
- SPDY-over-websocket is tried first; set `KUBECTL_PORT_FORWARD_WEBSOCKETS=false` to force plain SPDY.
- Register cobra flags with `cmd.Flags()`, not `cmd.LocalFlags()`. `LocalFlags()` returns a computed set that doesn't participate in parsing.
- When using atomics for connection tracking, update `lastActivity` **before** decrementing `activeConns` in disconnect paths — otherwise a monitor goroutine can see zero connections with a stale timestamp and shut down prematurely.
- `createStream()` spawns a goroutine that sends on an unbuffered `errCh`. Callers that discard `errCh` must drain it (`go func() { <-errCh }()`) to avoid leaking that goroutine.
- `RunServerJob` must clean up the Job on every error path after `Jobs().Create()` — the caller never gets a `ServerJob` handle to call `Close()` on if setup fails, and `ttlSecondsAfterFinished` only applies to finished Jobs (not stuck-Pending ones).
- E2e tests with a local server image require `imagePullPolicy: IfNotPresent` in the pod patch; the code defaults to `Always` which fails for images not in a registry. Set `KRELAY_SERVER_IMAGE` and use `serverPodPatchWithArgs()` in test helpers.
