package httpd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

// storedRegistry is a stored server for the registry of a test server,
// logged in to as the pushing account, which it admits.
func storedRegistry(source *testServer, name string) config.Server {
	return config.Server{Name: name, Type: config.ServerTypeRegistry, URL: source.url(""),
		Auth: config.RegistryAuthBasic, Username: pusher, Password: pusherPassword, AllowedUsers: []string{pusher}}
}

func browseRegistry(t *testing.T, local *testServer, session *http.Cookie, server, path string) (int, registryBrowseJSON, string) {
	t.Helper()
	var view registryBrowseJSON
	var message string
	res, data := transferRequest(t, local, http.MethodPost, "/?go-fs=registry-browse", session,
		map[string]string{"server": server, "path": path})
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatalf("the browse answered %s: %v", data, err)
		}
	} else {
		message = string(data)
	}
	return res.StatusCode, view, message
}

func browsed(view registryBrowseJSON) []string {
	var names []string
	for _, entry := range view.Entries {
		name := entry.Name
		if entry.Dir {
			name += "/"
		}
		names = append(names, name)
	}
	return names
}

// A stored registry is browsed in the Pull image dialog, and an image of it
// is checked and pulled with the login it stores, which the page never sees.
func TestRegistryPullsFromAStoredRegistry(t *testing.T) {
	source := newRegistryServer(t, func(cfg *httpConfig) { cfg.RegistryAnonymousRead = false })
	source.pushImage(t, "team/app", "1.0", "linux/arm64", "b")
	source.pushImage(t, "team/app", "1.0", "linux/amd64", "a")
	source.pushImage(t, "tool", "latest", "linux/arm/v7", "c")
	index := tagDigest(t, source, "team/app", "1.0")

	hidden := storedRegistry(source, "hidden")
	hidden.AllowedUsers = nil
	local := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.Servers = []config.Server{storedRegistry(source, "mirror"), hidden}
	})
	remote.ForgetCatalogs()
	ci := login(t, local, "/", pusher, pusherPassword)

	res, page := transferRequest(t, local, http.MethodGet, "/?go-fs=registry", ci, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), `<option value="mirror">mirror</option>`) ||
		strings.Contains(string(page), `<option value="hidden">`) {
		t.Errorf("the registry page answered %d without the stored registry offered", res.StatusCode)
	}

	status, view, message := browseRegistry(t, local, ci, "mirror", "/")
	if status != http.StatusOK || !reflect.DeepEqual(browsed(view), []string{"team/", "tool/"}) || view.Parent != "" {
		t.Fatalf("the top of the registry answered %d: %+v %s", status, view, message)
	}
	status, view, _ = browseRegistry(t, local, ci, "mirror", "/team/app")
	if status != http.StatusOK || !reflect.DeepEqual(browsed(view), []string{"1.0"}) || view.Parent != "/team" {
		t.Errorf("the repository answered %d: %+v", status, view)
	}
	if status, _, _ = browseRegistry(t, local, ci, "mirror", "/nothing"); status != http.StatusNotFound {
		t.Errorf("a folder that is not there answered %d", status)
	}
	if status, _, _ = browseRegistry(t, local, ci, "hidden", "/"); status != http.StatusNotFound {
		t.Errorf("a registry that does not admit the account answered %d", status)
	}
	if status, _, _ = browseRegistry(t, local, nil, "mirror", "/"); status != http.StatusForbidden {
		t.Errorf("a browse without a session answered %d", status)
	}

	var found registryPullCheckJSON
	status = checkTransfer(t, local, "/?go-fs=registry-pull-check", ci,
		map[string]string{"server": "mirror", "reference": "team/app:1.0"}, &found)
	if status != http.StatusOK {
		t.Fatalf("the check answered %d", status)
	}
	if choiceKeys(found.Platforms) != "linux/amd64 linux/arm64" || found.Digest != index.String() ||
		found.Local != "team/app:1.0" {
		t.Errorf("the check found %+v", found)
	}
	for _, bad := range []string{"team/app", "Team/app:1.0", "team/app:bad tag"} {
		var said string
		if status := checkTransfer(t, local, "/?go-fs=registry-pull-check", ci,
			map[string]string{"server": "mirror", "reference": bad}, &said); status != http.StatusBadRequest {
			t.Errorf("the reference %q answered %d: %s", bad, status, said)
		}
	}

	job := startTransfer(t, local, "/?go-fs=registry-pull", ci, map[string]string{"server": "mirror",
		"reference": "team/app:1.0", "digest": found.Digest, "platforms": "linux/arm64"})
	if job.State != jobDone {
		t.Fatalf("the pull ended %s: %s", job.State, job.Message)
	}
	if got := tagDigest(t, local, "team/app", "1.0"); got == "" || got == index {
		t.Errorf("the pull of one platform is tagged %s, the index being %s", got, index)
	}
	local.logs.mu.Lock()
	for _, record := range local.logs.records {
		for key, value := range record {
			// a handler writes a value as it resolves
			if strings.Contains(slog.AnyValue(value).Resolve().String(), pusherPassword) {
				t.Errorf("the record %v holds the password in %s", record["msg"], key)
			}
		}
	}
	local.logs.mu.Unlock()
}

