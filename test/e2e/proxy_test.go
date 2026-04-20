//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"
)

func TestSOCKS5Proxy(t *testing.T) {
	ki := startKrelay(t, 1, "SOCKS5 server is running", "proxy", "--listen", "127.0.0.1:0")
	port := ki.localPorts(t)[0]

	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, proxy.Direct)
	require.NoError(t, err)

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
	}

	svcAddr := fmt.Sprintf("http://test-nginx-svc.%s.svc.cluster.local", testNS)
	var resp *http.Response
	for range 10 {
		resp, err = httpClient.Get(svcAddr)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
