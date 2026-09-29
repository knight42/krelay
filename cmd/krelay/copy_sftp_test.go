package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	ssh "github.com/tailscale/gliderssh"
)

func testSFTP(t *testing.T) *sftp.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(); _ = server.Close() }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = clientConn.Close(); _ = serverConn.Close(); <-done })
	return client
}

func TestSFTPCopyRoundTrip(t *testing.T) {
	client := testSFTP(t)
	for name, isDir := range map[string]bool{"file": false, "directory": true} {
		t.Run(name, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "source ' ; $HOME")
			file := local
			if isDir {
				if err := os.MkdirAll(filepath.Join(local, "nested"), 0750); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(local, "nested", "binary")
				if err := os.Symlink("nested/binary", filepath.Join(local, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/absolute/target", filepath.Join(local, "absolute")); err != nil {
					t.Fatal(err)
				}
			}
			data := bytes.Repeat([]byte{0, 255, 10, 9, 7}, 100000)
			if err := os.WriteFile(file, data, 0751); err != nil {
				t.Fatal(err)
			}
			remote := filepath.Join(t.TempDir(), "renamed")
			if err := transferCopy(t.Context(), local, remote, isDir, true, client); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			for range 2 {
				if err := transferCopy(t.Context(), dest, remote, isDir, false, client); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(dest, "renamed")
			if isDir {
				target = filepath.Join(target, "nested", "binary")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("roundtrip: %v", err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0751 {
				t.Fatalf("mode: %v %v", info, err)
			}
			if isDir {
				for link, want := range map[string]string{"link": "nested/binary", "absolute": "/absolute/target"} {
					got, err := os.Readlink(filepath.Join(dest, "renamed", link))
					if err != nil || got != want {
						t.Fatalf("link %s: %q %v", link, got, err)
					}
				}
				if err := transferCopy(t.Context(), filepath.Join(t.TempDir(), "no-r"), remote, false, false, client); err == nil {
					t.Fatal("directory accepted without -r")
				}
			}
		})
	}
}

func TestSFTPCopyDestinationSymlink(t *testing.T) {
	client := testSFTP(t)
	remote := t.TempDir()
	if err := os.Mkdir(filepath.Join(remote, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "nested", "file"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dest, filepath.Base(remote))
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := transferCopy(t.Context(), dest, remote, true, false, client); err == nil {
		t.Fatal("followed destination directory symlink")
	}
	got, err := os.ReadFile(filepath.Join(outside, "file"))
	if err != nil || string(got) != "original" {
		t.Fatalf("outside modified: %q %v", got, err)
	}
}

func TestSFTPOldServerAndCancellation(t *testing.T) {
	t.Run("old server", func(t *testing.T) {
		conn := serveSSH(t, func(s ssh.Session) { _ = s.Exit(1) })
		err := withSFTP(t.Context(), conn, func(*sftp.Client) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "SFTP-capable") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("handshake canceled", func(t *testing.T) {
		conn, peer := connPair(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() { _, _ = io.CopyN(io.Discard, peer, 1); cancel() }()
		if err := withSFTP(ctx, conn, func(*sftp.Client) error { return nil }); err == nil {
			t.Fatal("canceled transfer succeeded")
		}
	})
}
