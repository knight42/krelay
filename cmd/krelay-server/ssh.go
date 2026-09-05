package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"

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
	cmd := nsenterCommand(sess.RawCommand())

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
func nsenterCommand(rawCmd string) *exec.Cmd {
	shell := hostShell()
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

// hostShell returns the path to bash on the host if it exists (visible
// under /proc/1/root since we have hostPID), falling back to sh.
func hostShell() string {
	for _, p := range []string{"/proc/1/root/bin/bash", "/proc/1/root/usr/bin/bash"} {
		if _, err := os.Stat(p); err == nil {
			return "bash"
		}
	}
	return "sh"
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
