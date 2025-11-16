package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/httpstream"

	"github.com/knight42/krelay/pkg/kube"
	slogutil "github.com/knight42/krelay/pkg/slog"
	"github.com/knight42/krelay/pkg/xnet"
)

// socks5Handshake was excerpted from https://github.com/shadowsocks/go-shadowsocks2/blob/e1fe9ea737409e4d71efaa65e3caefa42a8fc188/socks/socks.go
func socks5Handshake(clientConn net.Conn) (ap xnet.AddrPort, err error) {
	// maxAddrLen is the maximum size of SOCKS address in bytes.
	const maxAddrLen = 256
	br := bytesReader{
		// Read RFC 1928 for request and reply structure and sizes.
		buf: make([]byte, maxAddrLen),
		r:   clientConn,
	}

	var data []byte
	// read VER, NMETHODS, METHODS
	data, err = br.ReadBytes(2)
	if err != nil {
		return
	}

	nmethods := data[1]
	_, err = br.ReadBytes(int(nmethods))
	if err != nil {
		return
	}

	// write VER METHOD
	// VERSION 5, METHOD 0 (no authentication)
	_, err = clientConn.Write([]byte{5, 0})
	if err != nil {
		return
	}

	// read VER CMD RSV ATYP DST.ADDR DST.PORT
	data, err = br.ReadBytes(3)
	if err != nil {
		return
	}
	cmd := data[1]

	data, err = br.ReadBytes(1) // read 1st byte for address type
	if err != nil {
		return
	}
	adrType := data[0]

	var addr xnet.Addr
	switch adrType {
	case 1: // IPv4
		data, err = br.ReadBytes(net.IPv4len)
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeIP, copyBuffer(data))

	case 4: // IPv6
		data, err = br.ReadBytes(net.IPv6len)
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeIP, copyBuffer(data))

	case 3: // Domain name
		data, err = br.ReadString() // read domain name
		if err != nil {
			return
		}
		addr = xnet.AddrFromBytes(xnet.AddrTypeHost, copyBuffer(data))

	default:
		// X'08' Address type not supported
		_, _ = clientConn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return ap, fmt.Errorf("unsupported address type: %d", adrType)
	}

	port, err := br.ReadUint16() // read DST.PORT
	if err != nil {
		return
	}

	switch cmd {
	case 1: // CONNECT
		_, err = clientConn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		if err != nil {
			return
		}

	default:
		// X'07' Command not supported
		_, _ = clientConn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return ap, fmt.Errorf("unsupported command: %d", cmd)
	}

	return xnet.AddrPortFrom(addr, port), nil
}

func handleSOCKS5Conn(clientConn net.Conn, serverConn httpstream.Connection) {
	ap, err := socks5Handshake(clientConn)
	if err != nil {
		slog.Error("Fail to handle SOCKS5 handshake", slogutil.Error(err))
		return
	}

	go handleTCPConn(clientConn, serverConn, ap)
}

func runSOCKS5Server(l net.Listener, streamConn httpstream.Connection) {
	slog.Info("SOCKS5 server is running", slog.String("address", l.Addr().String()))
	for {
		select {
		case <-streamConn.CloseChan():
			return
		default:
		}

		c, err := l.Accept()
		if err != nil {
			slog.Error("Fail to accept tcp connection", slogutil.Error(err))
			return
		}
		go handleSOCKS5Conn(c, streamConn)
	}
}

type proxyOptions struct {
	kf *kube.Flags

	listenAddr string
}

func (o *proxyOptions) Run(ctx context.Context, _ []string) error {
	l, err := net.Listen("tcp", o.listenAddr)
	if err != nil {
		return err
	}
	defer l.Close()

	createdPod, err := o.kf.RunServerPod(ctx)
	if err != nil {
		return err
	}

	defer createdPod.Close()

	streamConn := createdPod.StreamConn()
	go runSOCKS5Server(l, streamConn)

	select {
	case <-streamConn.CloseChan():
		slog.Info("Lost connection to krelay-server pod")
	case <-ctx.Done():
	}

	return nil
}

func newProxyCommand(kf *kube.Flags) *cobra.Command {
	o := proxyOptions{
		kf: kf,
	}
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run a SOCKS5 proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			return o.Run(ctx, args)
		},
	}
	flags := cmd.LocalFlags()

	flags.StringVarP(&o.listenAddr, "listen", "l", "127.0.0.1:1080", "SOCKS5 proxy listen address")
	return cmd
}
