package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/knight42/krelay/pkg/kube"
)

const serverSSHPort = 22

// exitCodeError carries a remote command's exit status so main can exit with
// the same code, letting callers script against `kubectl relay ssh/NODE -- CMD`
// exactly like ssh.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string {
	return fmt.Sprintf("remote command exited with code %d", e.code)
}

// runSSH implements `kubectl relay ssh/NODE [-- COMMAND]`. It connects to the
// built-in SSH server of a krelay-server pod scheduled on the target node
// (privileged + hostPID), which uses nsenter to run sessions in the host
// namespaces. Without a command it opens an interactive shell on the local
// terminal; with a command it runs it and propagates the exit code, like ssh.
//
// The connection normally goes through the shared mux daemon (see sshmux.go);
// with --control-persist=0 or --server-token this process establishes its own
// tunnel and tears it down on exit.
func (o *options) runSSH(ctx context.Context, nodeName, command string) error {
	var conn net.Conn
	if o.controlPersist > 0 && o.serverToken == "" {
		var err error
		conn, err = o.muxDial(ctx, nodeName)
		if err != nil {
			return err
		}
	} else {
		tn, err := o.dialSSHTunnel(ctx, nodeName, true)
		if err != nil {
			return err
		}
		defer tn.Close()

		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		conn, err = tn.tc.DialTCPPort(dialCtx, serverSSHPort)
		cancel()
		if err != nil {
			return fmt.Errorf("dial SSH port on krelay-server: %w", err)
		}
	}

	if command != "" {
		return runSSHExec(conn, command, os.Stdin, os.Stdout, os.Stderr)
	}
	return runSSHShell(ctx, conn)
}

// sshTunnel is an established tunnel to a krelay-server running on a node.
type sshTunnel struct {
	tc *tailcat.Client
	sj *kube.ServerJob // nil when connecting to an existing server via --server-token
}

func (t *sshTunnel) Close() {
	t.tc.Close()
	if t.sj != nil {
		t.sj.Close()
	}
}

// dialSSHTunnel creates a krelay-server Job scheduled on the target node and
// establishes the WireGuard tunnel to it, leaving heartbeat and path-monitor
// goroutines running until ctx is canceled. The caller must Close the result.
func (o *options) dialSSHTunnel(ctx context.Context, nodeName string, quietPathLogs bool) (*sshTunnel, error) {
	// Start loading the DERP map now so the region code is usually ready,
	// at no extra latency, by the time the tunnel logs mention the region.
	regions := startDERPRegionResolver(ctx, http.DefaultClient, o.derpMapURL)

	priv := key.NewNode()
	token := o.serverToken
	var sj *kube.ServerJob
	if token == "" {
		restCfg, err := o.cf.ToRESTConfig()
		if err != nil {
			return nil, err
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, err
		}
		// Fail fast on a bad node name: the Job pins the pod via spec.nodeName,
		// which bypasses scheduling, so it would stay Pending forever.
		if _, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{}); err != nil {
			return nil, fmt.Errorf("node %q: %w", nodeName, err)
		}

		sj, err = startServer(ctx, o, priv, nodeName)
		if err != nil {
			return nil, err
		}
		tokenCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		token, err = sj.ReadToken(tokenCtx)
		cancel()
		if err != nil {
			sj.Close()
			return nil, fmt.Errorf("read connection token from pod logs: %w", err)
		}
		slog.Debug("Got connection token", slog.String("token", token))
	}

	tcLogf := logger.Discard
	if o.verbosity >= 5 {
		tcLogf = logger.WithPrefix(log.Printf, "tailcat: ")
	}
	tc := &tailcat.Client{
		Server: tailcat.Addr(token),
		Key:    priv,
		Logf:   tcLogf,
	}

	slog.Info("Establishing tunnel to krelay-server")
	establishCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := establishTunnel(establishCtx, tc, regions)
	cancel()
	if err != nil {
		tc.Close()
		if sj != nil {
			sj.Close()
		}
		return nil, fmt.Errorf("establish tunnel: %w", err)
	}

	go maintainHeartbeat(ctx, tc)
	go monitorPath(ctx, tc, regions, quietPathLogs)
	return &sshTunnel{tc: tc, sj: sj}, nil
}

