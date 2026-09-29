package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCopyPaths(t *testing.T) {
	for name, tc := range map[string]struct {
		input, node, file string
		invalid           bool
	}{
		"remote":       {input: "node:/tmp/a:b", node: "node", file: "/tmp/a:b"},
		"local colon":  {input: "./a:b", file: "./a:b"},
		"absolute":     {input: "/tmp/a:b", file: "/tmp/a:b"},
		"missing node": {input: ":file", invalid: true},
		"missing path": {input: "node:", invalid: true},
		"empty":        {invalid: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := parseCopyPath(tc.input)
			if (err != nil) != tc.invalid || p.node != tc.node || p.name != tc.file {
				t.Fatalf("got %+v, %v", p, err)
			}
		})
	}
}

func TestCopyMuxAndFlagScope(t *testing.T) {
	for mode, argv := range map[string][]string{
		"cp":           {"cp", "--control-persist=1s", "--derp-map-url=https://example.test/map", "-r", "./source", "node:/tmp"},
		"ssh":          {"ssh", "--control-persist=1s", "--derp-map-url=https://example.test/map", "node"},
		"port-forward": {"port-forward", "--file=targets.txt"},
	} {
		t.Run(mode, func(t *testing.T) {
			root := newCommand("krelay")
			child, _, err := root.Find([]string{mode})
			if err != nil {
				t.Fatal(err)
			}
			child.RunE = func(cmd *cobra.Command, _ []string) error {
				if mode == "cp" {
					args := muxDaemonArgs("node", cmd.Flags(), true)
					for _, arg := range args {
						if strings.Contains(arg, "recursive") {
							t.Fatalf("copy flag leaked to daemon: %q", args)
						}
					}
				}
				return nil
			}
			root.SetArgs(argv)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, mode := range []string{"cp", "port-forward", "socks", "ssh"} {
		cmd := newCommand("krelay")
		child, _, err := cmd.Find([]string{mode})
		if err != nil {
			t.Fatal(err)
		}
		for flag, owner := range map[string]string{"file": "port-forward", "control-persist": "ssh", "derp-map-url": "ssh"} {
			if got := child.Flags().Lookup(flag) != nil; got != (mode == owner || (owner == "ssh" && mode == "cp")) {
				t.Fatalf("%s flag %s ownership incorrect", mode, flag)
			}
			if cmd.PersistentFlags().Lookup(flag) != nil {
				t.Fatalf("%s is still global", flag)
			}
		}
	}
}