// A stored registry is not something a file is fetched from or sent to.
func TestStoredRegistryIsNoFileServer(t *testing.T) {
	source := newRegistryServer(t, nil)
	source.pushImage(t, "app", "1.0", "linux/amd64", "a")
	stored := storedRegistry(source, "mirror")
	stored.AllowedUsers = nil
	view := sendViewOf([]config.Server{stored})
	if len(view.Servers) != 0 {
		t.Errorf("Send File offers %v", view.Servers)
	}
	for _, protocol := range view.Protocols {
		if protocol.Registry {
			t.Errorf("Send File offers the protocol %s", protocol.ID)
		}
	}
	if rights := remoteRights(credential{user: &account{perms: config.Permissions{
		FileCreate: true, FileRetrieve: true, FileOverwrite: true, FolderCreate: true}}}, stored); rights.RemoteFetch ||
		rights.RemoteFetchFolder || rights.Upload {
		t.Errorf("the page of a stored registry offers %+v", rights)
	}
}

// A stored registry that lists no catalog is browsed by the repository named.
func TestStoredRegistryWithoutACatalog(t *testing.T) {
	hub := remotetest.NewRegistryHost(t, "", "", "", "")
	hub.NoCatalog.Store(true)
	hub.Tag("library/alpine", "3.20", "{}")
	local := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.Servers = []config.Server{{Name: "hub", Type: config.ServerTypeRegistry, URL: hub.URL,
			AllowedUsers: []string{pusher}}}
	})
	remote.ForgetCatalogs()
	ci := login(t, local, "/", pusher, pusherPassword)
	status, view, message := browseRegistry(t, local, ci, "hub", "/")
	if status != http.StatusOK || !view.NoCatalog || len(view.Entries) != 0 {
		t.Errorf("the top answered %d: %+v %s", status, view, message)
	}
	status, view, message = browseRegistry(t, local, ci, "hub", "/library/alpine")
	if status != http.StatusOK || !reflect.DeepEqual(browsed(view), []string{"3.20"}) {
		t.Errorf("the repository named answered %d: %+v %s", status, view, message)
	}
}

// The page of a stored registry lists its folders and tags, and one that
// lists no catalog offers to open a repository by its name.
func TestStoredRegistryPage(t *testing.T) {
	hub := remotetest.NewRegistryHost(t, "", "", "", "")
	hub.NoCatalog.Store(true)
	hub.Tag("library/alpine", "3.20", `{"schemaVersion":2}`)
	local := newServer(t, func(cfg *httpConfig) {
		cfg.Servers = []config.Server{{Name: "hub", Type: config.ServerTypeRegistry, URL: hub.URL,
			AllowedUsers: []string{"john"}}}
	})
	remote.ForgetCatalogs()
	session := login(t, local, "/", "john", "doe")
	page := map[string]string{"Accept": "text/html"}

	res := remoteDo(t, local, http.MethodGet, remoteURL("hub", "/"), session, nil, page)
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `class="by-name"`) ||
		!strings.Contains(body, "open one by its name") {
		t.Errorf("the top of the registry answered %d", res.StatusCode)
	}
	res = remoteDo(t, local, http.MethodGet, remoteURL("hub", "library/alpine"), session, nil, page)
	body = bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "3.20") || strings.Contains(body, `data-do="remote-fetch"`) {
		t.Errorf("the repository answered %d", res.StatusCode)
	}
	res = remoteDo(t, local, http.MethodGet, remoteURL("hub", "library/alpine/3.20"), session, nil, nil)
	if body := bodyOf(t, res); res.StatusCode != http.StatusOK || body != `{"schemaVersion":2}` {
		t.Errorf("the tag answered %d: %s", res.StatusCode, body)
	}
}
