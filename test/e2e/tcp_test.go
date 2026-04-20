//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

func TestTCPForwardPod(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding", "-n", testNS, "pod/test-nginx-pod", fmt.Sprintf("%d:80", port))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
}

func TestTCPForwardService(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding", "-n", testNS, "svc/test-nginx-svc", fmt.Sprintf("%d:80", port))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
}

func TestTCPForwardDeployment(t *testing.T) {
	port := freePort(t)
	startKrelay(t, "Forwarding", "-n", testNS, "deploy/test-nginx-deploy", fmt.Sprintf("%d:80", port))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
}
