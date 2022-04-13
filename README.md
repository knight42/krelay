![GitHub](https://img.shields.io/github/license/knight42/krelay)
![](https://github.com/knight42/krelay/actions/workflows/test.yml/badge.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/knight42/krelay)](https://goreportcard.com/report/github.com/knight42/krelay)
![GitHub last commit](https://img.shields.io/github/last-commit/knight42/krelay)

# krelay

This kubectl plugin is a drop-in replacement for `kubectl port-forward` with some enhanced features.

## Table of Contents

- [Features](#features)
- [Demo](#demo)
- [Installation](#installation)
- [Usage](#usage)
- [Flags](#flags)
- [How It Works](#how-it-works)

## Features

* Compatible with `kubectl port-forward`
* Supports UDP port forwarding
* Forwarding data to the given IP or hostname that is accessible within the kubernetes cluster
  * You could forward a local port to a port in the `Service` or a workload like `Deployment` or `StatefulSet`, and the forwarding session will not be interfered even if you perform rolling updates.
  * The hostname is resolved inside the cluster, so you don't need to change your local nameserver or modify the `/etc/hosts`.

## Demo

### Forwarding UDP port

[![asciicast](https://asciinema.org/a/452745.svg)](https://asciinema.org/a/452745)

### Forwarding traffic to a Service

[![asciicast](https://asciinema.org/a/452747.svg)](https://asciinema.org/a/452747)

NOTE: The forwarding session is not affected after rolling update.

### Forwarding traffic to a IP or hostname

[![asciicast](https://asciinema.org/a/452749.svg)](https://asciinema.org/a/452749)

## Installation

| Distribution                           | Command / Link                                                 |
|----------------------------------------|----------------------------------------------------------------|
| [Krew](https://krew.sigs.k8s.io/)      | `kubectl krew install relay`                                   |
| Pre-built binaries for macOS, Linux    | [GitHub releases](https://github.com/knight42/krelay/releases) |

NOTE: If you only have limited access to the cluster, please make sure the permissions specified in [rbac.yaml](./manifests/rbac.yaml)
is granted:
```bash
kubectl create -f https://raw.githubusercontent.com/knight42/krelay/main/manifests/rbac.yaml
```

### Build from source

```
git clone https://github.com/knight42/krelay
cd krelay
make krelay
cp krelay "$GOPATH/bin/kubectl-relay"
kubectl relay -V
```

## Usage

```bash
# Listen on port 8080 locally, forwarding data to the port named "http" in the service
kubectl relay service/my-service 8080:http

# Listen on a random port locally, forwarding udp packets to port 53 in a pod selected by the deployment
kubectl relay -n kube-system deploy/kube-dns :53@udp

# Listen on port 5353 on all addresses, forwarding data to port 53 in the pod
kubectl relay --address 0.0.0.0 pod/my-pod 5353:53

# Listen on port 6379 locally, forwarding data to "redis.cn-north-1.cache.amazonaws.com:6379" from the cluster
kubectl relay host/redis.cn-north-1.cache.amazonaws.com 6379

# Listen on port 5000 and 6000 locally, forwarding data to "1.2.3.4:5000" and "1.2.3.4:6000" from the cluster
kubectl relay ip/1.2.3.4 5000@tcp 6000@udp

# Create the agent in the kube-public namespace, and forward local port 5000 to "1.2.3.4:5000"
kubectl relay --server.namespace kube-public ip/1.2.3.4 5000
```

## Flags

| flag                 | default                                 | description                                                 |
|----------------------|-----------------------------------------|-------------------------------------------------------------|
| `--address`          | `127.0.0.1`                             | Address to listen on. Only accepts IP addresses as a value. |
| `--server.image`     | `ghcr.io/knight42/krelay-server:v0.0.1` | The krelay-server image to use.                             |
| `--server.namespace` | `default`                               | The namespace in which krelay-server is located.            |

## How It Works

`krelay` will install an agent(named `krelay-server`) to the kubernetes cluster, and the agent will forward the traffic to the target ip/hostname.

If the target is an object in the cluster, like `Deployment`, `StatefulSet`, `krelay` will automatically select a pod it managed like `kubectl port-forward` does.
After that `krelay` will tell the destination IP(i.e. the pod's IP) and the destination port to the agent by sending a special `Header` first,
and then the data will be forwarded to the agent and sent to the target address.

Specifically, if the target is a `Service`, `krelay` will try to determine the destination address automatically:
* If the `Service` has a clusterIP, then the clusterIP is used as the destination IP.
* If the type of `Service` is `ExternalName`, then the external name is used as the destination address.
* If none of the above scenario is met, then `krelay` will choose a pod selected by this `Service`.

The `Header` looks like this:

|            | Version | Header Length | Request ID | Protocol | Destination Port | Address Type | Address  |
|------------|---------|---------------|------------|----------|------------------|--------------|----------|
| Byte Count | 1       | 2             | 16         | 1        | 2                | 1            | Variable |

* `Version`: This field is preserved for future extension, and it is not in-use now.
* `Header Length`: The total length of the `Header` in bytes.
* `Request ID`: The ID of the request(now a UUID is used as the request ID).
* `Protocol`: The protocol of the request, `0` stands for TCP and `1` stands for UDP.
* `Destination Port`: The destination port of the request.
* `Address Type`: The type of the destination address, `0` stands for IP and `1` stands for hostname.
* `Address`: The destination address of the request:
  * 4 bytes for IPv4 address
  * 16 bytes for IPv6 address
  * Variable bytes for hostname
