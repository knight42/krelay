//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

func TestForwardToClusterIP(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", fmt.Sprintf("ip/%s", svcClusterIP), ":80")
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}

func TestForwardToHostname(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding",
		fmt.Sprintf("host/test-nginx-svc.%s.svc.cluster.local", testNS), ":80")
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}
