//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultiplePorts(t *testing.T) {
	ki := startKrelay(t, 2, "Forwarding", "-n", testNS, "svc/test-nginx-svc", ":80", ":80")
	ports := ki.localPorts(t)
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ports[0]))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ports[1]))
}

func TestMultiTargetFile(t *testing.T) {
	content := fmt.Sprintf("-n %s pod/test-nginx-pod :80\n-n %s svc/test-nginx-svc :80\n",
		testNS, testNS)

	tmpFile, err := os.CreateTemp("", "krelay-targets-*.txt")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	ki := startKrelay(t, 2, "Forwarding", "-f", tmpFile.Name())
	ports := ki.localPorts(t)
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ports[0]))
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ports[1]))
}
