package main

import (
	"bytes"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestCLIDialects(t *testing.T) {
	for name, tc := range map[string]struct {
		executable string
		argv       []string
		command    string
		args       []string
	}{
		"plugin forwarding":     {"kubectl-relay", []string{"svc/nginx", "8080:80"}, "kubectl", []string{"svc/nginx", "8080:80"}},
		"plugin ssh":            {"kubectl-relay", []string{"ssh/node", "--", "echo", "--help"}, "kubectl", []string{"ssh/node", "echo", "--help"}},
		"standalone forwarding": {"krelay", []string{"port-forward", "svc/nginx", "8080:80"}, "port-forward", []string{"svc/nginx", "8080:80"}},
		"standalone ssh":        {"krelay", []string{"ssh", "node", "--", "echo", "--help"}, "ssh", []string{"node", "echo", "--help"}},
		"standalone socks":      {"krelay", []string{"socks", "1081"}, "socks", []string{"1081"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := newCommand(tc.executable)
			called := false
			capture := func(cmd *cobra.Command, args []string) error {
				called = true
				if cmd.Name() != tc.command || !slices.Equal(args, tc.args) {
					t.Fatalf("got %s %q", cmd.Name(), args)
				}
				return nil
			}
			root.RunE = capture
			for _, child := range root.Commands() {
				child.RunE = capture
			}
			root.SetArgs(tc.argv)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("command was not executed")
			}
		})
	}
}

func TestStandaloneMuxCLI(t *testing.T) {
	for name, argv := range map[string][]string{
		"flags before command": {"--context", "staging", "ssh", "node", "--server.image", "image with spaces"},
		"flags after command":  {"ssh", "node", "--context", "staging", "--server.image", "image with spaces"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := newCommand("krelay")
			var daemonArgs []string
			ssh, _, err := parent.Find([]string{"ssh"})
			if err != nil {
				t.Fatal(err)
			}
			ssh.RunE = func(cmd *cobra.Command, args []string) error {
				daemonArgs = muxDaemonArgs(args[0], cmd.Flags(), true)
				return nil
			}
			parent.SetArgs(argv)
			if err := parent.Execute(); err != nil {
				t.Fatal(err)
			}
			daemon := newCommand("krelay")
			ssh, _, err = daemon.Find([]string{"ssh"})
			if err != nil {
				t.Fatal(err)
			}
			ssh.RunE = func(cmd *cobra.Command, args []string) error {
				if !slices.Equal(args, []string{"node"}) {
					t.Fatalf("args = %q", args)
				}
				for flag, want := range map[string]string{"context": "staging", "server.image": "image with spaces"} {
					got, err := cmd.Flags().GetString(flag)
					if err != nil || got != want {
						t.Fatalf("%s = %q, %v", flag, got, err)
					}
				}
				mux, err := cmd.Flags().GetBool("ssh-mux")
				if err != nil || !mux {
					t.Fatalf("ssh-mux = %v, %v", mux, err)
				}
				return nil
			}
			daemon.SetArgs(daemonArgs)
			if err := daemon.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStandaloneRejectsLegacyTarget(t *testing.T) {
	cmd := newCommand("krelay")
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"ssh/node"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unknown command error")
	}
}
