package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"bottlerocket brush": {
			files:   map[string]os.FileMode{"bin/brush": 0o755},
			links:   map[string]string{"bin/sh": "brush"},
			wantErr: "host shell unsupported: Bottlerocket's brush",
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
