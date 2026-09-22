package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func TestMuxID(t *testing.T) {
	testCases := map[string]struct {
		a, b     []string
		wantSame bool
	}{
		"same inputs match": {
			a:        []string{"ctx", "ns", "node"},
			b:        []string{"ctx", "ns", "node"},
			wantSame: true,
		},
		"different node differs": {
			a: []string{"ctx", "ns", "node-1"},
			b: []string{"ctx", "ns", "node-2"},
		},
		"parts are delimited": {
			a: []string{"ab", "c"},
			b: []string{"a", "bc"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			idA, idB := muxID(tc.a...), muxID(tc.b...)
			if (idA == idB) != tc.wantSame {
				t.Fatalf("muxID(%q) = %s, muxID(%q) = %s, wantSame = %v", tc.a, idA, tc.b, idB, tc.wantSame)
			}
			// The id is embedded in a unix socket path, which has a tight
			// length limit (~104 bytes on macOS).
			if len(idA) != 16 {
				t.Fatalf("len(muxID) = %d, want 16", len(idA))
			}
		})
	}
}

// writeKubeconfig writes a kubeconfig with two clusters; ctx-a and ctx-a2
// both point at cluster A, ctx-b at cluster B.
func writeKubeconfig(t *testing.T, serverA, serverB string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: a
  cluster: {server: %q}
- name: b
  cluster: {server: %q}
contexts:
- name: ctx-a
  context: {cluster: a, user: u}
- name: ctx-a2
  context: {cluster: a, user: u}
- name: ctx-b
  context: {cluster: b, user: u}
users:
- name: u
  user: {}
current-context: ctx-a
`, serverA, serverB)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOptions(kubeconfig, kubeContext, namespace string) *options {
	o := &options{cf: genericclioptions.NewConfigFlags(true), serverNamespace: namespace}
	*o.cf.KubeConfig = kubeconfig
	*o.cf.Context = kubeContext
	return o
}

func TestMuxPathsClusterIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	kubeconfig := writeKubeconfig(t, "https://a.example:6443", "https://b.example:6443")

	testCases := map[string]struct {
		a, b     *options
		nodeA    string
		nodeB    string
		wantSame bool
	}{
		"different clusters, same flags shape": {
			a:        testOptions(kubeconfig, "ctx-a", "default"),
			b:        testOptions(kubeconfig, "ctx-b", "default"),
			nodeA:    "node-1",
			nodeB:    "node-1",
			wantSame: false,
		},
		"same cluster via different contexts shares the daemon": {
			a:        testOptions(kubeconfig, "ctx-a", "default"),
			b:        testOptions(kubeconfig, "ctx-a2", "default"),
			nodeA:    "node-1",
			nodeB:    "node-1",
			wantSame: true,
		},
		"different nodes": {
			a:        testOptions(kubeconfig, "ctx-a", "default"),
			b:        testOptions(kubeconfig, "ctx-a", "default"),
			nodeA:    "node-1",
			nodeB:    "node-2",
			wantSame: false,
		},
		"different server namespaces": {
			a:        testOptions(kubeconfig, "ctx-a", "default"),
			b:        testOptions(kubeconfig, "ctx-a", "payments"),
			nodeA:    "node-1",
			nodeB:    "node-1",
			wantSame: false,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			pathsA, err := tc.a.muxPaths(tc.nodeA)
			if err != nil {
				t.Fatal(err)
			}
			pathsB, err := tc.b.muxPaths(tc.nodeB)
			if err != nil {
				t.Fatal(err)
			}
			if (pathsA.sock == pathsB.sock) != tc.wantSame {
				t.Fatalf("sock A = %s, sock B = %s, wantSame = %v", pathsA.sock, pathsB.sock, tc.wantSame)
			}
		})
	}
}

func TestParseMuxStatus(t *testing.T) {
	testCases := map[string]struct {
		line       string
		wantStatus muxStatus
		wantMsg    string
		wantErr    bool
	}{
		"ready":              {line: "READY\n", wantStatus: muxReady},
		"busy":               {line: "BUSY\n", wantStatus: muxBusy},
		"error with message": {line: "ERROR node \"x\" not found\n", wantStatus: muxError, wantMsg: "node \"x\" not found"},
		"unknown":            {line: "BOGUS\n", wantErr: true},
		"empty":              {line: "", wantErr: true},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			status, msg, err := parseMuxStatus(tc.line)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseMuxStatus(%q) error = %v, wantErr = %v", tc.line, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if status != tc.wantStatus || msg != tc.wantMsg {
				t.Fatalf("parseMuxStatus(%q) = (%v, %q), want (%v, %q)", tc.line, status, msg, tc.wantStatus, tc.wantMsg)
			}
		})
	}
}
