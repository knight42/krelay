package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/pkg/sftp"
	ssh "github.com/tailscale/gliderssh"
)

func sftpSessionHandler(sess ssh.Session) {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintln(sess.Stderr(), err)
		_ = sess.Exit(1)
		return
	}
	// Execute our own binary before chroot: no binary, shell, or tar is required
	// on the host. Cancellation kills only this session's worker.
	cmd := exec.CommandContext(sess.Context(), exe, "--sftp-server")
	runWithPipes(sess, cmd)
}

func serveHostSFTP() error {
	// hostPID exposes the node's PID 1. A process-wide chroot makes absolute
	// symlinks resolve on the host, which prefixing paths with /proc/1/root cannot.
	if err := syscall.Chroot("/proc/1/root"); err != nil {
		return fmt.Errorf("SFTP host root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	stream := struct {
		io.Reader
		io.WriteCloser
	}{os.Stdin, os.Stdout}
	srv, err := sftp.NewServer(stream)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	err = srv.Serve()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
