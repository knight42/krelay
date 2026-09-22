// SSH mux: `kubectl relay ssh/NODE` invocations share one krelay-server pod
// and one WireGuard tunnel through a per-node background daemon, in the
// spirit of OpenSSH's ControlMaster/ControlPersist. The first invocation
// spawns the daemon, which creates the server Job, establishes the tunnel and
// listens on a unix socket; subsequent invocations connect to the socket and
// are proxied straight to the server's SSH port, so a command costs only an
// SSH handshake over the already-established tunnel. The daemon exits —
// deleting the Job — after --control-persist without sessions.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"

	"github.com/knight42/krelay/pkg/activity"
)

// muxPaths are the per-(cluster, namespace, node) files of one mux daemon.
// The lock file designates the daemon owning the socket: it is held for the
// daemon's lifetime, so a bindable-but-dead socket file is always an orphan.
type muxPaths struct {
	sock string
	lock string
	log  string
}

func (o *options) muxPaths(nodeName string) (muxPaths, error) {
	// Key the daemon on the resolved apiserver URL, not on the raw
	// kubeconfig flags: identical flags can reach different clusters (e.g.
	// after `kubectl config use-context`), and different flags can name the
	// same cluster, which should share a daemon.
	restCfg, err := o.cf.ToRESTConfig()
	if err != nil {
		return muxPaths{}, err
	}
	stateDir, err := stateDir()
	if err != nil {
		return muxPaths{}, err
	}
	base := filepath.Join(stateDir, "ssh-"+muxID(restCfg.Host, o.serverNamespace, nodeName))
	return muxPaths{sock: base + ".sock", lock: base + ".lock", log: base + ".log"}, nil
}

// stateDir returns the krelay state directory following the XDG base
// directory spec: $XDG_STATE_HOME/krelay, ~/.local/state/krelay by default.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "krelay"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "krelay"), nil
}

// muxID derives a fixed-length identifier so the socket path stays within the
// unix socket path limit regardless of node and context name lengths.
func muxID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// muxDial returns a connection to the SSH port of the node's krelay-server
// through the mux daemon, spawning the daemon when none is running.
func (o *options) muxDial(ctx context.Context, nodeName string) (net.Conn, error) {
	paths, err := o.muxPaths(nodeName)
	if err != nil {
		return nil, err
	}
	if conn, err := net.Dial("unix", paths.sock); err == nil {
		return conn, nil
	}

	// Bounded attempts: a daemon that reports BUSY lost the flock race, and
	// the winner it lost to may be one that is shutting down and never serves
	// the socket, in which case waitForMuxSocket asks for a respawn.
	var lastErr error
	for range 3 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slog.Info("Starting SSH mux daemon", slog.String("node", nodeName), slog.String("log", paths.log))
		status, msg, err := o.spawnMuxDaemon(ctx, nodeName, paths)
		if err != nil {
			return nil, err
		}
		switch status {
		case muxReady, muxBusy:
			conn, err := waitForMuxSocket(ctx, paths, 3*time.Minute)
			if err == nil {
				return conn, nil
			}
			if !errors.Is(err, errMuxDaemonGone) {
				return nil, fmt.Errorf("%w (log: %s)", err, paths.log)
			}
			lastErr = err
		case muxError:
			return nil, fmt.Errorf("mux daemon: %s (log: %s)", msg, paths.log)
		}
	}
	return nil, fmt.Errorf("%w (log: %s)", lastErr, paths.log)
}

var errMuxDaemonGone = errors.New("mux daemon exited before serving its socket")

