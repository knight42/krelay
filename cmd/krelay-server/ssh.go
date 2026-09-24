package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	ssh "github.com/tailscale/gliderssh"
	gossh "golang.org/x/crypto/ssh"
)

const sshPort = 22

// newSSHHandler returns a function that serves incoming tailcat TCP connections
// on port 22 as SSH sessions. Each session runs nsenter to enter the host
// namespaces (PID 1), giving the SSH client a shell on the node.
func newSSHHandler() func(net.Conn) {
	hostKey, err := generateHostKey()
	if err != nil {
		log.Fatalf("generate SSH host key: %v", err)
	}

	srv := &ssh.Server{
		Handler:             sessionHandler,
		NoClientAuthHandler: func(ssh.Context) error { return nil },
		ChannelHandlers:     map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
		RequestHandlers:     map[string]ssh.RequestHandler{},
	}
	srv.AddHostKey(hostKey)

	return func(c net.Conn) {
		srv.HandleConn(c)
	}
}

func sessionHandler(sess ssh.Session) {
	shell, err := hostShell("/proc/1/root")
	if err == nil {
		err = checkShell(sess.Context(), nsenterCommand(shell, shellProbe))
	}
	if err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "krelay: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	cmd := nsenterCommand(shell, sess.RawCommand())

	env := cmd.Env
	for _, kv := range sess.Environ() {
		if acceptEnv(kv) {
			env = append(env, kv)
		}
	}
	cmd.Env = env

	ptyReq, winCh, isPTY := sess.Pty()
	if isPTY {
		if ptyReq.Term != "" {
			cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		}
		sess.DisablePTYEmulation()
		runWithPTY(sess, cmd, ptyReq, winCh)
	} else {
		runWithPipes(sess, cmd)
	}
}

// nsenterCommand builds the command that enters all host namespaces via PID 1.
func nsenterCommand(shell, rawCmd string) *exec.Cmd {
	nsenter := []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid", "--",
	}
	if rawCmd == "" {
		nsenter = append(nsenter, shell, "-l")
	} else {
		nsenter = append(nsenter, shell, "-c", rawCmd)
	}
	cmd := exec.Command(nsenter[0], nsenter[1:]...)
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}
	return cmd
}

// hostShell selects an executable host bash or sh under the host root
// (/proc/1/root with hostPID). checkShell verifies its shell semantics before use.
func hostShell(root string) (string, error) {
	for _, p := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh", "/usr/bin/sh"} {
		hostPath := filepath.Join(root, p)
		if info, err := os.Stat(hostPath); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("host shell unsupported: no executable bash or sh found on the host")
}

// Exercise variable expansion and a shell builtin without touching host files
// or depending on external tools. A successful exit alone does not prove that
// a restricted command dispatcher actually interpreted the script.
const shellProbe = `krelay_probe=ok; printf 'krelay-shell-%s' "$krelay_probe"`

// checkShell runs the probe in the same namespaces and environment as the
// session, bounded so an unusable shell cannot hang session startup.
func checkShell(ctx context.Context, cmd *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	probe.Env = cmd.Env
	probe.WaitDelay = 100 * time.Millisecond
	output, err := probe.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("host shell check failed: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("host shell check failed: cannot execute shell commands: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if string(output) != "krelay-shell-ok" {
		return errors.New("host shell unsupported: shell command probe returned unexpected output")
	}
	return nil
}

func runWithPTY(sess ssh.Session, cmd *exec.Cmd, ptyReq ssh.Pty, winCh <-chan ssh.Window) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "pty open: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	defer ptmx.Close()
	defer tty.Close()

	_ = pty.Setsize(tty, &pty.Winsize{
		Rows: uint16(ptyReq.Window.Height),
		Cols: uint16(ptyReq.Window.Width),
	})

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setctty: true,
		Setsid:  true,
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty

	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "start: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	tty.Close()

	go func() {
		for win := range winCh {
			_ = pty.Setsize(ptmx, &pty.Winsize{
				Rows: uint16(win.Height),
				Cols: uint16(win.Width),
				X:    uint16(win.WidthPixels),
				Y:    uint16(win.HeightPixels),
			})
		}
	}()

	go func() { _, _ = io.Copy(ptmx, sess) }()
	_, _ = io.Copy(sess, ptmx)

	if err := cmd.Wait(); err != nil {
		_ = sess.Exit(exitCode(err))
		return
	}
	_ = sess.Exit(0)
}

func runWithPipes(sess ssh.Session, cmd *exec.Cmd) {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "stdin pipe: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}
	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()

	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "start: %v\r\n", err)
		_ = sess.Exit(1)
		return
	}

	go func() {
		defer stdinPipe.Close()
		_, _ = io.Copy(stdinPipe, sess)
	}()

	if err := cmd.Wait(); err != nil {
		_ = sess.Exit(exitCode(err))
		return
	}
	_ = sess.Exit(0)
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

func generateHostKey() (gossh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	mk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mk})
	return gossh.ParsePrivateKey(pemData)
}

func acceptEnv(kv string) bool {
	for _, prefix := range []string{"TERM=", "LANG=", "LC_"} {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
