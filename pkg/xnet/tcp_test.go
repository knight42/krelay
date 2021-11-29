package xnet

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/knight42/krelay/pkg/constants"
)

func TestProxyHTTPS(t *testing.T) {
	const msg = "Hello, World!"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(msg))
	}))
	defer ts.Close()
	tsHost := strings.TrimPrefix(ts.URL, "https://")

	l, err := net.Listen(constants.ProtocolTCP, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	dialer := net.Dialer{Timeout: time.Second * 10}

	go func() {
		c, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}

		upConn, err := dialer.DialContext(context.Background(), constants.ProtocolTCP, tsHost)
		if err != nil {
			t.Errorf("dial upstream: %v", err)
			return
		}

		ProxyTCP("req-id", c.(*net.TCPConn), upConn.(*net.TCPConn))
	}()

	resp, err := ts.Client().Get("https://" + l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, msg, string(body))
}
