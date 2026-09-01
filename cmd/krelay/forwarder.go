package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/tailscale/tailcat"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/ports"
	"github.com/knight42/krelay/pkg/protocol"
	"github.com/knight42/krelay/pkg/resolver"
)

type forwarder struct {
	target     resolver.Target
	getter     resolver.AddrGetter
	ports      ports.Pair
	listenAddr string

	ln net.Listener
}

func (f *forwarder) listen() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(f.listenAddr, strconv.Itoa(int(f.ports.Local))))
	if err != nil {
		return err
	}
	f.ln = ln
	slog.Info("Forwarding",
		slog.String("from", ln.Addr().String()),
		slog.String("to", fmt.Sprintf("%s:%d", f.target, f.ports.Remote)),
	)
	return nil
}

func (f *forwarder) close() {
	if f.ln != nil {
		_ = f.ln.Close()
	}
}

func (f *forwarder) run(ctx context.Context, tc *tailcat.Client) {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("Fail to accept connection", slog.Any("error", err))
			}
			return
		}
		go f.handleConn(ctx, tc, conn)
	}
}

func (f *forwarder) handleConn(ctx context.Context, tc *tailcat.Client, conn net.Conn) {
	defer conn.Close()

	host, err := f.getter.Get(ctx)
	if err != nil {
		slog.Error("Fail to resolve target", slog.String("target", f.target.String()), slog.Any("error", err))
		return
	}
	dest := net.JoinHostPort(host, strconv.Itoa(int(f.ports.Remote)))

	remote, err := dialTunnel(ctx, tc, dest)
	if err != nil {
		slog.Error("Fail to connect", slog.String("dest", dest), slog.Any("error", err))
		return
	}
	slog.Debug("Connected", slog.String("client", conn.RemoteAddr().String()), slog.String("dest", dest))
	tailcat.ProxyConns(conn, remote)
}

// dialTunnel opens a tunnel connection to krelay-server and completes the
// dial handshake for dest (empty dest opens a heartbeat connection).
func dialTunnel(ctx context.Context, tc *tailcat.Client, dest string) (net.Conn, error) {
	conn, err := tc.DialTCPPort(ctx, constants.TunnelPort)
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	if err := protocol.WriteDialRequest(conn, dest); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send dial request: %w", err)
	}
	if err := protocol.ReadDialResponse(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// maintainHeartbeat keeps one idle tunnel connection open so krelay-server
// knows a client is still attached and does not shut down for inactivity.
func maintainHeartbeat(ctx context.Context, tc *tailcat.Client) {
	for ctx.Err() == nil {
		conn, err := dialTunnel(ctx, tc, "")
		if err != nil {
			slog.Debug("Fail to open heartbeat connection", slog.Any("error", err))
		} else {
			// Block until the connection drops (the server never writes).
			_, _ = io.Copy(io.Discard, conn)
			conn.Close()
			slog.Debug("Heartbeat connection closed")
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
