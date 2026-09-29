package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

type copyPath struct{ node, name string }

func parseCopyPath(s string) (copyPath, error) {
	if s == "" || strings.ContainsRune(s, 0) {
		return copyPath{}, errors.New("copy path must be non-empty and contain no NUL bytes")
	}
	// An explicit relative or absolute path disambiguates local names containing ':'.
	colon := strings.IndexByte(s, ':')
	slash := strings.IndexByte(s, '/')
	if colon < 0 || (slash >= 0 && slash < colon) {
		return copyPath{name: s}, nil
	}
	if colon == 0 || colon == len(s)-1 {
		return copyPath{}, errors.New("remote path must be NODE:PATH")
	}
	return copyPath{node: s[:colon], name: s[colon+1:]}, nil
}

func (o *options) runCopy(ctx context.Context, source, destination string, recursive bool) error {
	src, err := parseCopyPath(source)
	if err != nil {
		return err
	}
	dst, err := parseCopyPath(destination)
	if err != nil {
		return err
	}
	if (src.node == "") == (dst.node == "") {
		return errors.New("exactly one copy path must be NODE:PATH")
	}
	if o.targetsFile != "" || o.sshMux {
		return errors.New("cp cannot be combined with --file or --ssh-mux")
	}
	node := src.node
	if node == "" {
		node = dst.node
	}
	return o.withSSHConn(ctx, node, func(conn net.Conn) error {
		return withSFTP(ctx, conn, func(client *sftp.Client) error {
			if src.node == "" {
				return transferCopy(ctx, src.name, dst.name, recursive, true, client)
			}
			return transferCopy(ctx, dst.name, src.name, recursive, false, client)
		})
	})
}

// withSFTP uses the existing authenticated tunnel and closes it on cancellation,
// including while the SSH handshake or SFTP request is blocked.
func withSFTP(ctx context.Context, conn net.Conn, run func(*sftp.Client) error) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	cc, channels, requests, err := gossh.NewClientConn(conn, "krelay-server", sshClientConfig())
	if err != nil {
		return fmt.Errorf("SSH handshake: %w", err)
	}
	sshClient := gossh.NewClient(cc, channels, requests)
	defer sshClient.Close()
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("open SFTP subsystem (requires an SFTP-capable krelay-server image; restart an existing SSH mux to use it): %w", err)
	}
	defer func() { _ = client.Close() }()
	if err := run(client); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// copyFS exposes only the operations cp needs. Local operations use os.Root;
// remote operations use SFTP, never shell commands or archive extraction.
type copyFS struct {
	lstat    func(string) (os.FileInfo, error)
	readDir  func(string) ([]os.FileInfo, error)
	open     func(string) (io.ReadCloser, error)
	create   func(string) (io.WriteCloser, error)
	mkdir    func(string) error
	chmod    func(string, fs.FileMode) error
	readlink func(string) (string, error)
	symlink  func(string, string) error
	remove   func(string) error
}

func localCopyFS(root *os.Root) copyFS {
	return copyFS{
		lstat: root.Lstat,
		readDir: func(name string) ([]os.FileInfo, error) {
			f, err := root.Open(name)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return f.Readdir(-1)
		},
		open: func(name string) (io.ReadCloser, error) { return root.Open(name) },
		create: func(name string) (io.WriteCloser, error) {
			return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		},
		mkdir: func(name string) error { return root.Mkdir(name, 0700) },
		chmod: root.Chmod, readlink: root.Readlink, symlink: root.Symlink, remove: root.Remove,
	}
}

func remoteCopyFS(client *sftp.Client) copyFS {
	return copyFS{
		lstat: client.Lstat, readDir: client.ReadDir,
		open: func(name string) (io.ReadCloser, error) { return client.Open(name) },
		create: func(name string) (io.WriteCloser, error) {
			return client.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		},
		mkdir: client.Mkdir, chmod: client.Chmod, readlink: client.ReadLink, symlink: client.Symlink, remove: client.Remove,
	}
}

func transferCopy(ctx context.Context, local, remote string, recursive, upload bool, client *sftp.Client) error {
	var info os.FileInfo
	var err error
	if upload {
		info, err = os.Lstat(local)
	} else {
		info, err = client.Lstat(remote)
	}
	if err != nil {
		return err
	}
	if info.IsDir() && !recursive {
		return errors.New("source is a directory; use -r")
	}
	if upload {
		dest, err := client.Stat(remote)
		if err == nil && dest.IsDir() {
			remote = path.Join(remote, filepath.Base(filepath.Clean(local)))
		} else if err != nil && (!os.IsNotExist(err) || strings.HasSuffix(remote, "/")) {
			return err
		}
	} else {
		dest, err := os.Stat(local)
		if err == nil && dest.IsDir() {
			local = filepath.Join(local, path.Base(path.Clean(remote)))
		} else if err != nil && (!os.IsNotExist(err) || strings.HasSuffix(local, string(os.PathSeparator))) {
			return err
		}
	}
	local = filepath.Clean(local)
	root, err := os.OpenRoot(filepath.Dir(local))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	localName := filepath.Base(local)
	// For recursive downloads, confine all writes to the selected directory,
	// including when the destination already contains symlinks.
	if !upload && info.IsDir() {
		if err := root.Mkdir(localName, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		dir, err := root.OpenRoot(localName)
		if err != nil {
			return err
		}
		defer func() { _ = dir.Close() }()
		return copyEntry(ctx, remoteCopyFS(client), localCopyFS(dir), remote, ".")
	}
	if upload {
		return copyEntry(ctx, localCopyFS(root), remoteCopyFS(client), localName, remote)
	}
	return copyEntry(ctx, remoteCopyFS(client), localCopyFS(root), remote, localName)
}

func copyEntry(ctx context.Context, srcFS, dstFS copyFS, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := srcFS.lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		existing, err := dstFS.lstat(dst)
		if os.IsNotExist(err) {
			err = dstFS.mkdir(dst)
		} else if err == nil && !existing.IsDir() {
			return fmt.Errorf("destination is not a directory: %s", dst)
		}
		if err != nil {
			return err
		}
		entries, err := srcFS.readDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			// The peer controls directory listings; accept only single path components.
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
				return fmt.Errorf("invalid directory entry %q", name)
			}
			if err := copyEntry(ctx, srcFS, dstFS, path.Join(src, name), path.Join(dst, name)); err != nil {
				return err
			}
		}
		return dstFS.chmod(dst, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("unsupported source type: %s", src)
	}
	// Open/read the source before replacing an existing destination.
	var input io.ReadCloser
	var target string
	if info.Mode()&os.ModeSymlink != 0 {
		target, err = srcFS.readlink(src)
	} else {
		input, err = srcFS.open(src)
	}
	if err != nil {
		return err
	}
	if input != nil {
		defer input.Close()
	}
	// Unlink instead of following destination symlinks or truncating hardlinks.
	// Exclusive creation also prevents a concurrent symlink replacement.
	if existing, err := dstFS.lstat(dst); err == nil {
		if existing.IsDir() {
			return fmt.Errorf("cannot replace directory: %s", dst)
		}
		if err := dstFS.remove(dst); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return dstFS.symlink(target, dst)
	}
	output, err := dstFS.create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if err := errors.Join(copyErr, output.Close()); err != nil {
		return err
	}
	return dstFS.chmod(dst, info.Mode().Perm())
}
