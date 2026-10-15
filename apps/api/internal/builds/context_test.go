package builds

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareContextIgnoreRulesAndPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name, rootRules, specificRules string
		specific, rootFile, otherFile  bool
	}{
		{"root rules with negations", "ignored/\n!ignored/keep.txt\nroot-only.txt\nDockerfile\n!.git/config\n!nested/.git/config\n", "", false, false, true},
		{"specific replaces root rules", "root-only.txt\n", "specific-only.txt\n", true, true, false},
		{"empty specific replaces root rules", "root-only.txt\n", "", true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newContextTestRoot(t)
			for _, name := range []string{".git/config", "nested/.git/config", "ignored/keep.txt", "ignored/drop.txt", "root-only.txt", "specific-only.txt"} {
				writeContextTestFile(t, root, name, "file:"+name)
			}
			writeContextTestFile(t, root, ".dockerignore", tt.rootRules)
			if tt.specific {
				writeContextTestFile(t, root, "Dockerfile.dockerignore", tt.specificRules)
			}
			archive, err := prepareContext(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = archive.Close() })
			entries := readContextTestArchive(t, archive)
			for name := range entries {
				for _, component := range strings.Split(name, "/") {
					if component == ".git" {
						t.Errorf("Git metadata included: %s", name)
					}
				}
			}
			for name, want := range map[string]bool{"Dockerfile": true, "ignored/keep.txt": true, "root-only.txt": tt.rootFile, "specific-only.txt": tt.otherFile} {
				if _, got := entries[name]; got != want {
					t.Errorf("archive includes %s = %t, want %t", name, got, want)
				}
			}
			if !tt.specific {
				if _, exists := entries["ignored/drop.txt"]; exists {
					t.Error("excluded sibling was restored by a child negation")
				}
			}
		})
	}
}

func TestPrepareContextPreservesLinksWithoutReadingHostFiles(t *testing.T) {
	root, outside := newContextTestRoot(t), t.TempDir()
	const secret = "host-content-must-not-be-uploaded"
	writeContextTestFile(t, outside, "private.txt", secret)
	writeContextTestFile(t, root, "regular.txt", "included")
	links := map[string]string{"file-link": filepath.Join(outside, "private.txt"), "directory-link": outside, "dangling-link": "missing", "internal-link": "regular.txt"}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	archive, err := prepareContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	entries := readContextTestArchive(t, archive)
	for name, target := range links {
		entry := entries[name]
		if entry.header == nil || entry.header.Typeflag != tar.TypeSymlink || entry.header.Linkname != target || entry.body != "" {
			t.Errorf("link %s was followed or changed: %+v", name, entry)
		}
	}
	for name, entry := range entries {
		if strings.Contains(entry.body, secret) || strings.HasPrefix(name, "directory-link/") {
			t.Errorf("outside content leaked through %s", name)
		}
	}
}

func TestPrepareContextRejectsInvalidControlFiles(t *testing.T) {
	for _, name := range []string{"Dockerfile", ".dockerignore", "Dockerfile.dockerignore"} {
		for _, kind := range []string{"symlink", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root := newContextTestRoot(t)
				_ = os.Remove(filepath.Join(root, name))
				path := filepath.Join(root, name)
				if kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				if archive, err := prepareContext(t.Context(), root); archive != nil || !errors.Is(err, ErrInvalidContext) {
					if archive != nil {
						_ = archive.Close()
					}
					t.Fatalf("invalid control file accepted: %v", err)
				}
			})
		}
	}
	for _, rules := range []string{"[\n", strings.Repeat("#x\n", maxIgnoreBytes/3+1)} {
		root := newContextTestRoot(t)
		writeContextTestFile(t, root, ".dockerignore", rules)
		if archive, err := prepareContext(t.Context(), root); archive != nil || !errors.Is(err, ErrInvalidContext) {
			if archive != nil {
				_ = archive.Close()
			}
			t.Errorf("invalid/oversized ignore rules accepted: %v", err)
		}
	}
}

func TestPrepareContextRejectsSpecialFiles(t *testing.T) {
	root, err := os.MkdirTemp("", "ctx-") // Keep Unix socket paths below platform limits.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	writeContextTestFile(t, root, "Dockerfile", "FROM scratch\n")
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Skipf("Unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	if archive, err := prepareContext(t.Context(), root); archive != nil || !errors.Is(err, ErrInvalidContext) {
		if archive != nil {
			_ = archive.Close()
		}
		t.Errorf("included socket accepted: %v", err)
	}
}

func TestBuildContextCleanupAndCancellation(t *testing.T) {
	temp, root := t.TempDir(), newContextTestRoot(t)
	t.Setenv("TMPDIR", temp)
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	archive, err := prepareContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := archive.Stat(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("archive permissions: %v, %v", info, err)
	}
	if archive.Name() != filepath.Join(temp, filepath.Base(archive.Name())) {
		t.Fatalf("archive was not created outside checkout: %s", archive.Name())
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive.Name(), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(archive.Name()); err != nil || string(data) != "replacement" {
		t.Fatalf("repeated Close removed another file: %q, %v", data, err)
	}
	if err := os.Remove(archive.Name()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if archive, err := prepareContext(ctx, root); archive != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation: %v", err)
	}
	// This fails after the archive is created, proving failure cleanup as well.
	for i := 0; i < maxContextEntries; i++ {
		writeContextTestFile(t, root, fmt.Sprintf("f%05d", i), "")
	}
	if archive, err := prepareContext(t.Context(), root); archive != nil || !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("entry limit not enforced: %v", err)
	}
	if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
		t.Fatalf("temporary archives leaked: %d entries, %v", len(entries), err)
	}
}

func TestContextByteLimitAndMidCopyCancellation(t *testing.T) {
	var buffer bytes.Buffer
	w := &contextLimitWriter{writer: &buffer, remaining: 4}
	if n, err := w.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("exact limit rejected: %d, %v", n, err)
	}
	if n, err := w.Write([]byte("5")); n != 0 || !errors.Is(err, ErrContextTooLarge) || buffer.String() != "1234" {
		t.Fatalf("byte limit exceeded: %d, %v, %q", n, err, buffer.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &contextInputReader{ctx: ctx, reader: &contextCancelTestReader{cancel: cancel}}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy ignored mid-stream cancellation: %v", err)
	}
}

type contextCancelTestReader struct{ cancel context.CancelFunc }

func (r *contextCancelTestReader) Read(p []byte) (int, error) {
	if r.cancel == nil {
		return 0, io.EOF
	}
	r.cancel()
	r.cancel = nil
	return copy(p, "payload"), nil
}

func newContextTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeContextTestFile(t, root, "Dockerfile", "FROM scratch\nCOPY . /app/\n")
	return root
}

func writeContextTestFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

type contextTestEntry struct {
	header *tar.Header
	body   string
}

func readContextTestArchive(t *testing.T, archive *buildContext) map[string]contextTestEntry {
	t.Helper()
	entries := make(map[string]contextTestEntry)
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = contextTestEntry{header: header, body: string(body)}
	}
}
