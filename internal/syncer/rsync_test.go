package syncer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

func skipWithout(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if !features.Has(name) {
			t.Skipf("built without %s", name)
		}
	}
}

// fixture is a sync between a local folder and a folder of an SFTP host.
type fixture struct {
	host  *remotetest.SFTPHost
	local string
	trash string
	job   job
	logs  *bytes.Buffer
}

func newFixture(t *testing.T, direction string, del bool) *fixture {
	t.Helper()
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	if err := os.Mkdir(filepath.Join(host.Dir, "remote"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	local := filepath.Join(base, "photos")
	if err := os.Mkdir(local, 0o755); err != nil {
		t.Fatal(err)
	}
	server := config.Server{Name: "backup", Type: config.ServerTypeSFTP, Host: host.Host, Port: host.Port,
		Username: "alice", Password: "secret", HostKeyFingerprint: host.Fingerprint}
	f := &fixture{host: host, local: local, trash: t.TempDir(), logs: &bytes.Buffer{}}
	f.job = job{
		sync: config.Sync{Name: "photos", Enabled: true, Protocol: config.SyncProtocolRsync, Server: "backup",
			RemoteFolder: "remote", LocalFolder: "/photos", Direction: direction, Delete: del, Every: "1h"},
		server: server, ssh: config.Default().General.SSH,
		local: local, trash: f.trash, stateDir: t.TempDir(),
	}
	return f
}

func (f *fixture) run(t *testing.T) (result, error) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return runRsync(context.Background(), f.job, log)
}

func (f *fixture) mustRun(t *testing.T) result {
	t.Helper()
	res, err := f.run(t)
	if err != nil {
		t.Fatalf("the run failed: %v\n%s", err, f.logs)
	}
	if res.failed > 0 {
		t.Fatalf("the run failed for %v\n%s", res.failures, f.logs)
	}
	return res
}

// write puts a file into a folder, with a modification time.
func write(t *testing.T, root, name, content string, mod time.Time) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) remote(name string) string {
	return filepath.Join(f.host.Dir, "remote", filepath.FromSlash(name))
}

func (f *fixture) writeRemote(t *testing.T, name, content string, mod time.Time) {
	t.Helper()
	write(t, filepath.Join(f.host.Dir, "remote"), name, content, mod)
}

func (f *fixture) writeLocal(t *testing.T, name, content string, mod time.Time) {
	t.Helper()
	write(t, f.local, name, content, mod)
}

// holds checks a file has the content and the modification time, to the
// second.
func holds(t *testing.T, at, content string, mod time.Time) {
	t.Helper()
	body, err := os.ReadFile(at)
	if err != nil {
		t.Errorf("%s: %v", at, err)
		return
	}
	if string(body) != content {
		t.Errorf("%s holds %q, want %q", at, body, content)
	}
	info, _ := os.Stat(at)
	if !info.ModTime().Truncate(time.Second).Equal(mod.Truncate(time.Second)) {
		t.Errorf("%s was modified %v, want %v", at, info.ModTime(), mod)
	}
}

func missing(t *testing.T, at string) {
	t.Helper()
	if _, err := os.Lstat(at); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still there", at)
	}
}

var then = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

func TestPush(t *testing.T) {
	f := newFixture(t, config.SyncPush, false)
	f.writeLocal(t, "a.txt", "a", then)
	f.writeLocal(t, "deep/er/b.txt", "bb", then.Add(time.Hour))
	f.writeLocal(t, ".x.go-fs-1234.part", "half", then)
	f.writeRemote(t, "only-remote.txt", "r", then)

	res := f.mustRun(t)
	if res.uploaded != 2 || res.downloaded != 0 || res.deleted != 0 || res.bytes != 3 {
		t.Errorf("the run did %+v", res)
	}
	holds(t, f.remote("a.txt"), "a", then)
	holds(t, f.remote("deep/er/b.txt"), "bb", then.Add(time.Hour))
	missing(t, f.remote(".x.go-fs-1234.part"))
	holds(t, f.remote("only-remote.txt"), "r", then)
	if left := f.host.Leftovers(t, "remote"); len(left) != 0 {
		t.Errorf("the run left %v", left)
	}

	// alike now, so nothing is copied again
	if res := f.mustRun(t); res.uploaded != 0 || res.bytes != 0 {
		t.Errorf("a second run did %+v", res)
	}
	// a changed file is copied again
	f.writeLocal(t, "a.txt", "changed", then.Add(2*time.Hour))
	if res := f.mustRun(t); res.uploaded != 1 {
		t.Errorf("a run after a change did %+v", res)
	}
	holds(t, f.remote("a.txt"), "changed", then.Add(2*time.Hour))
}

func TestPushWithDelete(t *testing.T) {
	f := newFixture(t, config.SyncPush, true)
	f.writeLocal(t, "a.txt", "a", then)
	f.writeRemote(t, "gone/x.txt", "x", then)
	f.writeRemote(t, "old.txt", "o", then)

	res := f.mustRun(t)
	if res.uploaded != 1 || res.deleted != 3 {
		t.Errorf("the run did %+v", res)
	}
	missing(t, f.remote("gone"))
	missing(t, f.remote("old.txt"))
	if !strings.Contains(f.logs.String(), "sync removed") {
		t.Error("a removal is not logged")
	}
}

