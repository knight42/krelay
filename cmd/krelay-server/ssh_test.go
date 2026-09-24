package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostShell(t *testing.T) {
	for name, tt := range map[string]struct {
		files   map[string]os.FileMode
		links   map[string]string
		want    string
		wantErr string
	}{
		"prefer bash": {
			files: map[string]os.FileMode{"bin/bash": 0o755, "bin/sh": 0o755},
			want:  "/bin/bash",
		},
		"usr bash": {
			files: map[string]os.FileMode{"usr/bin/bash": 0o755},
			want:  "/usr/bin/bash",
		},
		"busybox sh": {
			files: map[string]os.FileMode{"bin/busybox": 0o755},
			links: map[string]string{"bin/sh": "busybox"},
			want:  "/bin/sh",
		},
		"missing shell": {wantErr: "host shell unsupported: no executable bash or sh"},
		"non executable shell": {
			files:   map[string]os.FileMode{"bin/bash": 0o644},
			wantErr: "host shell unsupported: no executable bash or sh",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for path, mode := range tt.files {
				path = filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, mode); err != nil {
					t.Fatal(err)
				}
			}
			for path, target := range tt.links {
				if err := os.Symlink(target, filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := hostShell(root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("hostShell() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("hostShell() = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

func TestCheckShell(t *testing.T) {
	for name, tt := range map[string]struct {
		script  string
		wantErr string
	}{
		"POSIX shell": {script: `exec /bin/sh "$@"`},
		"rejects commands": {
			script:  `printf 'commands not allowed' >&2; exit 127`,
			wantErr: "commands not allowed",
		},
		"ignores commands with success": {
			script:  `exit 0`,
			wantErr: "host shell unsupported",
		},
		"hangs": {
			script:  `while :; do :; done`,
			wantErr: "deadline exceeded",
		},
	} {
		t.Run(name, func(t *testing.T) {
			shell := filepath.Join(t.TempDir(), "shell")
			if err := os.WriteFile(shell, []byte("#!/bin/sh\n"+tt.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := checkShell(ctx, exec.Command(shell, "-c", shellProbe))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkShell() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
