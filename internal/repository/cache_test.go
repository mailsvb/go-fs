package repository

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/mailsvb/go-fs/internal/store"
)

// withCache gives r a cache in a store of its own, and answers it.
func withCache(t *testing.T, r *runner) *store.Bucket {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.cache = st.Bucket("repository." + r.repo.Name)
	return r.cache
}

// restart makes the runner a restart of go-fs makes of r, from what r kept.
func restart(t *testing.T, r *runner) *runner {
	t.Helper()
	next, err := newRunner(r.repo, r.base, r.cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	next.restore()
	return next
}

// disguise replaces the file at path with as many bytes that are no package,
// keeping its modification time, so that only a runner that reads it again
// finds out.
func disguise(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, int(info.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}