func TestPullWithDeleteToTheTrash(t *testing.T) {
	f := newFixture(t, config.SyncPull, true)
	f.writeRemote(t, "a.txt", "a", then)
	f.writeLocal(t, "old/x.txt", "x", then)

	res := f.mustRun(t)
	if res.downloaded != 1 || res.deleted != 2 {
		t.Errorf("the run did %+v", res)
	}
	holds(t, filepath.Join(f.local, "a.txt"), "a", then)
	missing(t, filepath.Join(f.local, "old"))
	// the trash holds the file at the path it had in the served folder
	if body, err := os.ReadFile(filepath.Join(f.trash, "photos", "old", "x.txt")); err != nil || string(body) != "x" {
		t.Errorf("the trash does not hold the removed file: %v", err)
	}
}

func TestAnEmptiedSideRemovesNothing(t *testing.T) {
	f := newFixture(t, config.SyncPull, true)
	f.writeLocal(t, "keep.txt", "k", then)
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "is empty while the other side is not") {
		t.Errorf("the run answered %v", err)
	}
	holds(t, filepath.Join(f.local, "keep.txt"), "k", then)
}

func TestBothWays(t *testing.T) {
	f := newFixture(t, config.SyncBoth, true)
	f.writeLocal(t, "local.txt", "l", then)
	f.writeRemote(t, "remote.txt", "r", then)
	f.writeLocal(t, "both.txt", "older", then)
	f.writeRemote(t, "both.txt", "newer", then.Add(time.Hour))

	// a first run removes nothing, and of what differs the newer wins
	res := f.mustRun(t)
	if res.uploaded != 1 || res.downloaded != 2 || res.deleted != 0 {
		t.Errorf("the first run did %+v", res)
	}
	holds(t, f.remote("local.txt"), "l", then)
	holds(t, filepath.Join(f.local, "remote.txt"), "r", then)
	holds(t, filepath.Join(f.local, "both.txt"), "newer", then.Add(time.Hour))

	// a file removed on one side is removed on the other, one changed is
	// copied
	if err := os.Remove(f.remote("local.txt")); err != nil {
		t.Fatal(err)
	}
	f.writeLocal(t, "remote.txt", "edited", then.Add(3*time.Hour))
	res = f.mustRun(t)
	if res.uploaded != 1 || res.downloaded != 0 || res.deleted != 1 {
		t.Errorf("the second run did %+v", res)
	}
	missing(t, filepath.Join(f.local, "local.txt"))
	holds(t, f.remote("remote.txt"), "edited", then.Add(3*time.Hour))

	// a file removed on one side and changed on the other is kept
	if err := os.Remove(filepath.Join(f.local, "both.txt")); err != nil {
		t.Fatal(err)
	}
	f.writeRemote(t, "both.txt", "changed again", then.Add(4*time.Hour))
	res = f.mustRun(t)
	if res.downloaded != 1 || res.deleted != 0 {
		t.Errorf("the third run did %+v", res)
	}
	holds(t, filepath.Join(f.local, "both.txt"), "changed again", then.Add(4*time.Hour))
}

func TestBothWaysWithoutDeleteBringsBack(t *testing.T) {
	f := newFixture(t, config.SyncBoth, false)
	f.writeLocal(t, "a.txt", "a", then)
	f.mustRun(t)
	if err := os.Remove(f.remote("a.txt")); err != nil {
		t.Fatal(err)
	}
	if res := f.mustRun(t); res.uploaded != 1 || res.deleted != 0 {
		t.Errorf("the run did %+v", res)
	}
	holds(t, f.remote("a.txt"), "a", then)
}

func TestASnapshotOfOtherFoldersIsNotUsed(t *testing.T) {
	f := newFixture(t, config.SyncBoth, true)
	f.writeLocal(t, "a.txt", "a", then)
	f.mustRun(t)
	if err := os.Remove(f.remote("a.txt")); err != nil {
		t.Fatal(err)
	}
	f.job.sync.LocalFolder = "/elsewhere"
	if res := f.mustRun(t); res.deleted != 0 || res.uploaded != 1 {
		t.Errorf("the run did %+v", res)
	}
}

func TestAMissingRemoteFolderFails(t *testing.T) {
	f := newFixture(t, config.SyncPush, false)
	f.job.sync.RemoteFolder = "nowhere"
	if _, err := f.run(t); err == nil || !strings.Contains(err.Error(), `"nowhere"`) {
		t.Errorf("the run answered %v", err)
	}
}

// In a vault, the server holds ciphertext, and the sizes and times a run
// compares are those of the cleartext.
func TestPushIntoAVault(t *testing.T) {
	skipWithout(t, features.Cryptomator)
	f := newFixture(t, config.SyncPush, true)
	f.job.server.VaultPath = "vault"
	f.job.server.VaultPassword = "pw"
	f.job.sync.RemoteFolder = ""
	login, err := loginOf(f.job.server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.CreateVault(context.Background(), login, f.job.ssh); err != nil {
		t.Fatal(err)
	}
	f.writeLocal(t, "deep/a.txt", "secret content", then)

	if res := f.mustRun(t); res.uploaded != 1 {
		t.Errorf("the first run did %+v", res)
	}
	if res := f.mustRun(t); res.uploaded != 0 || res.deleted != 0 {
		t.Errorf("a second run did %+v", res)
	}
	// what the server holds is not the file
	found := false
	_ = filepath.WalkDir(filepath.Join(f.host.Dir, "vault"), func(at string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if body, _ := os.ReadFile(at); strings.Contains(string(body), "secret content") {
				found = true
			}
		}
		return nil
	})
	if found {
		t.Error("the vault holds the cleartext")
	}
}
