# krelay

A command-line utility that forwards TCP and UDP traffic to applications running inside a Kubernetes cluster, making traffic appear as if initiated from within the cluster.

## Architecture

```
┌─────────────┐     kubeconfig      ┌──────────────────┐     APIService     ┌────────────────┐
│  krelay CLI │ ──────────────────► │  K8s API Server  │ ─────────────────► │ krelay-server  │
│  (client)   │                     │                  │    (aggregated)    │ (in-cluster)   │
└─────────────┘                     └──────────────────┘                    └───────┬────────┘
                                                                                    │
                                                                                    ▼
                                                                          ┌─────────────────┐
                                                                          │  Target (pod,   │
                                                                          │  service, IP,   │
                                                                          │  hostname)      │
                                                                          └─────────────────┘
```

### Components

1. **krelay-server**: An aggregated API server deployed in the cluster (like metrics-server). Registers `APIService` for `relay.krelay.io/v1alpha1`. Handles the actual network connections to targets from within the cluster.

2. **krelay CLI**: Client binary that reads kubeconfig and communicates with krelay-server via the Kubernetes API.

3. **Kubernetes manifests**: Deployment, Service, APIService, RBAC, deployed to `krelay-system` namespace.

## Modes

### 1. `port-forward` mode

Forwards local ports to remote targets inside the cluster. Supports both TCP and UDP.

#### Syntax

```
krelay port-forward [flags] <target> <port>...
```

#### Flags

- `-n, --namespace`: Target namespace (defaults to kubeconfig's current namespace)

#### Target types

| Target | Format | Behavior |
|--------|--------|----------|
| Pod | `pod/<name>` | Direct connection to pod IP. Session ends if pod dies. |
| Service | `svc/<name>` | Via Service ClusterIP/DNS. Survives rolling updates. |
| Deployment | `deploy/<name>` | Connect to one pod. Auto-reconnects to another pod if current pod dies. |
| IP | `ip/<address>` | Direct connection to arbitrary IP inside cluster. |
| Host | `host/<hostname>` | DNS resolution inside cluster, then connect. |

#### Port format

```
[local_port:]<remote_port>[@protocol]
```

- `local_port`: Optional. Defaults to `remote_port` if omitted.
- `remote_port`: Required. The port on the target.
- `protocol`: Optional. `tcp` (default) or `udp`.

#### Examples

```bash
# Single TCP port (local 8080 -> remote 80)
krelay port-forward pod/nginx 8080:80

# Local port defaults to remote port
krelay port-forward svc/nginx 80

# UDP port
krelay port-forward svc/kube-dns 5353:53@udp

# Multiple ports
krelay port-forward pod/app 8080:80 8443:443

# Mixed TCP and UDP
krelay port-forward pod/app 8080:80@tcp 5353:53@udp

# With namespace
krelay port-forward -n production svc/api 8080:80

# Arbitrary IP inside cluster
krelay port-forward ip/10.96.0.1 443

# Hostname resolution inside cluster
krelay port-forward host/kubernetes.default.svc 443
```

### 2. `proxy` mode

Runs a local proxy server that routes traffic through the cluster. Supports TCP only.

#### Syntax

```
krelay proxy [flags] <listen_addr>
```

#### Flags

- `-p, --protocol`: Proxy protocol, `http` (default) or `socks5`

#### Examples

```bash
# HTTP proxy (default protocol)
krelay proxy :8080

# SOCKS5 proxy
krelay proxy -p socks5 :1080
krelay proxy --protocol=socks5 :1080
```

## API Design

### Aggregated API

Registers `APIService` for `relay.krelay.io/v1alpha1`.

### Tunnel Endpoint

```
GET /apis/relay.krelay.io/v1alpha1/tunnel
```

Upgrades to WebSocket for bidirectional streaming.

#### Query Parameters

| Parameter | Required | Description |
|-----------|----------|-------------|
| `target` | Yes | Target specifier: `pod/<name>`, `svc/<name>`, `deploy/<name>`, `ip/<addr>`, `host/<hostname>` |
| `namespace` | For namespaced targets | Namespace for pod/svc/deploy targets |
| `port` | Yes | Remote port number |
| `protocol` | No | `tcp` (default) or `udp` |

#### WebSocket Protocol

- For TCP: Bidirectional byte stream over WebSocket binary frames
- For UDP: Each WebSocket binary frame contains one UDP datagram
- Periodic ping frames to keep the underlying TCP connection alive and detect dead connections
  - Client sends ping frames at regular intervals (e.g., every 30 seconds)
  - Server responds with pong frames
  - Connection considered dead if no pong received within timeout (e.g., 60 seconds)

## Project Structure

```
krelay/
├── cmd/
│   ├── krelay/              # CLI binary
│   │   └── main.go
│   └── krelay-server/       # Aggregated API server binary
│       └── main.go
├── pkg/
│   ├── apis/
│   │   └── relay/
│   │       └── v1alpha1/    # API types
│   ├── client/              # CLI client logic
│   ├── server/              # Aggregated API server implementation
│   └── tunnel/              # WebSocket tunnel (shared between client/server)
├── deploy/
│   ├── namespace.yaml
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── apiservice.yaml
│   └── rbac.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## Implementation Notes

### Deployment auto-reconnect

For `deploy/<name>` targets:
1. krelay-server looks up Deployment → ReplicaSet → Pods
2. Selects one available pod and connects
3. Watches for pod deletion events
4. If current pod is deleted, transparently reconnects to another available pod
5. Client TCP connection stays alive; server-side connection switches

### UDP handling

- Client opens local UDP listener
- UDP datagrams encapsulated in WebSocket binary frames
- krelay-server maintains UDP "sessions" with timeouts
- Each UDP datagram is independent (no guaranteed ordering/delivery)

### Authentication

- CLI uses kubeconfig (supports all standard auth methods)
- krelay-server authenticates via Kubernetes API server (aggregated API pattern)
- No additional authentication for proxy mode (keep simple)

### TLS

- krelay-server uses TLS for communication with Kubernetes API server
- Certificates managed via cert-manager or static secrets
