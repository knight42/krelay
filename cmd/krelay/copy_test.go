package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestCopyArchiveRoundTrip(t *testing.T) {
	for name, directory := range map[string]bool{"file": false, "directory": true} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "source ' with spaces")
			data := []byte{0, 1, 2, 255, 10}
			file := src
			if directory {
				if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(src, "nested", "file")
				if err := os.Chmod(src, 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("nested/file", filepath.Join(src, "link")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(file, data, 0751); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			if err := writeCopyArchive(&archive, src, "source"); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "renamed")
			if err := readCopyArchive(&archive, dst, "source", directory); err != nil {
				t.Fatal(err)
			}
			target := dst
			if directory {
				target = filepath.Join(dst, "nested", "file")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("data %q, %v", got, err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0751 {
				t.Fatalf("mode %v, %v", info, err)
			}
			if directory {
				info, err := os.Stat(dst)
				if err != nil || info.Mode().Perm() != 0750 {
					t.Fatalf("directory mode: %v, %v", info, err)
				}
				link, err := os.Readlink(filepath.Join(dst, "link"))
				if err != nil || link != "nested/file" {
					t.Fatalf("link %q, %v", link, err)
				}
			}
		})
	}
}

func testCopyArchive(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCopyArchiveRejectsEscapes(t *testing.T) {
	for name, h := range map[string]*tar.Header{
		"parent":            {Name: "source/../escape", Typeflag: tar.TypeReg},
		"absolute":          {Name: "/escape", Typeflag: tar.TypeReg},
		"wrong root":        {Name: "other/file", Typeflag: tar.TypeReg},
		"hardlink escape":   {Name: "source/link", Typeflag: tar.TypeLink, Linkname: "../escape"},
		"symlink traversal": {Name: "source/link/file", Typeflag: tar.TypeReg},
		"device":            {Name: "source/device", Typeflag: tar.TypeChar},
	} {
		t.Run(name, func(t *testing.T) {
			data := testCopyArchive(t, &tar.Header{Name: "source", Typeflag: tar.TypeDir, Mode: 0755}, &tar.Header{Name: "source/link", Typeflag: tar.TypeSymlink, Linkname: ".."}, h)
			if err := readCopyArchive(bytes.NewReader(data), filepath.Join(t.TempDir(), "dst"), "source", true); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestCopyArchiveExistingDirectoryAndRecursive(t *testing.T) {
	data := testCopyArchive(t, &tar.Header{Name: "source", Typeflag: tar.TypeDir, Mode: 0755}, &tar.Header{Name: "source/file", Typeflag: tar.TypeReg, Mode: 0644})
	dir := t.TempDir()
	if err := readCopyArchive(bytes.NewReader(data), dir, "source", false); err == nil {
		t.Fatal("directory accepted without -r")
	}
	if err := readCopyArchive(bytes.NewReader(data), dir, "source", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "source", "file")); err != nil {
		t.Fatal(err)
	}
}

func TestCopyStreamFailure(t *testing.T) {
	sentinel := errors.New("transport failed")
	runner := func(_ context.Context, _ string, _ io.Reader, _ io.Writer) error { return sentinel }
	err := downloadCopy(t.Context(), "/tmp/source", filepath.Join(t.TempDir(), "dst"), false, runner)
	if !errors.Is(err, sentinel) {
		t.Fatalf("download error: %v", err)
	}
	// The consumer stops without reading: the producer must unblock and finish.
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 65536), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner = func(_ context.Context, _ string, _ io.Reader, out io.Writer) error {
		calls++
		if calls == 1 {
			_, err := io.WriteString(out, "f")
			return err
		}
		return sentinel
	}
	if err := uploadCopy(t.Context(), src, "/tmp/dst", false, runner); !errors.Is(err, sentinel) {
		t.Fatalf("upload error: %v", err)
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