// sshClientConfig returns the client config for the krelay-server SSH server.
// Host key verification is deliberately skipped: the transport is already
// end-to-end encrypted and keyed to this client, and the server generates a
// fresh host key on every run and accepts any client, so the SSH layer
// carries no trust of its own.
func sshClientConfig() *gossh.ClientConfig {
	return &gossh.ClientConfig{
		// The username is ignored by the server (nsenter always lands in
		// the host namespaces as root).
		User:            "root",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	}
}

// runSSHExec runs one command on the node over an established SSH transport,
// keeping stdout and stderr separate and reporting the remote exit status as
// an exitCodeError. No PTY is allocated, mirroring `ssh HOST COMMAND`.
func runSSHExec(conn net.Conn, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	defer conn.Close()

	cc, chans, reqs, err := gossh.NewClientConn(conn, "krelay-server", sshClientConfig())
	if err != nil {
		return fmt.Errorf("SSH handshake: %w", err)
	}
	client := gossh.NewClient(cc, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session: %w", err)
	}
	defer sess.Close()

	// Stdin goes through a pipe instead of sess.Stdin: the session's own
	// stdin copier is waited on by sess.Wait, which would then block on a
	// pending stdin read after the remote command exits.
	inPipe, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(inPipe, stdin)
		_ = inPipe.Close()
	}()
	sess.Stdout = stdout
	sess.Stderr = stderr

	err = sess.Run(command)
	var exitErr *gossh.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr):
		return exitCodeError{code: exitErr.ExitStatus()}
	default:
		return fmt.Errorf("run remote command: %w", err)
	}
}

// runSSHShell opens an interactive shell on the node using an in-process SSH
// client over the given connection.
func runSSHShell(ctx context.Context, conn net.Conn) error {
	defer conn.Close()

	cc, chans, reqs, err := gossh.NewClientConn(conn, "krelay-server", sshClientConfig())
	if err != nil {
		return fmt.Errorf("SSH handshake: %w", err)
	}
	client := gossh.NewClient(cc, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session: %w", err)
	}
	defer sess.Close()

	// Stdin goes through a pipe instead of sess.Stdin: the session's own
	// stdin copier is waited on by sess.Wait, which would then block on a
	// pending os.Stdin read after the remote shell exits.
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(stdin, os.Stdin)
		_ = stdin.Close()
	}()
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("set raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState) //nolint:errcheck

		width, height, err := term.GetSize(fd)
		if err != nil {
			width, height = 80, 24
		}
		termType := os.Getenv("TERM")
		if termType == "" {
			termType = "xterm"
		}
		// The server ignores terminal modes (it applies the pty defaults).
		if err := sess.RequestPty(termType, height, width, gossh.TerminalModes{}); err != nil {
			return fmt.Errorf("request pty: %w", err)
		}

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(fd); err == nil {
					_ = sess.WindowChange(h, w)
				}
			}
		}()
	}

	if err := sess.Shell(); err != nil {
		return fmt.Errorf("start remote shell: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		var exitErr *gossh.ExitError
		if err == nil || errors.As(err, &exitErr) {
			// The shell's own exit status is not a krelay failure.
			return nil
		}
		return fmt.Errorf("SSH session: %w", err)
	}
}

// parseSSHTarget extracts the node name from an "ssh/NODE" argument.
func parseSSHTarget(arg string) (nodeName string, isSSH bool) {
	typ, name, ok := strings.Cut(arg, "/")
	if !ok || name == "" {
		return "", false
	}
	if !strings.EqualFold(typ, "ssh") {
		return "", false
	}
	return name, true
}
