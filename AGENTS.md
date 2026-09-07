# krelay

kubectl plugin (`kubectl-relay`) that forwards local TCP/UDP ports into a
Kubernetes cluster over a tailcat (WireGuard + DERP) tunnel, bypassing the
apiserver for data. Client: `cmd/krelay`. Short-lived in-cluster server pod:
`cmd/krelay-server`. Shared code: `pkg/`.

## Branch policy

All work happens on the `v2` branch (an orphan rewrite). Never commit to or
modify `main`.

## Commands

- Build: `make krelay` / `make krelay-server`; `make install` installs
  `kubectl-relay` into GOPATH/bin.
- Test: `make test` (`go test -race ./...`).
- Lint: `make lint` (golangci-lint; config in `.golangci.yaml`). Run it before
  declaring a change done.
- Server image: `make server-image` / `make push-server-image`
  (`ghcr.io/knight42/krelay-server:v2`).

## Code conventions

- `encoding/json` is banned (depguard); use `encoding/json/v2` and
  `encoding/json/jsontext`.
- slog messages are constant strings; variable data goes in key-value attrs,
  never `Sprintf`-ed into the message.
- Table-driven tests use `map[string]struct{...}`, not slices.
- Unit tests stay in-memory: `httptest.NewTestServer` (its network is only
  reachable via `srv.Client()`, so code under test takes an injectable
  `*http.Client`; `srv.URL` is empty until `srv.Client()` is first called)
  and `t.TempDir()`. No real network or cluster in unit tests.
- Imports are grouped stdlib / external / `github.com/knight42/krelay`
  (goimports local prefix).

## Verification

End-to-end runs use the local OrbStack cluster (`kubectl` context
`orbstack`), e.g. `./krelay svc/kubernetes 8443:443`. Client-only changes
work against the already-published server image.
