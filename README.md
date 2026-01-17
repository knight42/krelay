# krelay

A command-line utility that forwards TCP and UDP traffic to applications running inside a Kubernetes cluster, making traffic appear as if initiated from within the cluster.

## Features

- **Port Forwarding**: Forward local ports to pods, services, deployments, arbitrary IPs, or hostnames inside the cluster
- **Proxy Mode**: Run HTTP or SOCKS5 proxy servers that route traffic through the cluster
- **Protocol Support**: TCP and UDP for port-forward mode, TCP for proxy mode
- **Auto-reconnect**: Deployment targets automatically reconnect to another pod if the current one dies
- **Service Resilience**: Service targets survive rolling updates by connecting via ClusterIP

## Architecture

krelay uses an aggregated API server pattern (similar to metrics-server):

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

## Installation

### Prerequisites

- Go 1.21+
- Kubernetes cluster with `kubectl` configured
- Docker (for building server image)

### Build

```bash
# Build both CLI and server
make build

# Build CLI only
make build-cli

# Install CLI to $GOPATH/bin
make install
```

### Deploy Server

```bash
# Generate TLS certificates and deploy
make deploy

# Or step by step:
make generate-certs
make create-tls-secret
make docker-build
kubectl apply -k deploy/
```

## Usage

### Port Forward Mode

Forward local ports to targets inside the cluster.

```bash
# Forward to a pod
krelay port-forward pod/nginx 8080:80

# Forward to a service (survives rolling updates)
krelay port-forward svc/api 8080:80

# Forward to a deployment (auto-reconnects if pod dies)
krelay port-forward deploy/web 8080:80

# Forward to arbitrary IP inside cluster
krelay port-forward ip/10.96.0.1 443

# Forward to hostname (DNS resolved inside cluster)
krelay port-forward host/kubernetes.default.svc 443

# Multiple ports
krelay port-forward pod/app 8080:80 8443:443

# UDP port
krelay port-forward svc/dns 5353:53@udp

# Specify namespace
krelay port-forward -n production svc/api 8080:80
```

#### Target Types

| Target | Format | Behavior |
|--------|--------|----------|
| Pod | `pod/<name>` | Direct connection. Session ends if pod dies. |
| Service | `svc/<name>` | Via ClusterIP/DNS. Survives rolling updates. |
| Deployment | `deploy/<name>` | Connect to pod. Auto-reconnects if pod dies. |
| IP | `ip/<address>` | Direct connection to cluster-internal IP. |
| Host | `host/<hostname>` | DNS resolved inside cluster. |

#### Port Format

```
[local_port:]<remote_port>[@protocol]
```

- `local_port`: Optional. Defaults to `remote_port`.
- `remote_port`: Required. Port on the target.
- `protocol`: Optional. `tcp` (default) or `udp`.

### Proxy Mode

Run a local proxy server that routes traffic through the cluster.

```bash
# HTTP proxy (default)
krelay proxy :8080

# SOCKS5 proxy
krelay proxy -p socks5 :1080
krelay proxy --protocol=socks5 :1080
```

Then configure applications to use the proxy:

```bash
# HTTP proxy
export http_proxy=http://127.0.0.1:8080
export https_proxy=http://127.0.0.1:8080
curl http://kubernetes.default.svc

# SOCKS5 proxy
curl --socks5 127.0.0.1:1080 http://kubernetes.default.svc
```

## Configuration

### CLI Flags

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig file (defaults to `~/.kube/config`) |
| `--context` | Kubernetes context to use |
| `-n, --namespace` | Target namespace (defaults to current context's namespace) |

### Server Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--addr` | Listen address | `:8443` |
| `--tls-cert-file` | TLS certificate file | `/etc/krelay/tls/tls.crt` |
| `--tls-private-key-file` | TLS private key file | `/etc/krelay/tls/tls.key` |
| `-v` | Log verbosity level | `0` |

## Development

```bash
# Run tests
make test

# Format code
make fmt

# Lint
make lint

# Update dependencies
make deps
```

## License

MIT
