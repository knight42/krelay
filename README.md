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
* `ssh/NODE` — a root shell or one-shot commands on a cluster node via
  nsenter (like `kubectl node-shell` but over WireGuard). Repeated commands
  reuse the pod and tunnel and run in milliseconds.
* Data plane bypasses the apiserver — no more SPDY/websocket streams through
  the control plane; large transfers don't load the apiserver.
* End-to-end encrypted (WireGuard). The DERP relay only sees ciphertext and is
  used only until a direct path is established.
* Forwarding to a workload survives rolling updates: each new connection
  re-resolves a ready pod.
* Simultaneous forwarding to multiple targets (`-f targets.txt`).
* TCP and UDP.
* Local SOCKS5 proxy with cluster-side DNS (`kubectl relay socks`).

## Usage

The executable name selects the CLI. `kubectl-relay` (invoked as
`kubectl relay`) keeps the plugin syntax shown below. `krelay` uses explicit
subcommands:

```bash
krelay port-forward svc/nginx 8080:80
krelay port-forward -f targets.txt
krelay ssh my-node-01
krelay ssh my-node-01 -- journalctl -u kubelet -n 50
krelay socks 1080
krelay cp ./file my-node-01:/tmp/file
```

Shared flags such as `--context` and `--server.namespace` can appear before
or after the subcommand. Run `krelay COMMAND --help` for command help.
`port-forward` retains the plugin's target types, TCP/UDP port syntax, and
multiple-target file support. Both SSH entry points and `cp` share the same mux daemon.

Command-specific flags follow their subcommand: `--file/-f` belongs to
`port-forward`; `--control-persist` and `--derp-map-url` belong to `ssh` and
`cp`; `--recursive/-r` belongs to `cp`. The kubectl plugin keeps its existing
flat flag interface.

### Copy files

```bash
# Upload a file, or download it under a new name
krelay cp ./file.bin my-node-01:/tmp/file.bin
krelay cp my-node-01:/tmp/file.bin ./download.bin

# Copy a directory recursively
krelay cp -r ./data my-node-01:/tmp/
krelay cp -r my-node-01:/tmp/data ./backup
```

Exactly one operand must be `NODE:PATH`; remote paths refer to the node's
host filesystem. If the destination is an existing directory, the source
basename is appended; otherwise the destination names the copied file or
directory. The destination's parent must exist. Use `./` to disambiguate a
local filename containing a colon, and quote paths containing spaces.

Copies stream files over the server's built-in SFTP subsystem; neither tar nor
a shell is needed on the node. The server runs a dedicated worker rooted in
the host filesystem, so absolute paths and absolute symlinks resolve on the
host. Relative remote paths start at `/`. Regular files, directories, and
symbolic links are supported; symbolic links are copied as links. Ownership
is not preserved, and interrupted copies may leave partial results. Existing
files are replaced without following destination links.

This requires a server image with SFTP support. When upgrading a cached `v2`
image, use `--server.pull-policy=Always` for the new server Job. Existing mux
sessions keep their running server until its idle timeout; an older server
reports a clear SFTP error. `--control-persist=0` starts a fresh temporary
server when immediate use of the new image is needed.

### kubectl plugin

```bash
# Forward local port 8080 to port 80 of service "nginx"
kubectl relay svc/nginx 8080:80

# Hostname resolved inside the cluster
kubectl relay host/redis.cn-north-1.cache.amazonaws.com 6379

# Survives rolling updates
kubectl relay deploy/backend 5000

# Forward a UDP port (e.g. DNS)
kubectl relay -n kube-system svc/kube-dns 10053:53@udp

# Open a root shell on a cluster node
kubectl relay ssh/my-node-01

# Run a single command on a cluster node, like ssh
kubectl relay ssh/my-node-01 -- journalctl -u kubelet -n 50

# Multiple targets
kubectl relay -f targets.txt
```

Port syntax: `[LOCAL_PORT:]REMOTE_PORT[@PROTOCOL]`, where `REMOTE_PORT` may
be a named port of the target object (its declared protocol is used unless
`@tcp`/`@udp` is given; numeric ports default to TCP). `:REMOTE_PORT` picks
an ephemeral local port.

## SOCKS5 proxy

```bash
# Listen on 127.0.0.1:1080, with TCP CONNECT and UDP ASSOCIATE
kubectl relay socks

# Choose a port, or use 0 for an available port
kubectl relay --context staging socks 1081

# Choose where the proxy pod runs (also controls short-name DNS resolution)
kubectl relay --server.namespace payments socks

# In another terminal; socks5h sends the hostname to the cluster for resolution
curl --proxy socks5h://127.0.0.1:1080 http://api.payments.svc:8080
```

Use `--address` to change the local bind IP. For browsers, select SOCKS v5
and enable DNS through the proxy. Hostnames resolve inside the proxy pod;
`api` refers to a service in its namespace, while `api.payments.svc` names
a service in `payments`. IP destinations are also supported.

The proxy runs in the foreground. Ctrl-C stops it and deletes its server
Job. It does not configure system proxy settings or launch a child command.
`-n` and `-f` do not apply to SOCKS mode; use
`--server.namespace` for the proxy pod namespace. BIND is not supported.
The same UDP payload and reply-source limits described below apply.

## SSH mode

`kubectl relay ssh/NODE` creates a privileged (hostPID) pod pinned to the
node and uses nsenter to enter the host namespaces — like
`kubectl node-shell`, but over WireGuard. Without a command it opens an
interactive root shell; with `-- COMMAND` it runs the command with stdin
passed through, stdout/stderr kept separate, and the remote exit status
propagated as the exit code, so it scripts exactly like `ssh NODE COMMAND`:

```bash
kubectl relay ssh/my-node-01 -- 'crictl ps | grep etcd'
```

Sessions to the same node are shared, like ssh's ControlMaster: the first
command takes a few seconds to set up the pod and tunnel, later ones complete
in milliseconds. Everything is cleaned up after `--control-persist`
(default 10m) without sessions; `--control-persist=0` opts out of sharing.

SSH mode runs independently from port forwarding and cannot be combined with
`-f`.

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
| `--control-persist` | `10m` | SSH mode: how long the mux daemon keeps the server pod and tunnel alive after the last session (`0` disables the daemon) |
| `--derp-map-url` | `https://tailcat.dev/derpmap.json` | DERP map for the tunnel bootstrap (`file://` reads a local file) |
| `-v` | `3` | Log verbosity (5 also logs tailcat internals) |

## Caveats

* **UDP payload size**: the tunnel's MTU limits UDP payloads to 1232 bytes
  (`tailcat.MaxUDPPayload`); larger datagrams are dropped silently. Replies
  must come from the forwarded address and port — protocols that answer from
  an ephemeral port (e.g. TFTP) are not supported.
* **SSH mode** keeps its privileged pod on the node until `--control-persist`
  expires; each node gets its own pod.

## Development

```bash
make krelay        # build the CLI
make server-image  # build amd64 + arm64 images into dist/krelay-server.tar (OCI)
make push-server-image  # publish both architectures under the IMAGE tag
make test          # unit tests
```
