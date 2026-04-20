//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobCreatedNotPod(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", "-n", testNS, "svc/test-nginx-svc", ":80")

	jobs := listKrelayJobs(t)
	require.NotEmpty(t, jobs, "expected a krelay-server Job in namespace %s", testNS)
	t.Logf("krelay-server Job(s): %v", jobs)

	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}

func TestJobCleanedUpOnSIGINT(t *testing.T) {
	ki := startKrelay(t, 1, "Forwarding", "-n", testNS, "svc/test-nginx-svc", ":80")

	jobs := listKrelayJobs(t)
	require.NotEmpty(t, jobs)

	ki.stop()
	waitForNoKrelayJobs(t, 30*time.Second)
}

func TestIdleTimeoutExitsAfterClientKilled(t *testing.T) {
	ki := startKrelayWithServerArgs(t,
		[]string{"--idle-timeout=15s"},
		1, "Forwarding",
		"-n", testNS, "svc/test-nginx-svc", ":80",
	)

	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))

	jobs := listKrelayJobs(t)
	require.NotEmpty(t, jobs, "expected krelay-server Job before kill")

	ki.kill()

	// Server should idle-timeout (15s) + TTL GC (10s) = ~25s.
	// Give generous margin.
	waitForNoKrelayJobs(t, 60*time.Second)
}

func TestHeartbeatKeepsServerAlive(t *testing.T) {
	ki := startKrelayWithServerArgs(t,
		[]string{"--idle-timeout=10s"},
		1, "Forwarding",
		"-n", testNS, "svc/test-nginx-svc", ":80",
	)

	// Wait longer than the idle timeout while client is alive
	// (heartbeats should prevent server exit).
	time.Sleep(20 * time.Second)

	jobs := listKrelayJobs(t)
	assert.NotEmpty(t, jobs, "server should still be alive — heartbeat should prevent idle exit")

	// Verify forwarding still works
	httpGetOK(t, fmt.Sprintf("http://127.0.0.1:%d/", ki.localPorts(t)[0]))
}
