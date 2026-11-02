package repository

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
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

// Prune drops the cache of a repository taken out of the configuration and
// keeps that of one configured, of a kind built in or not; with none left, every
// cache goes.
func TestPrune(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, name := range []string{"one", "two", "gone"} {
		if err := st.Bucket(cachePrefix+name).Put(metaKey, cacheMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Bucket("syncs").Put("other", cacheMeta{}); err != nil {
		t.Fatal(err)
	}
	buckets := func() string {
		t.Helper()
		names, err := st.Buckets("")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(names, ",")
	}

	cfg := config.Config{Repositories: []config.Repository{{Name: "one"}, {Name: "two", Type: "not built in"}}}
	Prune(st, cfg, logger)
	if got := buckets(); got != "repository.one,repository.two,syncs" {
		t.Errorf("after a prune the buckets are %q", got)
	}
	Prune(st, config.Config{}, logger)
	if got := buckets(); got != "syncs" {
		t.Errorf("after a prune with no repository the buckets are %q", got)
	}
	Prune(nil, cfg, logger)
}
