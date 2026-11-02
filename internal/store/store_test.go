package store

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// clock is a time the test moves forward by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openTest opens a store in a temporary folder, on a clock of its own, with
// the sweep left to the test.
func openTest(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	s, err := open(filepath.Join(t.TempDir(), "go-fs.db"), discard, c.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

type share struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
}

func TestDefaultPath(t *testing.T) {
	cases := []struct{ config, want string }{
		{"/etc/go-fs/go-fs.toml", "/etc/go-fs/go-fs.db"},
		{"go-fs.toml", "go-fs.db"},
		{"conf/other.toml", "conf/other.db"},
		{"noextension", "noextension.db"},
	}
	for _, tc := range cases {
		if got := DefaultPath(tc.config); got != tc.want {
			t.Errorf("DefaultPath(%q) = %q, want %q", tc.config, got, tc.want)
		}
	}
}

func TestPutGet(t *testing.T) {
	s, _ := openTest(t)
	shares := s.Bucket("shares")

	var got share
	if found, err := shares.Get("abc", &got); err != nil || found {
		t.Fatalf("Get from a bucket that does not exist = %v, %v, want false, nil", found, err)
	}

	want := share{Path: "/music/a.flac", Owner: "doe"}
	if err := shares.Put("abc", want); err != nil {
		t.Fatal(err)
	}
	found, err := shares.Get("abc", &got)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v, want true, nil", found, err)
	}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	if found, err := shares.Get("missing", &got); err != nil || found {
		t.Errorf("Get of a missing key = %v, %v, want false, nil", found, err)
	}
	// the same key in another bucket is another record
	if found, err := s.Bucket("other").Get("abc", &got); err != nil || found {
		t.Errorf("Get from another bucket = %v, %v, want false, nil", found, err)
	}

	if err := shares.Delete("abc"); err != nil {
		t.Fatal(err)
	}
	if found, err := shares.Get("abc", &got); err != nil || found {
		t.Errorf("Get after Delete = %v, %v, want false, nil", found, err)
	}
	if err := shares.Delete("abc"); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}
	if err := s.Bucket("never").Delete("abc"); err != nil {
		t.Errorf("Delete from a bucket that does not exist = %v, want nil", err)
	}
}

