//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultiplePorts(t *testing.T) {
	port1 := freePort(t)
	port2 := freePort(t)
	startKrelay(t, "Forwarding", "-n", testNS, "svc/test-nginx-svc",
		fmt.Sprintf("%d:80", port1), fmt.Sprintf("%d:80", port2))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port1))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port2))
}

func TestMultiTargetFile(t *testing.T) {
	port1 := freePort(t)
	port2 := freePort(t)

	content := fmt.Sprintf("-n %s pod/test-nginx-pod %d:80\n-n %s svc/test-nginx-svc %d:80\n",
		testNS, port1, testNS, port2)

	tmpFile, err := os.CreateTemp("", "krelay-targets-*.txt")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	startKrelay(t, "Forwarding", "-f", tmpFile.Name())
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port1))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", port2))
}
