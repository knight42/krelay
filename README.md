# krelay

`krelay` is a drop-in replacement for `kubectl port-forward` with enhanced
features. It is a ground-up reimplementation on top of
[tailcat](https://github.com/tailscale/tailcat): traffic between your machine
and the cluster flows over an end-to-end encrypted WireGuard tunnel
(bootstrapped via a DERP relay, upgraded to a direct UDP path when possible)
instead of being funneled through the Kubernetes apiserver.

## Highlights

* Forwards to `pod`, `svc`, `deploy`, `sts`, `ds`, `rs`, an in-cluster `ip`,
  or a `host`name resolved inside the cluster.
* `ssh/NODE` — SSH into a cluster node via nsenter (like `kubectl node-shell`
  but over WireGuard).
* Data plane bypasses the apiserver — no more SPDY/websocket streams through
  the control plane; large transfers don't load the apiserver.
* End-to-end encrypted (WireGuard). The DERP relay only sees ciphertext and is
  used only until a direct path is established.
* Forwarding to a workload survives rolling updates: each new connection
  re-resolves a ready pod.
* Simultaneous forwarding to multiple targets (`-f targets.txt`).
* TCP and UDP.

## Usage

```bash
# Forward local port 8080 to port 80 of service "nginx"
kubectl relay svc/nginx 8080:80

# Hostname resolved inside the cluster
kubectl relay host/redis.cn-north-1.cache.amazonaws.com 6379

# Survives rolling updates
kubectl relay deploy/backend 5000

# Forward a UDP port (e.g. DNS)
kubectl relay -n kube-system svc/kube-dns 10053:53@udp

# SSH into a cluster node (prints local address, then ssh into it)
kubectl relay ssh/my-node-01
# → ssh -p 57121 127.0.0.1

# Multiple targets
kubectl relay -f targets.txt
```

Port syntax: `[LOCAL_PORT:]REMOTE_PORT[@PROTOCOL]`, where `REMOTE_PORT` may
be a named port of the target object (its declared protocol is used unless
`@tcp`/`@udp` is given; numeric ports default to TCP). `:REMOTE_PORT` picks
an ephemeral local port.

## How it works

1. The client generates an ephemeral WireGuard node key and creates a
   short-lived `krelay-server` Job in the cluster, passing its public key so
   the server accepts only this client.
2. `krelay-server` starts a [tailcat](https://github.com/tailscale/tailcat)
   server, picks the nearest DERP region, and prints a connection token to its
   stdout. The client reads the token from the pod logs (the only control
   traffic that touches the apiserver).
3. The client connects over DERP and upgrades to a direct UDP path when NAT
   traversal succeeds. Each local connection becomes one tunneled TCP
   connection: a tiny header names the real destination, the server dials it
   from inside the cluster, and bytes are spliced.
4. On exit the client deletes the Job. If the client dies uncleanly, the
   server exits by itself after `--idle-timeout` (default 10m) without an
   attached client.

## Bring your own DERP relay

By default the tunnel bootstraps through
[tailcat's free rate-limited DERP relays](https://tailcat.dev/derpmap.json).
For production use, [run your own DERP server](https://github.com/tailscale/tailscale/tree/main/cmd/derper#derp)
and point krelay at it:

```bash
kubectl relay --derp-map-url=https://derp.example.com/derpmap.json svc/nginx 8080:80
```

The URL is only fetched by the server pod; the connection token embeds the
relay details, so the client needs no extra configuration.

A `file://` URL is also accepted. The file is read on the client machine and
its contents are passed to the server pod inline, so the pod never fetches
anything:

```bash
kubectl relay --derp-map-url=file:///etc/krelay/derpmap.json svc/nginx 8080:80
```

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-l, --address` | `127.0.0.1` | Local address to listen on |
| `-f, --file` | | Targets file, one target per line (`-` for stdin) |
| `-n, --namespace` | | Namespace of the target object |
| `--server.image` | `ghcr.io/knight42/krelay-server:v2` | Server image |
| `--server.namespace` | `default` | Namespace for the server Job |
| `--server.pull-policy` | `IfNotPresent` | Image pull policy of the server pod |
| `--derp-map-url` | `https://tailcat.dev/derpmap.json` | DERP map for the tunnel bootstrap (`file://` reads a local file) |
| `-v` | `3` | Log verbosity (5 also logs tailcat internals) |

## Caveats

* **UDP payload size**: the tunnel's MTU limits UDP payloads to 1232 bytes
  (`tailcat.MaxUDPPayload`); larger datagrams are dropped silently. Replies
  must come from the forwarded address and port — protocols that answer from
  an ephemeral port (e.g. TFTP) are not supported.
* **SSH mode** runs independently from port forwarding and cannot be combined
  with `-f`. To SSH into multiple nodes, run separate
  `kubectl relay ssh/NODE` processes.

## Development

```bash
make krelay        # build the CLI
make server-image  # build the krelay-server image
make test          # unit tests
```
