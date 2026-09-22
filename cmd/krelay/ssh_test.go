package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
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

// connPair returns two connected net.Conn ends backed by a kernel-buffered
// socketpair. net.Pipe would deadlock (it is unbuffered, and both SSH sides
// start by writing their version banner), and a filesystem unix socket in
// t.TempDir() can exceed the ~104-byte socket path limit on macOS.
func connPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		// net.FileConn dups the fd, so the os.File wrapper is closed here.
		f := os.NewFile(uintptr(fd), "socketpair")
		conns[i], err = net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}
	}
	t.Cleanup(func() {
		conns[0].Close()
		conns[1].Close()
	})
	return conns[0], conns[1]
}

// serveSSH runs an SSH server with the given session handler on one end of a
// socketpair and returns the client end.
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

	client, server := connPair(t)
	go srv.HandleConn(server)
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
			err := runSSHExec(t.Context(), conn, tc.command, strings.NewReader(tc.stdin), &stdout, &stderr)
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

// TestRunSSHExecCanceled covers Ctrl-C/SIGTERM during a running command:
// cancellation must abort the blocked sess.Run and surface a non-zero error,
// not wait for the remote command.
func TestRunSSHExecCanceled(t *testing.T) {
	sessionStarted := make(chan struct{})
	conn := serveSSH(t, func(s ssh.Session) {
		close(sessionStarted)
		<-s.Context().Done() // never exits on its own
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-sessionStarted
		cancel()
	}()

	err := runSSHExec(ctx, conn, "sleep infinity", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runSSHExec() error = %v, want context.Canceled", err)
	}
	if _, ok := errors.AsType[exitCodeError](err); ok {
		t.Fatal("cancellation must not masquerade as a remote exit status")
	}
}
