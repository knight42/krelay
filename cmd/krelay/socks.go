package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/net/socks5"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func socksListenAddress(address string, args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("usage: %s socks [PORT]; running a command is not supported", programName())
	}
	if net.ParseIP(address) == nil {
		return "", fmt.Errorf("invalid listen IP address %q", address)
	}
	port := uint64(1080)
	if len(args) == 1 {
		var err error
		port, err = strconv.ParseUint(args[0], 10, 16)
		if err != nil {
			return "", fmt.Errorf("invalid SOCKS port %q: expected 0–65535", args[0])
		}
	}
	return net.JoinHostPort(address, strconv.FormatUint(port, 10)), nil
}

// runSOCKS exposes the existing cluster-side dial protocol as a local SOCKS5
// proxy. Both dialers pass hostnames unchanged so DNS runs in the server pod.
func (o *options) runSOCKS(ctx context.Context, args []string) error {
	addr, err := socksListenAddress(o.address, args)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w; choose another port or use socks 0", addr, err)
	}
	defer ln.Close()

	regions := startDERPRegionResolver(ctx, http.DefaultClient, o.derpMapURL)
	priv := key.NewNode()
	token := o.serverToken
	if token == "" {
		sj, err := startServer(ctx, o, priv, "")
		if err != nil {
			return err
		}
		defer sj.Close()
		tokenCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		token, err = sj.ReadToken(tokenCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read connection token from pod logs: %w", err)
		}
	}

	tcLogf := logger.Discard
	if o.verbosity >= 5 {
		tcLogf = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	tc := &tailcat.Client{Server: tailcat.Addr(token), Key: priv, Logf: tcLogf}
	defer tc.Close()
	establishCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = establishTunnel(establishCtx, tc, regions)
	cancel()
	if err != nil {
		return fmt.Errorf("establish tunnel: %w", err)
	}
	go maintainHeartbeat(ctx, tc)
	go monitorPath(ctx, tc, regions, false)

	ss := &socks5.Server{
		Logf: logger.WithPrefix(log.Printf, "socks5: "),
		Dialer: func(dialCtx context.Context, network, dest string) (net.Conn, error) {
			// Bound UDP dials too, and cancel pending dials when the proxy stops.
			dialCtx, cancel := context.WithTimeout(dialCtx, 15*time.Second)
			defer cancel()
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			switch network {
			case "tcp":
				return dialTunnel(dialCtx, tc, dest)
			case "udp":
				return dialUDPTunnel(dialCtx, tc, dest)
			default:
				return nil, fmt.Errorf("unsupported SOCKS network %q", network)
			}
		},
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	fmt.Fprintf(os.Stderr, "\nSOCKS5 ready at %s\n  Proxy pod namespace: %s\n  DNS: resolved inside the cluster\n\n  curl --proxy socks5h://%s http://api.%s.svc:8080\n\nPress Ctrl-C to stop and remove the proxy pod.\n\n", ln.Addr(), o.serverNamespace, ln.Addr(), o.serverNamespace)
	slog.Warn("UDP datagrams larger than the tunnel MTU are dropped", slog.Int("maxPayload", tailcat.MaxUDPPayload))
	if err := ss.Serve(ln); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve SOCKS5: %w", err)
	}
	return nil
}
