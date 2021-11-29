package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/knight42/krelay/pkg/constants"
	"github.com/knight42/krelay/pkg/xio"
	"github.com/knight42/krelay/pkg/xnet"
)

func TestHandleTCPConn(t *testing.T) {
	const msg = "Hello, World!"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(msg))
	}))
	defer ts.Close()
	tsHost := strings.TrimPrefix(ts.URL, "https://")
	host, portStr, err := net.SplitHostPort(tsHost)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatal(err)
	}

	dialer := net.Dialer{Timeout: time.Second * 10}

	ctx := context.Background()

	l, err := net.Listen(constants.ProtocolTCP, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		c, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		handleConn(ctx, c.(*net.TCPConn), &dialer)
	}()

	client := ts.Client()
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("dial: %w", err)
		}
		hdr := xnet.Header{
			RequestID: uuid.New(),
			Protocol:  xnet.ProtocolTCP,
			Port:      uint16(port),
			Addr: xnet.Addr{
				Type: xnet.AddrTypeHost,
				Host: host,
			},
		}
		_, err = xio.WriteFull(c, hdr.Marshal())
		if err != nil {
			return nil, fmt.Errorf("write header: %w", err)
		}
		return c, nil
	}

	resp, err := ts.Client().Get("https://" + l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Got body: %s", string(body))
	assert.Equal(t, msg, string(body))
}
