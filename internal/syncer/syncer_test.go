package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

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

	m, err := New(cfg, t.TempDir(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	kept := st.Bucket("syncs")
	if err := kept.Put("gone", Status{Uploaded: 9}); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()

	m, err := New(cfg, stateDir, kept, logger)
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
	var gone Status
	if found, _ := kept.Get("gone", &gone); found {
		t.Error("the status of a sync taken out of the configuration was kept")
	}

	// started again, it shows the run before and does not run again
	// before it is due
	m, err = New(cfg, stateDir, kept, logger)
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
