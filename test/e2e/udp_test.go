//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUDPForwardService(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding", "-n", "kube-system", "svc/kube-dns", fmt.Sprintf("%d:53@udp", port))

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := r.LookupHost(ctx, "kubernetes.default.svc.cluster.local")
	require.NoError(t, err)
	require.NotEmpty(t, addrs)
}
