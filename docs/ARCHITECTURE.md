# Architecture

`krelay` has two binaries that cooperate over a Kubernetes port-forward stream.

## Client (`cmd/client`)

Binary: `kubectl-relay`. Parses `TYPE/NAME [LOCAL:]REMOTE[@PROTO]` args (or `-f targets.txt`), resolves each target to a remote address, creates a `krelay-server` **Job** in the default namespace (overridable via `--patch` / `--patch-file`), waits for the Job's pod to become Running, opens a single SPDY (or SPDY-over-websocket) stream via the `/portforward` subresource, and listens locally for TCP/UDP. On graceful exit the Job is deleted with `PropagationPolicy=Background`; on crash the server self-terminates on idle (see below) and the Job's `ttlSecondsAfterFinished` cleans it up.

Each local connection becomes a new multiplexed stream on that connection. UDP packets are length-prefixed over the same TCP-backed stream, with a per-client conntrack table (`cmd/client/conntrack.go`) routing replies back.

Subcommand `kubectl relay proxy` (`cmd/client/command_proxy.go`) runs a local SOCKS5 server that tunnels through the same pod.

## Server (`cmd/server`)

Image: `ghcr.io/knight42/krelay-server` (distroless, nonroot). Listens on `constants.ServerPort` (9527), reads an `xnet.Header`, dials the real destination (TCP or UDP), writes an `xnet.Acknowledgement`, then shovels bytes via `xnet.ProxyTCP` / `xnet.ProxyUDP`.

### Idle timeout

The client sends a `ProtocolKeepalive` heartbeat every 5 seconds over the port-forward stream. Each heartbeat refreshes the server's `lastActivity` timestamp. When the port-forward drops (client exit or crash), heartbeats stop. If no connections (including heartbeats) arrive within `--idle-timeout` (default 5m), the server closes the listener, `run()` returns nil, and the process exits 0 — the Job transitions to `Complete` and is garbage-collected by `ttlSecondsAfterFinished`.

## Wire protocol (`pkg/xnet/header.go`)

```
version(1) | total length(2) | request id(5) | protocol(1) | port(2) | addr type(1) | addr(variable)
```

- protocol: `0`=TCP, `1`=UDP, `2`=Keepalive (client heartbeat; server returns immediately)
- addr type: `0`=IP (4 bytes IPv4, 16 bytes IPv6), `1`=hostname (raw bytes; length is implied by total length − 12)
- ack codes: `AckCodeOK`, `AckCodeNoSuchHost`, `AckCodeResolveTimeout`, `AckCodeConnectTimeout`, `AckCodeUnknownProtocol`, `AckCodeUnknownError` — mapped from server-side `net.DNSError` / `net.OpError` in `cmd/server/main.go:ackCodeFromErr`.

## Service targeting

`cmd/client/utils.go:addrGetterForObject` picks a destination in this order for `svc/X`:

1. `Spec.Type == ExternalName` → `Spec.ExternalName` (hostname).
2. `Spec.ClusterIP` set and not `None` → cluster IP (static).
3. Otherwise → dynamic pod watch matching `Spec.Selector`.

For workloads (Deployment / StatefulSet / ReplicaSet / DaemonSet), the dynamic watcher keeps the forwarding session alive across rolling updates.

## Server-pod spec

`pkg/kube/flags.go:buildServerJob` wraps a minimal pod template in a `batch/v1.Job` with `backoffLimit: 0`, `ttlSecondsAfterFinished: 10`, `restartPolicy: Never`. The pod itself is non-root, read-only rootfs, no service-account token, no service links, with a `TopologySpreadConstraint` on `kubernetes.io/hostname`. `--patch` / `--patch-file` (JSON or YAML merge patch) is applied to the pod spec — namespace set by the patch is propagated to the Job's metadata so users can still retarget the namespace with a pod-shaped patch.

## Packages

- `pkg/kube` — Job lifecycle, REST config, SPDY-over-websocket dialer with SPDY fallback.
- `pkg/remoteaddr` — `Getter` interface; `static.go` for fixed IP/host, `dynamic.go` for pod-selector watches.
- `pkg/ports` — parses `8080:http`, `:53@udp`, etc. Uses the target object to resolve named ports and infer protocol.
- `pkg/xnet` — wire protocol, ack, `AddrPort`, `ProxyTCP`/`ProxyUDP`.
- `pkg/xio`, `pkg/alarm`, `pkg/slog`, `pkg/constants` — small helpers.
