package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"
)

func generateTestHostKey() (gossh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
}

func TestParseSSHTarget(t *testing.T) {
	testCases := map[string]struct {
		arg      string
		wantNode string
		wantSSH  bool
	}{
		"ssh target":            {arg: "ssh/node-1", wantNode: "node-1", wantSSH: true},
		"uppercase type":        {arg: "SSH/node-1", wantNode: "node-1", wantSSH: true},
		"service":               {arg: "svc/nginx", wantSSH: false},
		"missing node":          {arg: "ssh/", wantSSH: false},
		"no slash":              {arg: "ssh", wantSSH: false},
		"node with inner slash": {arg: "ssh/node/extra", wantNode: "node/extra", wantSSH: true},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			node, isSSH := parseSSHTarget(tc.arg)
			if isSSH != tc.wantSSH || node != tc.wantNode {
				t.Fatalf("parseSSHTarget(%q) = (%q, %v), want (%q, %v)", tc.arg, node, isSSH, tc.wantNode, tc.wantSSH)
			}
		})
	}
}

// serveSSH runs an SSH server with the given session handler on a unix socket
// pair (net.Pipe deadlocks: it is unbuffered, and both SSH sides start by
// writing their version banner) and returns the client end.
func serveSSH(t *testing.T, handler ssh.Handler) net.Conn {
	t.Helper()
	hostKey, err := generateTestHostKey()
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	srv := &ssh.Server{
		Handler:             handler,
		NoClientAuthHandler: func(ssh.Context) error { return nil },
		ChannelHandlers:     map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
	}
	srv.AddHostKey(hostKey)

	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "ssh.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		srv.HandleConn(conn)
	}()
	client, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client
}

func TestRunSSHExec(t *testing.T) {
	testCases := map[string]struct {
		command    string
		stdin      string
		handler    ssh.Handler
		wantErr    error
		wantStdout string
		wantStderr string
	}{
		"exit status is propagated": {
			command: "false",
			handler: func(s ssh.Session) {
				_ = s.Exit(3)
			},
			wantErr: exitCodeError{code: 3},
		},
		"command and streams pass through": {
			command: "cat >&2 && echo done",
			stdin:   "input-data",
			handler: func(s ssh.Session) {
				if got, want := s.RawCommand(), "cat >&2 && echo done"; got != want {
					_, _ = io.WriteString(s.Stderr(), "unexpected command "+got)
					_ = s.Exit(1)
					return
				}
				in, _ := io.ReadAll(s)
				_, _ = s.Stderr().Write(in)
				_, _ = io.WriteString(s, "done")
				_ = s.Exit(0)
			},
			wantStdout: "done",
			wantStderr: "input-data",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			conn := serveSSH(t, tc.handler)
			var stdout, stderr bytes.Buffer
			err := runSSHExec(conn, tc.command, strings.NewReader(tc.stdin), &stdout, &stderr)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("runSSHExec() error = %v, want %v", err, tc.wantErr)
			}
			if got := stdout.String(); got != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tc.wantStdout)
			}
			if got := stderr.String(); got != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
			}
		})
	}
}
