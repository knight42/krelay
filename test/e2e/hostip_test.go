//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

func TestForwardToClusterIP(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding", fmt.Sprintf("ip/%s", svcClusterIP), fmt.Sprintf("%d:80", port))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
}

func TestForwardToHostname(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding",
		fmt.Sprintf("host/test-nginx-svc.%s.svc.cluster.local", testNS),
		fmt.Sprintf("%d:80", port))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
}
