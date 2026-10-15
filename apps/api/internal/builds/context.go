package builds

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

var (
	ErrInvalidContext  = errors.New("invalid Docker build context")
	ErrContextTooLarge = errors.New("Docker build context exceeds its size or entry limit")
)

const (
	maxContextBytes   = 256 << 20
	maxContextEntries = 10000
	maxIgnoreBytes    = 1 << 20
)

type buildContext struct {
	*os.File
	path   string
	mu     sync.Mutex
	closed bool
}

func (c *buildContext) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var closeErr error
	if !c.closed {
		closeErr = c.File.Close()
		c.closed = true
	}
	if c.path == "" {
		return closeErr
	}
	removeErr := os.Remove(c.path)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		c.path = ""
	}
	return errors.Join(closeErr, removeErr)
}

func prepareContext(ctx context.Context, checkoutPath string) (_ *buildContext, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(checkoutPath) {
		return nil, fmt.Errorf("%w: checkout path must be absolute", ErrInvalidContext)
	}
	root, err := os.OpenRoot(checkoutPath)
	if err != nil {
		return nil, fmt.Errorf("%w: open checkout: %w", ErrInvalidContext, err)
	}
	defer root.Close()
	info, err := root.Lstat("Dockerfile")
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: root Dockerfile must be a regular file", ErrInvalidContext)
	}
	patterns, selectedIgnore, err := contextIgnorePatterns(ctx, root)
	if err != nil {
		return nil, err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ignore pattern: %w", ErrInvalidContext, err)
	}
	file, err := os.CreateTemp("", "shipyard-context-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create context archive: %w", err)
	}
	archive := &buildContext{File: file, path: file.Name()}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, archive.Close())
		}
	}()
	w := tar.NewWriter(&contextLimitWriter{writer: file, remaining: maxContextBytes})
	visited := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		visited++
		if visited > maxContextEntries {
			return ErrContextTooLarge
		}
		if name == "." {
			return nil
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".git" {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		ignored, err := matcher.MatchesOrParentMatches(name)
		if err != nil {
			return err
		}
		if ignored && name != "Dockerfile" && name != selectedIgnore {
			// A later negation can restore descendants of an excluded directory.
			if entry.IsDir() && !matcher.Exclusions() {
				return fs.SkipDir
			}
			return nil
		}
		return appendContextEntry(ctx, root, w, name)
	})
	if err != nil {
		return nil, fmt.Errorf("archive build context: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("finish context archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind context archive: %w", err)
	}
	success = true
	return archive, nil
}

func contextIgnorePatterns(ctx context.Context, root *os.Root) ([]string, string, error) {
	selected := ""
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxIgnoreBytes {
			return nil, "", fmt.Errorf("%w: %s must be a regular file of at most 1 MiB", ErrInvalidContext, name)
		}
		selected = name
	}
	if selected == "" {
		return nil, "", nil
	}
	file, err := root.Open(selected)
	if err != nil {
		return nil, "", fmt.Errorf("read ignore file: %w", err)
	}
	defer file.Close()
	limited := &io.LimitedReader{R: &contextInputReader{ctx: ctx, reader: file}, N: maxIgnoreBytes + 1}
	patterns, err := ignorefile.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("%w: read ignore patterns: %w", ErrInvalidContext, err)
	}
	if limited.N == 0 {
		return nil, "", fmt.Errorf("%w: ignore file exceeds 1 MiB", ErrInvalidContext)
	}
	return patterns, selected, nil
}

func appendContextEntry(ctx context.Context, root *os.Root, w *tar.Writer, name string) (resultErr error) {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if (name == "Dockerfile" || name == ".dockerignore" || name == "Dockerfile.dockerignore") && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: build control file changed while packaging", ErrInvalidContext)
	}
	link := ""
	var file *os.File
	switch {
	case info.Mode().IsRegular():
		file, err = root.Open(name)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			return fmt.Errorf("%w: file changed while packaging", ErrInvalidContext)
		}
		info = opened
	case info.Mode()&os.ModeSymlink != 0:
		link, err = root.Readlink(name)
		if err != nil {
			return err
		}
	case !info.IsDir():
		return fmt.Errorf("%w: included special file", ErrInvalidContext)
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
	if err := w.WriteHeader(header); err != nil {
		return err
	}
	if file != nil {
		_, err = io.Copy(w, &contextInputReader{ctx: ctx, reader: file})
	}
	return err
}

type contextInputReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextInputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *contextLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrContextTooLarge
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
