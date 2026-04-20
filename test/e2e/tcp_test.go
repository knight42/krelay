//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

func TestTCPForwardPod(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", "-n", testNS, "pod/test-nginx-pod", ":80")
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}

func TestTCPForwardService(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", "-n", testNS, "svc/test-nginx-svc", ":80")
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}

func TestTCPForwardDeployment(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", "-n", testNS, "deploy/test-nginx-deploy", ":80")
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}
