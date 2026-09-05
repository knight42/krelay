package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/knight42/krelay/pkg/kube"
)

// runSSH implements `kubectl relay ssh/NODE`. It starts a krelay-server,
// establishes the WireGuard tunnel, opens a local TCP listener on a random
// port that forwards to nodeIP:22 through the tunnel, and execs the system
// ssh client pointing at that local port.
func (o *options) runSSH(ctx context.Context, target, nodeIP string, sshPort uint16, sshArgs []string) error {
	priv := key.NewNode()
	token := o.serverToken
	var sj *kube.ServerJob
	if token == "" {
		var err error
		sj, err = startServer(ctx, o, priv)
		if err != nil {
			return err
		}
		defer sj.Close()

		tokenCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		token, err = sj.ReadToken(tokenCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read connection token from pod logs: %w", err)
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
	defer tc.Close()

	slog.Info("Establishing tunnel to krelay-server")
	establishCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := establishTunnel(establishCtx, tc)
	cancel()
	if err != nil {
		return fmt.Errorf("establish tunnel: %w", err)
	}

	go maintainHeartbeat(ctx, tc)
	go monitorPath(ctx, tc)

	// Listen on a random local port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for SSH proxy: %w", err)
	}
	defer ln.Close()
	localPort := ln.Addr().(*net.TCPAddr).Port
	dest := net.JoinHostPort(nodeIP, strconv.Itoa(int(sshPort)))

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("SSH proxy accept failed", slog.Any("error", err))
				}
				return
			}
			go handleSSHProxy(ctx, tc, conn, dest)
		}
	}()

	sshExe, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("ssh client not found in $PATH")
	}

	argv := []string{
		sshExe,
		"-o", "StrictHostKeyChecking no",
		"-o", "UserKnownHostsFile " + os.DevNull,
		"-o", fmt.Sprintf("HostKeyAlias %s", target),
		"-p", strconv.Itoa(localPort),
	}
	argv = append(argv, sshArgs...)
	argv = append(argv, "--", "127.0.0.1")

	slog.Info("Starting SSH session", slog.String("target", target), slog.String("via", dest))

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("ssh: %w", err)
	}
	return nil
}

func handleSSHProxy(ctx context.Context, tc *tailcat.Client, conn net.Conn, dest string) {
	defer conn.Close()
	remote, err := dialTunnel(ctx, tc, dest)
	if err != nil {
		slog.Error("SSH tunnel dial failed", slog.String("dest", dest), slog.Any("error", err))
		return
	}
	tailcat.ProxyConns(conn, remote)
}

// parseSSHTarget extracts the optional user and the rest from an
// "[user@]ssh/NODE" argument.
func parseSSHTarget(arg string) (user, resource string, isSSH bool) {
	u, rest, hasUser := strings.Cut(arg, "@")
	if !hasUser {
		rest = u
		u = ""
	}
	typ, _, ok := strings.Cut(rest, "/")
	if !ok {
		return u, rest, false
	}
	return u, rest, strings.EqualFold(typ, "ssh")
}

func portForSSH(portArgs []string) (uint16, error) {
	if len(portArgs) == 0 {
		return 22, nil
	}
	if len(portArgs) > 1 {
		return 0, errors.New("ssh target accepts at most one port argument")
	}
	s := portArgs[0]
	s = strings.TrimSuffix(s, "@tcp")
	if _, remote, ok := strings.Cut(s, ":"); ok {
		s = remote
	}
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil || p == 0 {
		return 0, fmt.Errorf("invalid SSH port %q", portArgs[0])
	}
	return uint16(p), nil
}
