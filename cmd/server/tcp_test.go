package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/knight42/krelay/pkg/testutils/tcp"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func TestHandleTCPConn(t *testing.T) {
	const msg = "Hello, World!"
	ts, tsURL, tsPort := tcp.NewTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(msg))
	})
	defer ts.Close()

	dialer := net.Dialer{Timeout: time.Second * 10}

	ctx := context.Background()

	l := tcp.NewTCPServer(t, func(c net.Conn) {
		handleConn(ctx, c.(*net.TCPConn), &dialer)
	})
	defer l.Close()

	client := ts.Client()
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("dial: %w", err)
		}
		hdr := xnet.Header{
			RequestID: xnet.NewRequestID(),
			Protocol:  xnet.ProtocolTCP,
			Port:      tsPort,
			Addr:      xnet.AddrFromHost(tsURL.Hostname()),
		}
		_, err = xio.WriteFull(c, hdr.Marshal())
		if err != nil {
			return nil, fmt.Errorf("write header: %w", err)
		}
		return c, nil
	}

	r := require.New(t)
	resp, err := ts.Client().Get("https://" + l.Addr().String())
	r.NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	r.NoError(err)
	t.Logf("Got body: %s", string(body))
	r.Equal(msg, string(body))
}
