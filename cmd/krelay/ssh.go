package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/knight42/krelay/pkg/kube"
)

const serverSSHPort = 22

// runSSH implements `kubectl relay ssh/NODE`. It creates a krelay-server Job
// scheduled on the target node (privileged + hostPID), establishes the
// WireGuard tunnel, and forwards a local TCP port to the server's built-in SSH
// server. The SSH server uses nsenter to give the client a shell in the host
// namespaces.
func (o *options) runSSH(ctx context.Context, nodeName string) error {
	priv := key.NewNode()
	token := o.serverToken
	var sj *kube.ServerJob
	if token == "" {
		var err error
		sj, err = startServer(ctx, o, priv, nodeName)
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
		slog.Debug("Got connection token", slog.String("token", token))
	}

	tcLogf := logger.Discard
	if o.verbosity >= 5 {
		tcLogf = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	tc := &tailcat.Client{
		Server: tailcat.Addr(token),
		Key:    priv,
		Logf:   tcLogf,
	}
	defer tc.Close()

	slog.Info("Establishing tunnel to krelay-server")
	establishCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := establishTunnel(establishCtx, tc)
	cancel()
	if err != nil {
		return fmt.Errorf("establish tunnel: %w", err)
	}

	go maintainHeartbeat(ctx, tc)
	go monitorPath(ctx, tc)

	ln, err := net.Listen("tcp", net.JoinHostPort(o.address, "0"))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stderr, "\n  To connect, run:\n\n    ssh -p %d %s\n\n", addr.Port, addr.IP)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("SSH proxy accept failed", slog.Any("error", err))
				}
				return
			}
			go proxySSH(ctx, tc, conn)
		}
	}()

	<-ctx.Done()
	return nil
}

// proxySSH forwards one local TCP connection to the krelay-server's SSH port
// through the tailcat tunnel.
func proxySSH(ctx context.Context, tc *tailcat.Client, conn net.Conn) {
	defer conn.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	remote, err := tc.DialTCPPort(dialCtx, serverSSHPort)
	cancel()
	if err != nil {
		slog.Error("Dial SSH port on krelay-server failed", slog.Any("error", err))
		return
	}
	tailcat.ProxyConns(conn, remote)
}

// parseSSHTarget extracts the optional user and the node name from an
// "[user@]ssh/NODE" argument.
func parseSSHTarget(arg string) (user, nodeName string, isSSH bool) {
	u, rest, hasUser := strings.Cut(arg, "@")
	if !hasUser {
		rest = u
		u = ""
	}
	typ, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return u, "", false
	}
	if !strings.EqualFold(typ, "ssh") {
		return u, "", false
	}
	return u, name, true
}
