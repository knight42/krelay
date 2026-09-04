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

	ln    net.Listener // TCP targets
	udpLn *net.UDPConn // UDP targets
}

func (f *forwarder) listen() error {
	addr := net.JoinHostPort(f.listenAddr, strconv.Itoa(int(f.ports.Local)))
	var from string
	if f.ports.Proto == ports.ProtocolUDP {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return err
		}
		f.udpLn, err = net.ListenUDP("udp", udpAddr)
		if err != nil {
			return err
		}
		from = f.udpLn.LocalAddr().String()
	} else {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		f.ln = ln
		from = ln.Addr().String()
	}
	slog.Info("Forwarding",
		slog.String("from", from),
		slog.String("to", fmt.Sprintf("%s:%d", f.target, f.ports.Remote)),
		slog.String("proto", f.ports.Proto),
	)
	return nil
}

func (f *forwarder) bound() bool {
	return f.ln != nil || f.udpLn != nil
}

func (f *forwarder) close() {
	if f.ln != nil {
		_ = f.ln.Close()
	}
	if f.udpLn != nil {
		_ = f.udpLn.Close()
	}
}

func (f *forwarder) run(ctx context.Context, tc *tailcat.Client) {
	if f.ports.Proto == ports.ProtocolUDP {
		f.runUDP(ctx, tc)
		return
	}
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

// establishTunnel pings krelay-server until it acknowledges this client.
// tailcat's Ping re-sends the meow every second within its internal 10s
// window, which covers the race where the server prints its token before its
// DERP registration completes. The outer loop retries whole windows, bounded
// by ctx, for failures a single window can't ride out (e.g. transient
// network trouble on either side).
func establishTunnel(ctx context.Context, tc *tailcat.Client) error {
	for attempt := 1; ; attempt++ {
		res, err := tc.Ping(ctx)
		if err == nil {
			slog.Info("Tunnel established",
				slog.Duration("derpLatency", res.Latency),
				slog.Int("attempt", attempt),
			)
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		slog.Debug("Fail to reach krelay-server, retrying", slog.Int("attempt", attempt), slog.Any("error", err))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// monitorPath logs whether tunnel traffic flows over a direct UDP path or is
// relayed by DERP. Disco pings actively drive NAT traversal, so the tight
// initial cadence also speeds up the upgrade to a direct path; once the path
// settles, it is re-checked occasionally and only changes are logged.
func monitorPath(ctx context.Context, tc *tailcat.Client) {
	const (
		upgradeInterval = 2 * time.Second
		settledInterval = time.Minute
		upgradeWindow   = 30 * time.Second
	)
	start := time.Now()
	interval := upgradeInterval
	var lastPath string
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res, err := tc.DiscoPing(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("Disco ping failed", slog.Any("error", err))
			timer.Reset(interval)
			continue
		}
		var path, via string
		if res.Endpoint != "" {
			path, via = "direct", res.Endpoint
		} else {
			path = "derp"
			via = fmt.Sprintf("derp-%d", res.DERPRegionID)
			// Addresses with embedded relay details carry no real region code.
			if code := res.DERPRegionCode; code != "" && code != fmt.Sprint(res.DERPRegionID) {
				via = fmt.Sprintf("%s(%d)", code, res.DERPRegionID)
			}
		}
		if key := path + " " + via; key != lastPath {
			slog.Info("Tunnel path",
				slog.String("path", path),
				slog.String("via", via),
				slog.Duration("latency", time.Duration(res.LatencySeconds*float64(time.Second)).Round(10*time.Microsecond)),
			)
			lastPath = key
		}
		if path == "direct" || time.Since(start) > upgradeWindow {
			interval = settledInterval
		}
		timer.Reset(interval)
	}
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
