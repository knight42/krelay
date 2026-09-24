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

## SSH mux daemon

`ssh/NODE` sessions go through a per-(cluster, namespace, node) background
daemon (hidden `--ssh-mux` flag, `cmd/krelay/sshmux.go`) that owns the server
Job and tunnel, à la OpenSSH ControlMaster/ControlPersist:

- Identity: sha256 of (resolved `ToRESTConfig().Host`, server namespace,
  node name) — never raw kubeconfig flags, so cluster identity survives
  `kubectl config use-context` and different kubeconfig spellings share a
  daemon.
- Files under `$XDG_STATE_HOME/krelay` (default `~/.local/state/krelay`):
  `ssh-<id>.sock`, `.lock`, `.log`. The flock on `.lock`, held for the
  daemon's lifetime, designates the socket owner: a socket whose lock is
  free is an orphan and may be unlinked before bind — never unlink one
  otherwise. `.lock` and `.log` are never removed; the log is truncated on
  each daemon start.
- Startup handshake: the parent passes a pipe as fd 3; the daemon writes one
  line — `READY`, `BUSY` (lost the flock race; wait for the winner's
  socket), or `ERROR <msg>`.
- Lifetime: exits — deleting the Job and socket — when a pod watch sees the
  server pod deleted or terminal (seconds; hung sessions get EOF), after
  `--control-persist` without sessions, or after two consecutive failures
  dialing the server's SSH port (backstop for node-level death the watch
  can't see; a broken watch is never treated as pod death). The server-side
  `--idle-timeout` backstops a SIGKILLed daemon.
- `--control-persist=0` and `--server-token` bypass the daemon: the
  invocation owns an in-process tunnel torn down on exit.
- Exec mode (`ssh/NODE -- CMD`) propagates the remote exit status via
  `exitCodeError`; keep it exiting silently with that code, like ssh.

## Verification

End-to-end runs use the local OrbStack cluster (its kubeconfig is not merged
into `~/.kube/config`; pass `--kubeconfig ~/.orbstack/k8s/config.yml`, node
name `orbstack`), e.g. `./krelay --kubeconfig ~/.orbstack/k8s/config.yml
svc/kubernetes 8443:443`. Client-only changes work against the
already-published server image.
