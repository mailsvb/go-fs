package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

// openStore opens a persistent data file of the test's own.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// waitFor waits until the status of the sync passes check.
func waitFor(t *testing.T, name string, check func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, ok := Statuses()[name]
		if ok && check(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the status of %s is %+v (%v)", name, status, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManager(t *testing.T) {
	f := newFixture(t, config.SyncPush, false)
	cfg := config.Default()
	cfg.General.BaseFolderFiles = filepath.Dir(f.local)
	cfg.Servers = []config.Server{f.job.server}
	cfg.Syncs = []config.Sync{f.job.sync}
	f.writeLocal(t, "a.txt", "a", then)

	m, err := New(cfg, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	// at an interval, the first run is at once, and the next an interval
	// after it
	first := waitFor(t, "photos", func(s Status) bool { return !s.Finished.IsZero() && !s.Running })
	if first.Uploaded != 1 || first.Error != "" {
		t.Errorf("the first run did %+v", first)
	}
	if !first.Next.After(time.Now().Add(50 * time.Minute)) {
		t.Errorf("the next run is at %v, want an hour after the first", first.Next)
	}
	holds(t, f.remote("a.txt"), "a", then)

	// run now
	f.writeLocal(t, "b.txt", "b", then)
	if err := Trigger("photos"); err != nil {
		t.Fatal(err)
	}
	second := waitFor(t, "photos", func(s Status) bool { return s.Finished.After(first.Finished) && !s.Running })
	if second.Uploaded != 1 {
		t.Errorf("the run asked for did %+v", second)
	}

	// disabled, it stops, and its status with it
	cfg.Syncs[0].Enabled = false
	if err := m.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := Statuses()["photos"]; ok {
		t.Error("a disabled sync still has a status")
	}
	if err := Trigger("photos"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("running a disabled sync answered %v", err)
	}
}

func TestManagerKeepsTheLastRun(t *testing.T) {
	f := newFixture(t, config.SyncPush, false)
	cfg := config.Default()
	cfg.General.BaseFolderFiles = filepath.Dir(f.local)
	cfg.Servers = []config.Server{f.job.server}
	cfg.Syncs = []config.Sync{f.job.sync}
	f.writeLocal(t, "a.txt", "a", then)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := openStore(t)

	m, err := New(cfg, st, "", logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := waitFor(t, "photos", func(s Status) bool { return !s.Finished.IsZero() && !s.Running })
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// started again, it shows the run before and does not run again
	// before it is due
	m, err = New(cfg, st, "", logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	again := waitFor(t, "photos", func(s Status) bool { return !s.Next.IsZero() })
	if !again.Finished.Equal(first.Finished) || again.Uploaded != first.Uploaded {
		t.Errorf("after a restart the status is %+v, want the run before, %+v", again, first)
	}
	if want := first.Started.Add(time.Hour); !again.Next.Equal(want) {
		t.Errorf("after a restart the next run is at %v, want an hour after the run before, %v", again.Next, want)
	}
	time.Sleep(200 * time.Millisecond)
	if s := Statuses()["photos"]; s.Running || !s.Finished.Equal(first.Finished) {
		t.Errorf("the sync ran again at once after a restart: %+v", s)
	}
}

// The snapshots an earlier version kept as files beside the configuration
// are moved into the data file; the file of a sync this configuration does
// not name is left alone, and the folder with it.
func TestNewMigratesTheSnapshotFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Syncs = []config.Sync{{Name: "photos"}}
	taken := snapshot{Server: "backup", RemoteFolder: "remote", LocalFolder: "/photos",
		Files: map[string]entry{"a.txt": {Size: 1, Mod: then}}}
	write := func(dir, name string) {
		t.Helper()
		body, err := json.Marshal(taken)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	legacy := filepath.Join(t.TempDir(), "go-fs-sync")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	write(legacy, "photos")
	write(legacy, "other")
	st := openStore(t)
	if _, err := New(cfg, st, legacy, logger); err != nil {
		t.Fatal(err)
	}
	var got snapshot
	if found, err := st.Bucket("syncs.snapshots").Get("photos", &got); err != nil || !found {
		t.Fatalf("the snapshot was not moved: %v, %v", found, err)
	}
	if got.Files["a.txt"].Size != 1 || got.LocalFolder != "/photos" {
		t.Errorf("the snapshot moved is %+v", got)
	}
	if _, err := os.Stat(filepath.Join(legacy, "photos.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file of a snapshot moved is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "other.json")); err != nil {
		t.Errorf("the file of a sync not configured was touched: %v", err)
	}

	// with nothing left in it, the folder goes
	alone := filepath.Join(t.TempDir(), "go-fs-sync")
	if err := os.Mkdir(alone, 0o700); err != nil {
		t.Fatal(err)
	}
	write(alone, "photos")
	if _, err := New(cfg, openStore(t), alone, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alone); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied folder is still there: %v", err)
	}
}

// Prune removes the status and snapshot of a sync taken out of the
// configuration, and keeps those of one configured, disabled or not.
func TestPrune(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := openStore(t)
	status, snapshots := st.Bucket(statusBucket), st.Bucket(snapshotBucket)
	for _, name := range []string{"photos", "music", "gone"} {
		if err := status.Put(name, Status{}); err != nil {
			t.Fatal(err)
		}
		if err := snapshots.Put(name, snapshot{}); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(bucket *store.Bucket) []string {
		t.Helper()
		var names []string
		if err := bucket.Scan("", func(name string, _ func(any) error) error {
			names = append(names, name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return names
	}

	cfg := config.Default()
	cfg.Syncs = []config.Sync{{Name: "photos", Enabled: true}, {Name: "music"}}
	Prune(st, cfg, logger)
	for _, bucket := range []*store.Bucket{status, snapshots} {
		if got := kept(bucket); !slices.Equal(got, []string{"music", "photos"}) {
			t.Errorf("after a prune %v is kept", got)
		}
	}
	Prune(st, config.Default(), logger)
	for _, bucket := range []*store.Bucket{status, snapshots} {
		if got := kept(bucket); len(got) != 0 {
			t.Errorf("after a prune with no sync %v is kept", got)
		}
	}
	Prune(nil, cfg, logger)
}
