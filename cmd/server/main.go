package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/knight42/krelay/pkg/constants"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

type options struct {
	connectTimeout time.Duration
}

func (o *options) run(ctx context.Context) error {
	tcpListener, err := net.Listen(constants.ProtocolTCP, fmt.Sprintf("0.0.0.0:%d", constants.ServerPort))
	if err != nil {
		return err
	}
	defer tcpListener.Close()

	dialer := net.Dialer{Timeout: o.connectTimeout}
	slog.Info("Accepting connections")
	for {
		c, err := tcpListener.Accept()
		if err != nil {
			var tmpErr interface {
				Temporary() bool
			}
			if errors.As(err, &tmpErr) && tmpErr.Temporary() {
				continue
			}
			slog.Error("Fail to accept connection", slogutil.Error(err))
			return err
		}
		go handleConn(ctx, c.(*net.TCPConn), &dialer)
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
	flags.IntP("v", "v", 0, "bogus flag to keep backward compatibility")
	_ = c.Execute()
}
