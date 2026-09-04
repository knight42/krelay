package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/tailscale/tailcat"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/protocol"
)

// udpSessionIdleTimeout is how long a local UDP peer may stay silent (in both
// directions) before its tunnel flow is torn down. It is kept below the
// server-side tailcat.DefaultUDPIdleTimeout (2m) so the client, not the
// server, decides when a flow ends.
const udpSessionIdleTimeout = 90 * time.Second

// maxPendingDatagrams bounds the datagrams buffered per session while its
// tunnel handshake is in flight; excess datagrams are dropped, which UDP
// applications must tolerate anyway.
const maxPendingDatagrams = 8

// runUDP relays datagrams between the local UDP socket and per-peer tunnel
// flows. Each distinct local source address gets its own flow through the
// tunnel, so replies find their way back to the right peer.
func (f *forwarder) runUDP(ctx context.Context, tc *tailcat.Client) {
	r := &udpRelay{
		fwd:      f,
		tc:       tc,
		sessions: make(map[netip.AddrPort]*udpSession),
	}
	buf := make([]byte, 65535)
	for {
		n, peer, err := f.udpLn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("Fail to read datagram", slog.Any("error", err))
			}
			return
		}
		r.deliver(ctx, peer, buf[:n])
	}
}

type udpRelay struct {
	fwd *forwarder
	tc  *tailcat.Client

	mu       sync.Mutex
	sessions map[netip.AddrPort]*udpSession
}

type udpSession struct {
	mu         sync.Mutex
	conn       tailcat.ConnPacketConn // nil until the handshake completes
	pending    [][]byte               // datagrams queued during the handshake
	lastActive time.Time
}

// deliver routes one local datagram to the peer's session, creating the
// session (and its tunnel flow) on first sight of the peer.
func (r *udpRelay) deliver(ctx context.Context, peer netip.AddrPort, datagram []byte) {
	r.mu.Lock()
	sess := r.sessions[peer]
	if sess == nil {
		sess = &udpSession{
			pending:    [][]byte{bytes.Clone(datagram)},
			lastActive: time.Now(),
		}
		r.sessions[peer] = sess
		r.mu.Unlock()
		go r.runSession(ctx, peer, sess)
		return
	}
	r.mu.Unlock()

	sess.mu.Lock()
	sess.lastActive = time.Now()
	conn := sess.conn
	if conn == nil {
		if len(sess.pending) < maxPendingDatagrams {
			sess.pending = append(sess.pending, bytes.Clone(datagram))
		}
		sess.mu.Unlock()
		return
	}
	sess.mu.Unlock()
	if _, err := conn.Write(datagram); err != nil {
		slog.Debug("Fail to write datagram to tunnel", slog.Any("error", err))
	}
}

// runSession owns one peer's tunnel flow: it performs the dial handshake,
// flushes datagrams queued meanwhile, and then pumps tunnel->local until the
// flow closes or falls idle.
func (r *udpRelay) runSession(ctx context.Context, peer netip.AddrPort, sess *udpSession) {
	defer func() {
		r.mu.Lock()
		delete(r.sessions, peer)
		r.mu.Unlock()
	}()

	f := r.fwd
	host, err := f.getter.Get(ctx)
	if err != nil {
		slog.Error("Fail to resolve target", slog.String("target", f.target.String()), slog.Any("error", err))
		return
	}
	dest := net.JoinHostPort(host, strconv.Itoa(int(f.ports.Remote)))

	conn, err := dialUDPTunnel(ctx, r.tc, dest)
	if err != nil {
		slog.Error("Fail to connect", slog.String("dest", dest), slog.Any("error", err))
		return
	}
	defer conn.Close()
	slog.Debug("Connected", slog.String("client", peer.String()), slog.String("dest", dest), slog.String("proto", "udp"))

	sess.mu.Lock()
	sess.conn = conn
	pending := sess.pending
	sess.pending = nil
	sess.mu.Unlock()
	for _, d := range pending {
		if _, err := conn.Write(d); err != nil {
			return
		}
	}

	buf := make([]byte, 65535)
	for {
		sess.mu.Lock()
		deadline := sess.lastActive.Add(udpSessionIdleTimeout)
		sess.mu.Unlock()
		_ = conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && ctx.Err() == nil {
				sess.mu.Lock()
				idle := time.Since(sess.lastActive) >= udpSessionIdleTimeout
				sess.mu.Unlock()
				if !idle {
					// Outbound traffic kept the session alive; keep reading.
					continue
				}
				slog.Debug("UDP session expired", slog.String("client", peer.String()))
			}
			return
		}
		sess.mu.Lock()
		sess.lastActive = time.Now()
		sess.mu.Unlock()
		if _, err := f.udpLn.WriteToUDPAddrPort(buf[:n], peer); err != nil {
			return
		}
	}
}

// dialUDPTunnel opens a UDP flow to krelay-server and completes the dial
// handshake for dest. The request datagram and its acknowledgment travel over
// the tunnel unreliably, so the request is re-sent until acknowledged; the
// server re-acknowledges duplicates, and application data only starts flowing
// after the handshake, keeping the two phases unambiguous.
func dialUDPTunnel(ctx context.Context, tc *tailcat.Client, dest string) (tailcat.ConnPacketConn, error) {
	conn, err := tc.DialUDPPort(ctx, constants.TunnelPort)
	if err != nil {
		return nil, fmt.Errorf("dial udp tunnel: %w", err)
	}
	var req bytes.Buffer
	if err := protocol.WriteDialRequest(&req, dest); err != nil {
		conn.Close()
		return nil, err
	}
	buf := make([]byte, 65535)
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		if _, err := conn.Write(req.Bytes()); err != nil {
			conn.Close()
			return nil, fmt.Errorf("send dial request: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			conn.Close()
			return nil, fmt.Errorf("read dial response: %w", err)
		}
		_ = conn.SetReadDeadline(time.Time{})
		if err := protocol.ReadDialResponse(bytes.NewReader(buf[:n])); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	conn.Close()
	return nil, fmt.Errorf("dial %s: no response from krelay-server", dest)
}
