// Package syncer runs the syncs of [[syncs]]: each keeps a folder of
// general.baseFolderFiles and a folder of a stored server alike, at an
// interval or at a time of day, one way or both ways.
//
// The one protocol is rsync, done here rather than by the rsync program: both
// folders are listed, a file whose size or modification time differs is
// copied whole, and the copy is given the modification time of the original,
// which is what tells the next run it is alike. It needs nothing on either
// host but SFTP, so it works with a password login, in a vault and on every
// system go-fs runs on.
//
// Both ways, a file changed on one side since the last run is copied to the
// other, and of one changed on both the newer wins. What was alike after the
// last run is kept in a snapshot, which is what tells a file removed on one
// side from one that is new on the other.
//
// Like internal/cleanup this deletes without a client having asked, so every
// removal is reported.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/store"
)

// protocols are how a run of a sync is done, by the protocol it names.
var protocols = map[string]func(context.Context, job, *slog.Logger) (result, error){
	config.SyncProtocolRsync: runRsync,
}

// StateDir is where the snapshots of the syncs are kept: beside the
// configuration file at path, which go-fs writes to already. "" keeps none,
// and a sync both ways then removes nothing.
func StateDir(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(path), "go-fs-sync")
}

// Manager runs the syncs. It is run by the supervisor like a server,
// whichever servers are enabled.
type Manager struct {
	log      *slog.Logger
	stateDir string
	// kept is where the status of each sync's last run is kept, by its
	// name, so that a restart neither forgets it nor runs a sync again
	// before it is due.
	kept *store.Bucket

	mu      sync.Mutex
	running map[string]*runner
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a manager for the enabled syncs of cfg, which keeps their
// snapshots in stateDir and the status of their last runs in kept.
func New(cfg config.Config, stateDir string, kept *store.Bucket, logger *slog.Logger) (*Manager, error) {
	m := &Manager{log: logger, stateDir: stateDir, kept: kept}
	m.prune(cfg)
	m.running = build(cfg, stateDir, kept, logger)
	return m, nil
}

// prune removes the kept status of every sync cfg no longer has. One that is
// disabled keeps it, for when it is enabled again.
func (m *Manager) prune(cfg config.Config) {
	configured := map[string]bool{}
	for _, s := range cfg.Syncs {
		configured[s.Name] = true
	}
	err := m.kept.Scan("", func(name string, _ func(any) error) error {
		if configured[name] {
			return nil
		}
		return m.kept.Delete(name)
	})
	if err != nil {
		m.log.Warn("cannot remove the kept status of the syncs taken out", "error", err)
	}
}

// build makes a runner for every sync of cfg the binary runs, by name.
func build(cfg config.Config, stateDir string, kept *store.Bucket, logger *slog.Logger) map[string]*runner {
	syncs := cfg.UsableSyncs()
	out := make(map[string]*runner, len(syncs))
	for _, s := range syncs {
		server, _ := config.FindServer(cfg.UsableServers(), s.Server)
		j := job{
			sync: s, server: server, ssh: cfg.General.SSH,
			local:    config.RepositoryPath(cfg.General.BaseFolderFiles, s.LocalFolder),
			trash:    cfg.General.BaseFolderTrash,
			stateDir: stateDir,
		}
		out[s.Name] = newRunner(j, kept, logger)
	}
	return out
}

// Start runs every sync on its schedule, until ctx ends or Shutdown is
// called.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx, m.cancel = context.WithCancel(ctx)
	for _, r := range m.running {
		m.launch(r)
	}
	return nil
}

func (m *Manager) launch(r *runner) {
	r.launched = true
	enlist(r)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		r.run(m.ctx)
	}()
}

