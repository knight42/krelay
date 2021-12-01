package xnet

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/testutils/tcp"
)

func TestProxyHTTPS(t *testing.T) {
	const msg = "Hello, World!"
	ts, tsURL, _ := tcp.NewTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(msg))
	})
	defer ts.Close()

	dialer := net.Dialer{Timeout: time.Second * 10}

	l := tcp.NewTCPServer(t, func(c net.Conn) {
		upConn, err := dialer.DialContext(context.Background(), constants.ProtocolTCP, tsURL.Host)
		if err != nil {
			t.Errorf("dial upstream: %v", err)
			return
		}

		ProxyTCP("req-id", c.(*net.TCPConn), upConn.(*net.TCPConn))
	})
	defer l.Close()

	r := require.New(t)
	resp, err := ts.Client().Get("https://" + l.Addr().String())
	r.NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	r.NoError(err)
	r.Equal(msg, string(body))
}
