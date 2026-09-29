package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
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

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type copyExec func(context.Context, string, io.Reader, io.Writer) error

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
	run := func(ctx context.Context, command string, stdin io.Reader, stdout io.Writer) error {
		return o.withSSHConn(ctx, node, func(conn net.Conn) error {
			return runSSHExec(ctx, conn, command, stdin, stdout, os.Stderr)
		})
	}
	if src.node == "" {
		return uploadCopy(ctx, src.name, dst.name, recursive, run)
	}
	return downloadCopy(ctx, src.name, dst.name, recursive, run)
}

func uploadCopy(ctx context.Context, src, dst string, recursive bool, run copyExec) error {
	src = filepath.Clean(src)
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() && !recursive {
		return errors.New("source is a directory; use -r")
	}
	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("unsupported source type: %s", src)
	}
	var kind bytes.Buffer
	if err := run(ctx, "if [ -d "+shellQuote(dst)+" ]; then printf d; else printf f; fi", strings.NewReader(""), &kind); err != nil {
		return err
	}
	if kind.String() != "d" && kind.String() != "f" {
		return errors.New("unexpected destination probe response")
	}
	if kind.String() == "f" && strings.HasSuffix(dst, "/") {
		return fmt.Errorf("destination directory does not exist: %s", dst)
	}
	if kind.String() == "d" {
		dst = path.Join(dst, filepath.Base(src))
	}
	dst = path.Clean(dst)
	// Prefix the archive member with './' so even a leading '-' is a file name.
	command := "tar -C " + shellQuote(path.Dir(dst)) + " -xf -"
	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := writeCopyArchive(writer, src, path.Base(dst))
		_ = writer.CloseWithError(err)
		produced <- err
	}()
	remoteErr := run(ctx, command, reader, io.Discard)
	_ = reader.CloseWithError(remoteErr)
	localErr := <-produced
	if localErr != nil && !errors.Is(localErr, io.ErrClosedPipe) {
		return fmt.Errorf("read source: %w", localErr)
	}
	if remoteErr != nil {
		return remoteErr
	}
	return localErr
}

func downloadCopy(ctx context.Context, src, dst string, recursive bool, run copyExec) error {
	src = path.Clean(src)
	base := path.Base(src)
	dir := path.Dir(src)
	if src == "/" {
		base, dir = ".", "/"
	}
	command := "tar -C " + shellQuote(dir) + " -cf - " + shellQuote("./"+base)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	finished := make(chan error, 1)
	go func() {
		err := run(ctx, command, strings.NewReader(""), writer)
		_ = writer.CloseWithError(err)
		finished <- err
	}()
	localErr := readCopyArchive(reader, dst, base, recursive)
	if localErr == nil {
		// tar ends before SSH EOF. Drain padding so the SSH stdout copier can finish.
		_, localErr = io.Copy(io.Discard, reader)
	}
	_ = reader.Close()
	if localErr != nil {
		cancel()
	}
	remoteErr := <-finished
	if localErr != nil {
		return fmt.Errorf("extract copy: %w", localErr)
	}
	return remoteErr
}

// writeCopyArchive does not follow symlinks while walking directories. Ownership
// is deliberately omitted: uploaded files belong to the remote SSH user (root).
func writeCopyArchive(w io.Writer, src, name string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(src, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(file)
			if err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", file)
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, file)
		if err != nil {
			return err
		}
		header.Name = "./" + path.Join(name, filepath.ToSlash(rel))
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
		header.Mode &= 0777
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		closeErr := f.Close()
		return errors.Join(err, closeErr)
	})
	return errors.Join(err, tw.Close())
}

// archiveCopyName confines every member (including hardlink targets) to the
// requested source, then maps that source to the user's destination basename.
func archiveCopyName(name, source, destination string) (string, error) {
	if slices.Contains(strings.Split(name, "/"), "..") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("absolute archive path %q", name)
	}
	name = path.Clean(name)
	source = path.Clean(source)
	var rel string
	if source == "." {
		rel = name
	} else if name == source {
		rel = "."
	} else {
		var ok bool
		rel, ok = strings.CutPrefix(name, source+"/")
		if !ok {
			return "", fmt.Errorf("unexpected archive member %q", name)
		}
	}
	return filepath.Join(destination, filepath.FromSlash(rel)), nil
}

func readCopyArchive(r io.Reader, dst, source string, recursive bool) error {
	tr := tar.NewReader(r)
	first, err := tr.Next()
	if err != nil {
		return fmt.Errorf("read archive header: %w", err)
	}
	if path.Clean(first.Name) != path.Clean(source) {
		return fmt.Errorf("unexpected archive root %q", first.Name)
	}
	if first.Typeflag == tar.TypeDir && !recursive {
		return errors.New("source is a directory; use -r")
	}
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		dst = filepath.Join(dst, source)
	} else if err != nil && (!errors.Is(err, os.ErrNotExist) || strings.HasSuffix(dst, string(os.PathSeparator))) {
		return err
	}
	dst = filepath.Clean(dst)
	root, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// os.Root enforces confinement even through pre-existing or archived symlinks.
	// Use a separate root for directories so links cannot reach destination siblings.
	if first.Typeflag == tar.TypeDir {
		if err := root.MkdirAll(filepath.Base(dst), 0755); err != nil {
			return err
		}
		dirRoot, err := root.OpenRoot(filepath.Base(dst))
		if err != nil {
			return err
		}
		defer func() { _ = dirRoot.Close() }()
		return extractCopyArchive(tr, first, dirRoot, source, ".")
	}
	return extractCopyArchive(tr, first, root, source, filepath.Base(dst))
}

func extractCopyArchive(tr *tar.Reader, header *tar.Header, root *os.Root, source, destination string) error {
	directory := header.Typeflag == tar.TypeDir
	// Keep directories writable until their children have been extracted.
	type directoryMode struct {
		name string
		mode fs.FileMode
	}
	var directories []directoryMode
	for {
		name, err := archiveCopyName(header.Name, source, destination)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0755); err != nil {
				return err
			}
			directories = append(directories, directoryMode{name, fs.FileMode(header.Mode) & 0777})
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(header.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := removeCopyLink(root, name); err != nil {
				return err
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := archiveCopyName(header.Linkname, source, destination)
			if err != nil {
				return err
			}
			if err := removeCopyLink(root, name); err != nil {
				return err
			}
			if err := root.Link(target, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d: %s", header.Typeflag, header.Name)
		}
		header, err = tr.Next()
		if errors.Is(err, io.EOF) {
			for _, dir := range slices.Backward(directories) {
				f, err := root.Open(dir.name)
				if err != nil {
					return err
				}
				chmodErr := f.Chmod(dir.mode)
				if err := errors.Join(chmodErr, f.Close()); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !directory {
			return errors.New("unexpected extra entry for a single-file copy")
		}
	}
}

// Replace existing files and links without removing destination directories.
func removeCopyLink(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("cannot replace directory %s with a link", name)
	}
	return root.Remove(name)
}