// waitForMuxSocket polls the daemon socket until it accepts a connection. It
// returns errMuxDaemonGone as soon as no daemon holds the lock anymore, so
// the caller can respawn instead of waiting out the timeout.
func waitForMuxSocket(ctx context.Context, paths muxPaths, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		if conn, err := net.Dial("unix", paths.sock); err == nil {
			return conn, nil
		}
		if free, err := muxLockFree(paths.lock); err == nil && free {
			return nil, errMuxDaemonGone
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for mux daemon socket %s", paths.sock)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// muxLockFree reports whether no daemon currently holds the mux lock.
func muxLockFree(lock string) (bool, error) {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil
	}
	return true, nil
}

type muxStatus int

const (
	muxReady muxStatus = iota
	muxBusy
	muxError
)

// parseMuxStatus decodes the one-line startup report a daemon writes to the
// pipe its parent passed in.
func parseMuxStatus(line string) (muxStatus, string, error) {
	status, msg, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch status {
	case "READY":
		return muxReady, "", nil
	case "BUSY":
		return muxBusy, "", nil
	case "ERROR":
		return muxError, msg, nil
	}
	return 0, "", fmt.Errorf("unexpected mux daemon status %q", line)
}

// spawnMuxDaemon starts this binary as a detached mux daemon for nodeName and
// waits for it to report its startup outcome over an inherited pipe.
func (o *options) spawnMuxDaemon(ctx context.Context, nodeName string, paths muxPaths) (muxStatus, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(paths.log), 0o700); err != nil {
		return 0, "", err
	}
	logFile, err := os.OpenFile(paths.log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer logFile.Close()

	args := []string{
		"--ssh-mux", "ssh/" + nodeName,
		"--server.namespace=" + o.serverNamespace,
		"--server.image=" + o.serverImage,
		"--server.pull-policy=" + o.serverPullPolicy,
		"--derp-map-url=" + o.derpMapURL,
		"--control-persist=" + o.controlPersist.String(),
		"-v=" + strconv.Itoa(o.verbosity),
	}
	if v := strDeref(o.cf.KubeConfig); v != "" {
		args = append(args, "--kubeconfig="+v)
	}
	if v := strDeref(o.cf.Context); v != "" {
		args = append(args, "--context="+v)
	}
	if v := strDeref(o.cf.ClusterName); v != "" {
		args = append(args, "--cluster="+v)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return 0, "", err
	}
	defer r.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{w} // fd 3: the startup status pipe
	// New session: the daemon survives this process and its terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return 0, "", fmt.Errorf("start mux daemon: %w", err)
	}
	w.Close()
	_ = cmd.Process.Release()

	// Pod scheduling and image pulls dominate daemon startup time.
	_ = r.SetReadDeadline(time.Now().Add(3 * time.Minute))
	stop := context.AfterFunc(ctx, func() { _ = r.SetReadDeadline(time.Unix(0, 0)) })
	defer stop()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return 0, "", fmt.Errorf("mux daemon exited without reporting status: %w (log: %s)", err, paths.log)
	}
	status, msg, err := parseMuxStatus(line)
	if err != nil {
		return 0, "", fmt.Errorf("%w (log: %s)", err, paths.log)
	}
	return status, msg, nil
}

// runSSHMux is the daemon side: it owns the krelay-server Job and tunnel for
// one node and proxies unix socket connections to the server's SSH port.
func (o *options) runSSHMux(ctx context.Context, nodeName string) error {
	report := muxStatusReporter()
	fail := func(err error) error {
		report("ERROR " + strings.ReplaceAll(err.Error(), "\n", " "))
		return err
	}

	paths, err := o.muxPaths(nodeName)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.lock), 0o700); err != nil {
		return fail(err)
	}
	lockFile, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Another daemon owns this node; it will serve the socket.
		report("BUSY")
		return nil
	}

	tn, err := o.dialSSHTunnel(ctx, nodeName, false)
	if err != nil {
		return fail(err)
	}
	defer tn.Close()

	// The flock above guarantees any existing socket file is an orphan.
	_ = os.Remove(paths.sock)
	ln, err := net.Listen("unix", paths.sock)
	if err != nil {
		return fail(fmt.Errorf("listen on mux socket: %w", err))
	}
	defer ln.Close()
	defer func() { _ = os.Remove(paths.sock) }()
	_ = os.Chmod(paths.sock, 0o600)

	report("READY")
	slog.Info("SSH mux daemon ready",
		slog.String("node", nodeName),
		slog.String("socket", paths.sock),
		slog.Duration("controlPersist", o.controlPersist),
	)

	tracker := activity.NewTracker()
	shutdown := make(chan string, 1)
	// Consecutive failures to reach the server pod (e.g. it was evicted)
	// shut the daemon down instead of serving dead sessions; the next client
	// invocation then starts fresh.
	var dialFailures atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tracker.ConnStarted()
				defer tracker.ConnEnded()
				defer conn.Close()

				dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				remote, err := tn.tc.DialTCPPort(dialCtx, serverSSHPort)
				cancel()
				if err != nil {
					slog.Error("Dial SSH port on krelay-server failed", slog.Any("error", err))
					if dialFailures.Add(1) >= 2 {
						select {
						case shutdown <- "krelay-server is unreachable":
						default:
						}
					}
					return
				}
				dialFailures.Store(0)
				tailcat.ProxyConns(conn, remote)
			}()
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("Received signal, exiting")
			return nil
		case reason := <-shutdown:
			slog.Info("Shutting down", slog.String("reason", reason))
			return nil
		case <-ticker.C:
			if idle, ok := tracker.IdleFor(); ok && idle > o.controlPersist {
				slog.Info("No sessions, exiting", slog.Duration("idle", idle.Round(time.Second)))
				return nil
			}
		}
	}
}

// muxStatusReporter writes one status line to the pipe the parent passed as
// fd 3, falling back to stderr when run by hand.
func muxStatusReporter() func(string) {
	pipe := os.NewFile(3, "mux-status")
	var once sync.Once
	return func(status string) {
		once.Do(func() {
			if pipe != nil {
				_, err := fmt.Fprintln(pipe, status)
				pipe.Close()
				if err == nil {
					return
				}
			}
			fmt.Fprintln(os.Stderr, "mux status:", status)
		})
	}
}