func TestExpiry(t *testing.T) {
	s, c := openTest(t)
	shares := s.Bucket("shares")
	if err := shares.PutUntil("short", "a", c.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := shares.PutUntil("long", "b", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := shares.PutUntil("zero", "c", time.Time{}); err != nil {
		t.Fatal(err)
	}

	var v string
	if found, _ := shares.Get("short", &v); !found {
		t.Fatal("a record that has not run out is not found")
	}
	c.Advance(time.Minute)
	if found, _ := shares.Get("short", &v); found {
		t.Error("a record that ran out is still found")
	}
	if keys := scanKeys(t, shares, ""); strings.Join(keys, ",") != "long,zero" {
		t.Errorf("Scan = %v, want [long zero]", keys)
	}

	removed, err := s.sweep()
	if err != nil || removed != 1 {
		t.Fatalf("sweep = %d, %v, want 1, nil", removed, err)
	}
	c.Advance(100 * 365 * 24 * time.Hour)
	removed, err = s.sweep()
	if err != nil || removed != 1 {
		t.Fatalf("second sweep = %d, %v, want 1, nil", removed, err)
	}
	if found, _ := shares.Get("zero", &v); !found || v != "c" {
		t.Errorf("a record put until the zero time = %v, %q, want it kept for good", found, v)
	}
}

func TestScan(t *testing.T) {
	s, _ := openTest(t)
	plays := s.Bucket("jellyfin.playstate")
	if keys := scanKeys(t, plays, ""); len(keys) != 0 {
		t.Errorf("Scan of a bucket that does not exist = %v, want none", keys)
	}
	for _, key := range []string{"doe/2", "doe/1", "roe/1", "do/1"} {
		if err := plays.Put(key, key+"!"); err != nil {
			t.Fatal(err)
		}
	}
	if keys := scanKeys(t, plays, "doe/"); strings.Join(keys, ",") != "doe/1,doe/2" {
		t.Errorf("Scan(doe/) = %v, want [doe/1 doe/2]", keys)
	}

	// fn may write to the store, and an error from it ends the scan
	stop := errors.New("stop")
	calls := 0
	err := plays.Scan("", func(key string, decode func(any) error) error {
		calls++
		var v string
		if err := decode(&v); err != nil || v != key+"!" {
			t.Errorf("decode of %s = %q, %v", key, v, err)
		}
		if err := plays.Delete(key); err != nil {
			t.Errorf("Delete inside Scan: %v", err)
		}
		if calls == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 2 {
		t.Errorf("Scan = %v after %d calls, want stop after 2", err, calls)
	}
	if keys := scanKeys(t, plays, ""); strings.Join(keys, ",") != "doe/2,roe/1" {
		t.Errorf("after the scan = %v, want [doe/2 roe/1]", keys)
	}
}

func TestReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "data.db")
	s, err := Open(path, discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bucket("shares").Put("abc", share{Path: "/a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close = %v, want nil", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("the file has mode %04o, want 0600", mode)
		}
	}

	s, err = Open(path, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got share
	if found, err := s.Bucket("shares").Get("abc", &got); err != nil || !found || got.Path != "/a" {
		t.Errorf("Get after reopening = %+v, %v, %v", got, found, err)
	}
}

func TestOpenRefusesFileInUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go-fs.db")
	s, err := Open(path, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := Open(path, discard)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second Open of a file in use succeeded")
	}
	if !strings.Contains(err.Error(), "in use by another go-fs") {
		t.Errorf("error = %v, want it to say the file is in use", err)
	}
}

func scanKeys(t *testing.T, b *Bucket, prefix string) []string {
	t.Helper()
	var keys []string
	if err := b.Scan(prefix, func(key string, _ func(any) error) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestNilKeepsNothing(t *testing.T) {
	var s *Store
	b := s.Bucket("shares")
	if b != nil {
		t.Fatalf("Bucket of a nil store = %v, want nil", b)
	}
	if err := b.Put("abc", share{Path: "/a"}); err != nil {
		t.Fatal(err)
	}
	var got share
	if found, err := b.Get("abc", &got); err != nil || found {
		t.Fatalf("Get from a nil bucket = %v, %v, want false, nil", found, err)
	}
	if err := b.Delete("abc"); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := b.Scan("", func(string, func(any) error) error { called = true; return nil }); err != nil || called {
		t.Fatalf("Scan of a nil bucket = %v and called %v, want nil and not called", err, called)
	}
	if err := b.Write(func(*Writer) error { called = true; return nil }); err != nil || called {
		t.Fatalf("Write to a nil bucket = %v and called %v, want nil and not called", err, called)
	}
	var made string
	created, err := b.Remembered("key", &made, func() error { made = "fresh"; return nil })
	if err != nil || !created || made != "fresh" {
		t.Fatalf("Remembered on a nil bucket = %v, %v, %q, want true, nil, %q", created, err, made, "fresh")
	}
}

func TestWrite(t *testing.T) {
	s, _ := openTest(t)
	shares := s.Bucket("shares")
	if err := shares.Put("old", share{Path: "/old"}); err != nil {
		t.Fatal(err)
	}

	err := shares.Write(func(w *Writer) error {
		if err := w.Put("a", share{Path: "/a"}); err != nil {
			return err
		}
		if err := w.Put("b", share{Path: "/b"}); err != nil {
			return err
		}
		return w.Delete("old")
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := shares.Scan("", func(key string, _ func(any) error) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "a,b" {
		t.Fatalf("keys after Write = %v, want [a b]", keys)
	}

	// a write that fails leaves the bucket as it was
	failed := errors.New("failed")
	err = shares.Write(func(w *Writer) error {
		if err := w.Delete("a"); err != nil {
			return err
		}
		if err := w.Put("c", share{Path: "/c"}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("Write = %v, want %v", err, failed)
	}
	var got share
	if found, _ := shares.Get("a", &got); !found {
		t.Error("the delete of a Write that failed was kept")
	}
	if found, _ := shares.Get("c", &got); found {
		t.Error("the put of a Write that failed was kept")
	}
}

func TestRemembered(t *testing.T) {
	s, _ := openTest(t)
	keys := s.Bucket("keys")

	calls := 0
	create := func(v *string) func() error {
		return func() error {
			calls++
			*v = "secret"
			return nil
		}
	}
	var first string
	created, err := keys.Remembered("k", &first, create(&first))
	if err != nil || !created || first != "secret" {
		t.Fatalf("first Remembered = %v, %v, %q, want true, nil, %q", created, err, first, "secret")
	}
	var second string
	created, err = keys.Remembered("k", &second, create(&second))
	if err != nil || created || second != "secret" {
		t.Fatalf("second Remembered = %v, %v, %q, want false, nil, %q", created, err, second, "secret")
	}
	if calls != 1 {
		t.Fatalf("create was called %d times, want once", calls)
	}

	// a create that fails keeps nothing
	failed := errors.New("failed")
	var none string
	if _, err := keys.Remembered("other", &none, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("Remembered = %v, want %v", err, failed)
	}
	if found, _ := keys.Get("other", &none); found {
		t.Fatal("the record of a create that failed was kept")
	}
}

func TestBucketsAndDropBucket(t *testing.T) {
	s, _ := openTest(t)
	for _, name := range []string{"repository.music", "repository.apps", "syncs"} {
		if err := s.Bucket(name).Put("k", share{Path: name}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := s.Buckets("repository.")
	if err != nil || strings.Join(names, ",") != "repository.apps,repository.music" {
		t.Fatalf("Buckets = %v, %v, want [repository.apps repository.music]", names, err)
	}

	if err := s.DropBucket("repository.music"); err != nil {
		t.Fatal(err)
	}
	var got share
	if found, err := s.Bucket("repository.music").Get("k", &got); err != nil || found {
		t.Fatalf("Get from a dropped bucket = %v, %v, want false, nil", found, err)
	}
	if found, _ := s.Bucket("repository.apps").Get("k", &got); !found {
		t.Fatal("dropping one bucket took another with it")
	}
	if err := s.DropBucket("repository.music"); err != nil {
		t.Fatalf("dropping a bucket that is not there = %v, want nil", err)
	}
	if names, _ := s.Buckets(""); strings.Join(names, ",") != "repository.apps,syncs" {
		t.Fatalf("Buckets after the drop = %v", names)
	}

	var none *Store
	if names, err := none.Buckets(""); err != nil || names != nil {
		t.Fatalf("Buckets of a nil store = %v, %v", names, err)
	}
	if err := none.DropBucket("x"); err != nil {
		t.Fatal(err)
	}
}