// Reload takes over the syncs of cfg. One whose job did not change keeps
// running as it was; one that changed is stopped, a run in progress with it,
// and started again; one taken out or disabled stops, and its status with
// it.
func (m *Manager) Reload(cfg config.Config) error {
	m.prune(cfg)
	wanted := build(cfg, m.stateDir, m.kept, m.log)
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*runner, len(wanted))
	for name, r := range wanted {
		if old, ok := m.running[name]; ok && reflect.DeepEqual(old.job, r.job) {
			next[name] = old
			continue
		}
		next[name] = r
	}
	for name, old := range m.running {
		if next[name] != old {
			old.stop()
			forget(name, old)
		}
	}
	for name, r := range next {
		if m.running[name] != r && m.ctx != nil {
			m.launch(r)
		}
	}
	m.running = next
	return nil
}

// Shutdown stops every sync, a run in progress with it, and waits for them.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	for name, r := range m.running {
		r.stop()
		forget(name, r)
	}
	m.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status is what the admin interface shows of a sync.
type Status struct {
	// Running says a run is in progress, which began at Started.
	Running bool      `json:"running,omitempty"`
	Started time.Time `json:"started,omitzero"`
	// Finished is when the last run ended, zero before the first one.
	Finished time.Time `json:"finished,omitzero"`
	// Next is when the sync runs next.
	Next time.Time `json:"next,omitzero"`
	// Uploaded and Downloaded are how many files the last run copied to
	// the remote folder and to the local one, Deleted how many files and
	// folders it removed, and Bytes how much it copied.
	Uploaded   int   `json:"uploaded"`
	Downloaded int   `json:"downloaded"`
	Deleted    int   `json:"deleted"`
	Bytes      int64 `json:"bytes"`
	// Conflicts are the files the last run found changed on both sides.
	Conflicts int `json:"conflicts,omitempty"`
	// Error is why the last run failed, or what it did not do, "" when it
	// did all of it.
	Error string `json:"error,omitempty"`
	// Failed is how many paths the last run could not sync, and Failures
	// why, for the first of them.
	Failed   int               `json:"failed,omitempty"`
	Failures map[string]string `json:"failures,omitempty"`
}

// statuses are the statuses of the running syncs, and runners the runners,
// by name. They are kept here rather than on a Manager, because the admin
// interface that shows them, and runs a sync now, is in the http server,
// which the supervisor builds apart from this.
var statuses = struct {
	sync.Mutex
	byName  map[string]Status
	runners map[string]*runner
}{byName: map[string]Status{}, runners: map[string]*runner{}}

// Statuses reports the status of every running sync, by name.
func Statuses() map[string]Status {
	statuses.Lock()
	defer statuses.Unlock()
	out := make(map[string]Status, len(statuses.byName))
	for name, status := range statuses.byName {
		status.Failures = maps.Clone(status.Failures)
		out[name] = status
	}
	return out
}

// ErrNotRunning is a sync Trigger cannot run: there is none of the name, or
// it is disabled, or the syncs are not running.
var ErrNotRunning = errors.New("the sync is not running")

// ErrBusy is a sync Trigger finds running already.
var ErrBusy = errors.New("the sync is running already")

// Trigger runs a sync now, rather than at its next time.
func Trigger(name string) error {
	statuses.Lock()
	r, ok := statuses.runners[name]
	statuses.Unlock()
	if !ok {
		return fmt.Errorf("%q: %w", name, ErrNotRunning)
	}
	return r.trigger()
}

func enlist(r *runner) {
	statuses.Lock()
	defer statuses.Unlock()
	statuses.runners[r.job.sync.Name] = r
}

func report(r *runner, status Status) {
	statuses.Lock()
	defer statuses.Unlock()
	// a runner that was replaced does not report over the one that
	// replaced it
	if statuses.runners[r.job.sync.Name] == r {
		statuses.byName[r.job.sync.Name] = status
	}
}

func forget(name string, r *runner) {
	statuses.Lock()
	defer statuses.Unlock()
	if statuses.runners[name] == r {
		delete(statuses.runners, name)
		delete(statuses.byName, name)
	}
}

// loginOf is the login of a stored server.
func loginOf(server config.Server) (remote.Login, error) {
	login, err := remote.FromServer(server).Checked(false)
	if err != nil {
		return remote.Login{}, fmt.Errorf("server %q: %w", server.Name, err)
	}
	return login, nil
}
