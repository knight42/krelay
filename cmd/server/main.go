package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/knight42/krelay/pkg/constants"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

type options struct {
	connectTimeout time.Duration
	idleTimeout    time.Duration
}

// idleTracker closes the listener when no connections have been active for
// idleTimeout, so the Job can transition to Complete and be garbage-collected
// by its TTL if the client crashed without cleaning up.
type idleTracker struct {
	timeout      time.Duration
	activeConns  atomic.Int64
	lastActivity atomic.Int64
	// hadConn becomes true after the first connection is accepted.
	// The idle timer only fires after this — so the server never
	// exits before a client has connected at least once.
	hadConn atomic.Bool
}

func newIdleTracker(timeout time.Duration) *idleTracker {
	t := &idleTracker{timeout: timeout}
	t.lastActivity.Store(time.Now().UnixNano())
	return t
}

func (t *idleTracker) onConnect() {
	t.hadConn.Store(true)
	t.activeConns.Add(1)
	t.lastActivity.Store(time.Now().UnixNano())
}

func (t *idleTracker) onDisconnect() {
	t.activeConns.Add(-1)
	t.lastActivity.Store(time.Now().UnixNano())
}

func (t *idleTracker) monitor(ctx context.Context, lis net.Listener) {
	if t.timeout <= 0 {
		return
	}
	interval := max(t.timeout/4, time.Second)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !t.hadConn.Load() || t.activeConns.Load() > 0 {
				continue
			}
			idle := time.Since(time.Unix(0, t.lastActivity.Load()))
			if idle >= t.timeout {
				slog.Info("Idle timeout reached, shutting down", slog.Duration("idle", idle))
				_ = lis.Close()
				return
			}
		}
	}
}

func (o *options) run(ctx context.Context) error {
	tcpListener, err := net.Listen(constants.ProtocolTCP, fmt.Sprintf("0.0.0.0:%d", constants.ServerPort))
	if err != nil {
		return err
	}
	defer tcpListener.Close()

	dialer := net.Dialer{Timeout: o.connectTimeout}
	tracker := newIdleTracker(o.idleTimeout)
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	defer cancelMonitor()
	go tracker.monitor(monitorCtx, tcpListener)

	slog.Info("Accepting connections", slog.Duration("idleTimeout", o.idleTimeout))
	for {
		c, err := tcpListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var tmpErr interface {
				Temporary() bool
			}
			if errors.As(err, &tmpErr) && tmpErr.Temporary() {
				continue
			}
			slog.Error("Fail to accept connection", slogutil.Error(err))
			return err
		}
		tracker.onConnect()
		go func(c *net.TCPConn) {
			defer tracker.onDisconnect()
			handleConn(ctx, c, &dialer)
		}(c.(*net.TCPConn))
	}
}

func writeACK(c net.Conn, ack xnet.Acknowledgement) error {
	data := ack.Marshal()
	_, err := c.Write(data)
	return err
}

func ackCodeFromErr(err error) xnet.AckCode {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return xnet.AckCodeNoSuchHost
		}
		if dnsErr.IsTimeout {
			return xnet.AckCodeResolveTimeout
		}
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return xnet.AckCodeConnectTimeout
	}

	return xnet.AckCodeUnknownError
}

func handleConn(ctx context.Context, c *net.TCPConn, dialer *net.Dialer) {
	defer c.Close()

	hdr := xnet.Header{}
	err := hdr.FromReader(c)
	if err != nil {
		slog.Error("Fail to read header", slogutil.Error(err))
		return
	}

	dstAddr := xnet.JoinHostPort(hdr.Addr.String(), hdr.Port)
	l := slog.With(slog.String(constants.LogFieldRequestID, hdr.RequestID))
	switch hdr.Protocol {
	case xnet.ProtocolTCP:
		upstreamConn, err := dialer.DialContext(ctx, constants.ProtocolTCP, dstAddr)
		if err != nil {
			l.Error("Fail to create tcp connection", slog.String(constants.LogFieldDestAddr, dstAddr), slogutil.Error(err))
			_ = writeACK(c, xnet.Acknowledgement{
				Code: ackCodeFromErr(err),
			})
			return
		}
		err = writeACK(c, xnet.Acknowledgement{
			Code: xnet.AckCodeOK,
		})
		if err != nil {
			l.Error("Fail to write ack", slogutil.Error(err))
			return
		}
		l.Info("Start proxy tcp request", slog.String(constants.LogFieldDestAddr, dstAddr))
		xnet.ProxyTCP(hdr.RequestID, c, upstreamConn.(*net.TCPConn))

	case xnet.ProtocolUDP:
		upstreamConn, err := dialer.DialContext(ctx, constants.ProtocolUDP, dstAddr)
		if err != nil {
			l.Error("Fail to create udp connection", slog.String(constants.LogFieldDestAddr, dstAddr), slogutil.Error(err))
			_ = writeACK(c, xnet.Acknowledgement{
				Code: ackCodeFromErr(err),
			})
			return
		}
		err = writeACK(c, xnet.Acknowledgement{
			Code: xnet.AckCodeOK,
		})
		if err != nil {
			l.Error("Fail to write ack", slogutil.Error(err))
			return
		}
		l.Info("Start proxy udp request", slog.String(constants.LogFieldDestAddr, dstAddr))
		udpConn := &xnet.UDPConn{UDPConn: upstreamConn.(*net.UDPConn)}
		xnet.ProxyUDP(hdr.RequestID, c, udpConn)

	default:
		l.Error("Unknown protocol", slog.String(constants.LogFieldDestAddr, dstAddr), slog.Any(constants.LogFieldProtocol, hdr.Protocol))
		err = writeACK(c, xnet.Acknowledgement{
			Code: xnet.AckCodeUnknownProtocol,
		})
		if err != nil {
			l.Error("Fail to write ack", slogutil.Error(err))
			return
		}
	}
}

func main() {
	o := options{}
	c := cobra.Command{
		Use: constants.ServerName,
		RunE: func(_ *cobra.Command, _ []string) (err error) {
			return o.run(context.TODO())
		},
		SilenceUsage: true,
	}
	flags := c.Flags()
	flags.DurationVar(&o.connectTimeout, "connect-timeout", time.Second*10, "Timeout for connecting to upstream")
	flags.DurationVar(&o.idleTimeout, "idle-timeout", 5*time.Minute, "Exit when no connections have been active for this duration after the last client disconnects. 0 disables.")
	flags.IntP("v", "v", 0, "bogus flag to keep backward compatibility. This flag will be removed in the future.")
	_ = c.Execute()
}
