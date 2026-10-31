package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

// withSyncServer stores an SFTP server named backup, with a folder photos
// on it and one locally, the way the dialog of a server does.
func withSyncServer(t *testing.T) (*remotetest.SFTPHost, string, *httptest.Server) {
	t.Helper()
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	if err := os.MkdirAll(filepath.Join(host.Dir, "photos", "2025"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := testConfig(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "photos", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, front := testServer(t, path)
	status, body := postServer(t, front, ActionServerSave,
		serverSaveBody{Server: serverOn(host, "backup", "secret"), HostKey: host.Fingerprint})
	if status != http.StatusOK {
		t.Fatalf("the server was not stored: %d %s", status, body)
	}
	return host, path, front
}

func photosSync() syncJSON {
	return syncJSON{Name: "photos", Enabled: true, Protocol: config.SyncProtocolRsync, Server: "backup",
		RemoteFolder: "photos/", LocalFolder: "photos", Direction: config.SyncBoth, Delete: true,
		At: "02:30", Days: []string{"mon", "fri"}}
}

func TestSyncSaveAddsAndEdits(t *testing.T) {
	_, path, front := withSyncServer(t)

	status, body := postServer(t, front, ActionSyncSave, syncSaveBody{Sync: photosSync()})
	if status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	cfg := loadRepositories(t, path)
	if len(cfg.Syncs) != 1 {
		t.Fatalf("syncs = %+v", cfg.Syncs)
	}
	s := cfg.Syncs[0]
	if s.RemoteFolder != "photos" || s.LocalFolder != "/photos" || s.At != "02:30" ||
		!slices.Equal(s.Days, []string{"mon", "fri"}) || !s.Enabled || !s.Delete {
		t.Errorf("stored %+v", s)
	}

	// an edit replaces it, at an interval now
	edited := photosSync()
	edited.At, edited.Days, edited.Every = "", nil, "2h"
	edited.Direction = config.SyncPush
	if status, body := postServer(t, front, ActionSyncSave, syncSaveBody{Was: "photos", Sync: edited}); status != http.StatusOK {
		t.Fatalf("edit: %d %s", status, body)
	}
	s = loadRepositories(t, path).Syncs[0]
	if s.Every != "2h" || s.At != "" || len(s.Days) != 0 || s.Direction != config.SyncPush {
		t.Errorf("edited %+v", s)
	}

	// the state carries the syncs and what the dialog offers
	got := get(t, front)
	if _, ok := got.Syncs["photos"]; !ok || len(got.SyncProtocols) == 0 ||
		!slices.Equal(got.SyncProtocols[0].ServerTypes, []string{config.ServerTypeSFTP}) {
		t.Errorf("the state carries %+v, %+v", got.Syncs, got.SyncProtocols)
	}
}

func TestSyncSaveRefuses(t *testing.T) {
	_, path, front := withSyncServer(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*syncJSON)
		status int
		want   string
	}{
		{"no name", func(s *syncJSON) { s.Name = " " }, http.StatusBadRequest, "name the sync"},
		{"unknown protocol", func(s *syncJSON) { s.Protocol = "ftp" }, http.StatusBadRequest, "not one this build runs"},
		{"unknown server", func(s *syncJSON) { s.Server = "nowhere" }, http.StatusBadRequest, "not a stored server"},
		{"missing local folder", func(s *syncJSON) { s.LocalFolder = "/missing" }, http.StatusBadRequest, "missing"},
		{"no schedule", func(s *syncJSON) { s.At, s.Days = "", nil }, http.StatusBadRequest, "set every or at"},
		{"missing remote folder", func(s *syncJSON) { s.RemoteFolder = "gone" }, http.StatusBadRequest, `"gone"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := photosSync()
			c.change(&s)
			status, body := postServer(t, front, ActionSyncSave, syncSaveBody{Sync: s})
			if status != c.status || !strings.Contains(body, c.want) {
				t.Errorf("answered %d: %s, want %d with %q", status, body, c.status, c.want)
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a refused sync changed the file")
	}
}

func TestSyncBrowse(t *testing.T) {
	host, _, front := withSyncServer(t)

	browse := func(body syncBrowseBody) syncFolderJSON {
		t.Helper()
		status, answer := postServer(t, front, ActionSyncBrowse, body)
		if status != http.StatusOK {
			t.Fatalf("browse %+v: %d %s", body, status, answer)
		}
		var folder syncFolderJSON
		if err := json.Unmarshal([]byte(answer), &folder); err != nil {
			t.Fatal(err)
		}
		return folder
	}

	top := browse(syncBrowseBody{Remote: true, Server: "backup"})
	if top.Path != remotetest.Path(host.Dir) || !slices.Contains(top.Folders, "photos") {
		t.Errorf("the top of the server is %+v", top)
	}
	inner := browse(syncBrowseBody{Remote: true, Server: "backup", Path: top.Path + "/photos"})
	if !slices.Equal(inner.Folders, []string{"2025"}) || inner.Parent != top.Path {
		t.Errorf("a folder of the server is %+v", inner)
	}

	local := browse(syncBrowseBody{})
	if local.Path != "/" || local.Parent != "" || !slices.Contains(local.Folders, "photos") {
		t.Errorf("the top of the served folder is %+v", local)
	}
	local = browse(syncBrowseBody{Path: "/photos"})
	if !slices.Equal(local.Folders, []string{"inner"}) || local.Parent != "/" {
		t.Errorf("a served folder is %+v", local)
	}

	if status, body := postServer(t, front, ActionSyncBrowse, syncBrowseBody{Path: "/../.."}); status == http.StatusOK {
		t.Errorf("a path out of the served folder answered %d: %s", status, body)
	}
	if status, body := postServer(t, front, ActionSyncBrowse, syncBrowseBody{Path: "/missing"}); status != http.StatusNotFound {
		t.Errorf("a missing folder answered %d: %s", status, body)
	}
}

func TestSyncRunOfASyncThatIsNotRunning(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)
	status, body := postServer(t, front, ActionSyncRun, syncRunBody{Name: "nothing"})
	if status != http.StatusConflict || !strings.Contains(body, "is not running") {
		t.Errorf("answered %d: %s", status, body)
	}
}

// A server that is renamed takes its syncs with it.
func TestServerRenameKeepsItsSyncs(t *testing.T) {
	host, path, front := withSyncServer(t)
	if status, body := postServer(t, front, ActionSyncSave, syncSaveBody{Sync: photosSync()}); status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	renamed := serverOn(host, "archive", "")
	status, body := postServer(t, front, ActionServerSave,
		serverSaveBody{Was: "backup", Server: renamed, HostKey: host.Fingerprint})
	if status != http.StatusOK {
		t.Fatalf("rename: %d %s", status, body)
	}
	if s := loadRepositories(t, path).Syncs[0]; s.Server != "archive" {
		t.Errorf("the sync is of %q", s.Server)
	}
}
