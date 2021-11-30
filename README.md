![GitHub](https://img.shields.io/github/license/knight42/krelay)
![](https://github.com/knight42/krelay/actions/workflows/test.yml/badge.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/knight42/krelay)](https://goreportcard.com/report/github.com/knight42/krelay)
![GitHub last commit](https://img.shields.io/github/last-commit/knight42/krelay)

# krelay

This kubectl plugin is a drop-in replacement for `kubectl port-forward` with some enhanced features.

## Extra Features

* Supports UDP port forwarding
* Forwarding data to the given IP or hostname that is accessible within the kubernetes cluster
  * You could forward a local port to a port in the `Service`, and the forwarding session will not be interfered even if you perform rolling updates.

## Demo

### Forwarding UDP port

[![asciicast](https://asciinema.org/a/452745.svg)](https://asciinema.org/a/452745)

### Forwarding traffic to a Service

[![asciicast](https://asciinema.org/a/452747.svg)](https://asciinema.org/a/452747)

NOTE: The forwarding session is not affected after rolling update.

### Forwarding traffic to a IP or hostname

[![asciicast](https://asciinema.org/a/452749.svg)](https://asciinema.org/a/452749)

## Installing

| Distribution                           | Command / Link                                                 |
|----------------------------------------|----------------------------------------------------------------|
| Pre-built binaries for macOS, Linux    | [GitHub releases](https://github.com/knight42/krelay/releases) |

## Usage

```bash
# Listen on port 8080 locally, forwarding data to the port named "http" in the service
kubectl relay svc/my-service 8080:http

# Listen on a random port locally, forwarding udp packets to port 53 in a pod selected by the deployment
kubectl relay -n kube-system deploy/kube-dns :53@udp

# Listen on port 5353 on all addresses, forwarding data to port 53 in the pod
kubectl relay --address 0.0.0.0 pod/my-pod 5353:53

# Listen on port 6379 locally, forwarding data to "redis.cn-north-1.cache.amazonaws.com:6379" from the cluster
kubectl relay host/redis.cn-north-1.cache.amazonaws.com 6379

# Listen on port 5000 and 6000 locally, forwarding data to "1.2.3.4:5000" and "1.2.3.4:6000" from the cluster
kubectl relay ip/1.2.3.4 5000@tcp 6000@udp
```

## How It Works

TBD
