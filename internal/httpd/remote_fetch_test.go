package httpd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

// remoteFetchOf is the link that fetches what a link of the page of a server
// downloads.
func remoteFetchOf(link string) string {
	return strings.Replace(link, "="+actionRemote+"&", "="+actionRemoteFetch+"&", 1)
}

// localBrowse lists a folder of the served tree as the dialog does.
func localBrowse(t *testing.T, server *testServer, folder string, session *http.Cookie) (int, localFolderJSON) {
	t.Helper()
	res, data := transferRequest(t, server, http.MethodGet, "/?go-fs=local-browse&path="+url.QueryEscape(folder), session, nil)
	var view localFolderJSON
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, view
}

// remoteFetched starts a fetch of a link of a server into a folder and waits
// for it to end.
func remoteFetched(t *testing.T, server *testServer, link, folder string, session *http.Cookie) registryJobJSON {
	t.Helper()
	res, data := transferRequest(t, server, http.MethodPost, remoteFetchOf(link), session, remoteFetchBody{Folder: folder})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("the fetch answered %d: %s", res.StatusCode, data)
	}
	var job registryJobJSON
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	if job.Kind != jobFetch {
		t.Fatalf("the job is a %q", job.Kind)
	}
	return waitForFetch(t, server, "/", job.ID, session)
}

func TestLocalBrowseListsTheFoldersOfTheServedTree(t *testing.T) {
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	server, session := remoteServer(t, host, nil)
	server.write(t, "zeta.txt", "z")
	server.mkdir(t, "beta")
	server.write(t, "beta/inside.txt", "in")
	server.mkdir(t, "Alpha")

	status, view := localBrowse(t, server, "/", session)
	if status != http.StatusOK {
		t.Fatalf("the root answered %d", status)
	}
	if view.Path != "/" || view.Parent != "" || !view.Writable {
		t.Errorf("the root is %+v", view)
	}
	var names []string
	for _, entry := range view.Entries {
		names = append(names, entry.Name)
	}
	if got := strings.Join(names, ","); got != "Alpha,beta,zeta.txt" {
		t.Errorf("the root lists %s", got)
	}
	if !view.Entries[0].Dir || view.Entries[2].Dir || view.Entries[2].Size != 1 {
		t.Errorf("the entries are %+v", view.Entries)
	}

	status, view = localBrowse(t, server, "/beta/", session)
	if status != http.StatusOK || view.Path != "/beta" || view.Parent != "/" ||
		len(view.Entries) != 1 || view.Entries[0].Name != "inside.txt" {
		t.Errorf("a folder below answered %d: %+v", status, view)
	}
	if status, _ := localBrowse(t, server, "/../..", session); status != http.StatusNotFound {
		t.Errorf("a path above the root answered %d", status)
	}
	if status, _ := localBrowse(t, server, "/zeta.txt", session); status != http.StatusNotFound {
		t.Errorf("a file answered %d", status)
	}
	if status, _ := localBrowse(t, server, "/nowhere", session); status != http.StatusNotFound {
		t.Errorf("a folder that is not there answered %d", status)
	}
}

func TestLocalBrowseIsForASessionAndSaysWhereItMayStore(t *testing.T) {
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	server, _ := remoteServer(t, host, func(cfg *httpConfig) {
		cfg.Users = append(cfg.Users, readOnlyUser("reader", "pw"),
			config.User{Username: "blind", Password: "pw", HTTP: true, Paths: []string{"^/.*"}})
	})

	if status, _ := localBrowse(t, server, "/", nil); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("without a session the root answered %d", status)
	}
	if res := basic(t, server, http.MethodGet, "/?go-fs=local-browse&path=/", "john", "doe", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("a header answered %d", res.StatusCode)
	}
	reader := login(t, server, "/", "reader", "pw")
	if status, view := localBrowse(t, server, "/", reader); status != http.StatusOK || view.Writable {
		t.Errorf("a reader was answered %d: %+v", status, view)
	}
	// a folder that needs an account is listed as its page is: to one that
	// may read
	server.mkdir(t, "private")
	blind := login(t, server, "/", "blind", "pw")
	if status, _ := localBrowse(t, server, "/private", blind); status != http.StatusForbidden {
		t.Errorf("an account that may not read answered %d", status)
	}
	if status, _ := localBrowse(t, server, "/private", reader); status != http.StatusOK {
		t.Errorf("an account that may read answered %d", status)
	}
}

func TestRemoteFetchStoresAFileOfTheServer(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "deep/report.txt", "0123456789")
	server, session := remoteServer(t, host, nil)
	server.mkdir(t, "inbox")

	job := remoteFetched(t, server, remoteAt(host, "deep/report.txt"), "/inbox", session)
	expectDone(t, job)
	if got := server.read(t, "inbox/report.txt"); got != "0123456789" {
		t.Errorf("the file stored is %q", got)
	}
	if !strings.Contains(job.Message, "Fetched report.txt") || !strings.Contains(job.Message, "Backup") ||
		!strings.Contains(job.Message, "/inbox/") {
		t.Errorf("the job says %q", job.Message)
	}
	if job.Folder != "/inbox/" || job.Name != "report.txt" {
		t.Errorf("the job is of %q in %q", job.Name, job.Folder)
	}
	if remoteFile(host, "deep/report.txt") != "0123456789" {
		t.Error("the file of the server was changed")
	}
}

func TestRemoteFetchStoresAFileOfAnArtifactory(t *testing.T) {
	host := remotetest.NewArtifactoryHost(t, "the-token", "libs")
	host.Write(t, "libs/org/lib.jar", "jar-content")
	server, session := artifactoryServer(t, host)

	expectDone(t, remoteFetched(t, server, remoteURL("Artifacts", "/libs/org/lib.jar"), "/", session))
	if got := server.read(t, "lib.jar"); got != "jar-content" {
		t.Errorf("the file stored is %q", got)
	}
}

func TestRemoteFetchReplacesOnlyWithTheOverwriteRight(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "data.bin", "new")
	host.Write(t, "folder/inside.txt", "in")
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		kim := fullUser("kim", "pw")
		kim.AllowUserFileOverwrite = new(false)
		cfg.Users = append(cfg.Users, kim)
	})
	server.write(t, "data.bin", "old")

	kim := login(t, server, "/", "kim", "pw")
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "data.bin"), "/", kim), "already a file")
	if got := server.read(t, "data.bin"); got != "old" {
		t.Errorf("the file was replaced: %q", got)
	}
	expectDone(t, remoteFetched(t, server, remoteAt(host, "data.bin"), "/", session))
	if got := server.read(t, "data.bin"); got != "new" {
		t.Errorf("the file was not replaced: %q", got)
	}
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "missing.txt"), "/", session), "there is no")

	// a folder that would replace a file is refused before anything of it
	// is stored
	host.Write(t, "folder/another.txt", "another")
	server.write(t, "folder/inside.txt", "old")
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "folder"), "/", kim), "already a file")
	if got := server.read(t, "folder/inside.txt"); got != "old" {
		t.Errorf("the file was replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(server.base, "folder", "another.txt")); err == nil {
		t.Error("a refused fetch stored a file of the folder")
	}
}

func TestRemoteFetchKeepsToTheUploadLimit(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "big.bin", "01234567890123456789")
	server, session := remoteServer(t, host, func(cfg *httpConfig) { cfg.MaxUploadSize = 10 })

	expectFailed(t, remoteFetched(t, server, remoteAt(host, "big.bin"), "/", session), "larger than the maximum")
	if _, err := os.Stat(filepath.Join(server.base, "big.bin")); err == nil {
		t.Error("the file was stored")
	}
}

func TestRemoteFetchRefusesWhatIsNotAFetch(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "a.txt", "a")
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		cfg.Users = append(cfg.Users, readOnlyUser("reader", "pw"))
	})
	server.write(t, "file.txt", "f")
	link := remoteFetchOf(remoteAt(host, "a.txt"))
	reader := login(t, server, "/", "reader", "pw")

	for _, c := range []struct {
		name    string
		method  string
		link    string
		session *http.Cookie
		folder  string
		headers []string
		want    int
	}{
		{"no session", http.MethodPost, link, nil, "/", nil, http.StatusUnauthorized},
		{"a reader", http.MethodPost, link, reader, "/", nil, http.StatusForbidden},
		{"another site", http.MethodPost, link, session, "/", []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"a GET", http.MethodGet, link, session, "/", nil, http.StatusMethodNotAllowed},
		{"a server that is not stored", http.MethodPost, remoteFetchOf(remoteURL("Nowhere", "/a.txt")), session, "/", nil, http.StatusNotFound},
		{"no file", http.MethodPost, remoteFetchOf(remoteURL("Backup", "")), session, "/", nil, http.StatusBadRequest},
		{"no folder", http.MethodPost, link, session, "", nil, http.StatusBadRequest},
		{"a folder above the root", http.MethodPost, link, session, "/../..", nil, http.StatusBadRequest},
		{"a file for a folder", http.MethodPost, link, session, "/file.txt", nil, http.StatusBadRequest},
		{"a folder that is not there", http.MethodPost, link, session, "/nowhere", nil, http.StatusBadRequest},
	} {
		res, data := transferRequest(t, server, c.method, c.link, c.session, remoteFetchBody{Folder: c.folder}, c.headers...)
		if res.StatusCode != c.want && !(c.want == http.StatusUnauthorized && res.StatusCode == http.StatusForbidden) {
			t.Errorf("%s answered %d, want %d: %s", c.name, res.StatusCode, c.want, data)
		}
	}
	if n := host.Logins.Load(); n != 0 {
		t.Errorf("%d logins were offered for fetches that were refused", n)
	}
	if _, err := os.Stat(filepath.Join(server.base, "a.txt")); err == nil {
		t.Error("a refused fetch stored the file")
	}
}

func TestRemotePageOffersFetchWhereTheAccountMayCreate(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "a.txt", "a")
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		cfg.Users = append(cfg.Users, readOnlyUser("reader", "pw"))
	})

	body := bodyOf(t, remoteDo(t, server, http.MethodGet, remoteURL("Backup", ""), session, nil, nil))
	for _, want := range []string{`data-do="remote-fetch"`, `id="remote-fetch-dialog"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	reader := login(t, server, "/", "reader", "pw")
	body = bodyOf(t, remoteDo(t, server, http.MethodGet, remoteURL("Backup", ""), reader, nil, nil))
	if strings.Contains(body, `data-do="remote-fetch"`) || strings.Contains(body, `id="remote-fetch-dialog"`) {
		t.Error("the page of a reader offers Fetch")
	}
	// and the listing of the served tree offers it nowhere
	if strings.Contains(listingOf(t, server, session), `data-do="remote-fetch"`) {
		t.Error("the listing of the served tree offers Fetch from a server")
	}
}

// remoteMeasured measures a link of a server as the dialog does.
func remoteMeasured(t *testing.T, server *testServer, link string, session *http.Cookie) (int, remoteMeasureJSON) {
	t.Helper()
	measure := strings.Replace(link, "="+actionRemote+"&", "="+actionRemoteMeasure+"&", 1)
	res, data := transferRequest(t, server, http.MethodGet, measure, session, nil)
	var view remoteMeasureJSON
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &view); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, view
}

// fullDisk makes every disk have free bytes free, until the test ends.
func fullDisk(t *testing.T, free int64) {
	t.Helper()
	previous := freeSpace
	freeSpace = func(string) int64 { return free }
	t.Cleanup(func() { freeSpace = previous })
}

func TestRemoteFetchStoresAFolderWithAllBelowIt(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "aaaa")
	host.Write(t, "photos/2024/b.jpg", "bb")
	host.Write(t, "photos/2024/deep/c.jpg", "c")
	if err := os.MkdirAll(filepath.Join(host.Dir, "photos", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	server, session := remoteServer(t, host, nil)
	server.mkdir(t, "inbox")
	server.write(t, "inbox/photos/kept.txt", "kept")

	job := remoteFetched(t, server, remoteAt(host, "photos"), "/inbox", session)
	expectDone(t, job)
	for name, want := range map[string]string{
		"inbox/photos/a.jpg":           "aaaa",
		"inbox/photos/2024/b.jpg":      "bb",
		"inbox/photos/2024/deep/c.jpg": "c",
		"inbox/photos/kept.txt":        "kept",
	} {
		if got := server.read(t, name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(server.base, "inbox", "photos", "empty")); err != nil || !info.IsDir() {
		t.Errorf("the empty folder was not made: %v", err)
	}
	if !strings.Contains(job.Message, "Fetched 3 files") || !strings.Contains(job.Message, "of photos from Backup") {
		t.Errorf("the job says %q", job.Message)
	}
	if job.Name != "photos/" || job.Folder != "/inbox/" || job.BlobsTotal != 3 || job.BlobsDone != 3 ||
		job.BytesTotal != 7 || job.BytesDone != 7 {
		t.Errorf("the job is %+v", job)
	}
}

func TestRemoteFetchStoresAFolderOfAnArtifactory(t *testing.T) {
	host := remotetest.NewArtifactoryHost(t, "the-token", "libs")
	host.Write(t, "libs/org/lib.jar", "jar-content")
	host.Write(t, "libs/org/sub/doc.txt", "doc")
	server, session := artifactoryServer(t, host)

	expectDone(t, remoteFetched(t, server, remoteURL("Artifacts", "/libs/org"), "/", session))
	if got := server.read(t, "org/lib.jar"); got != "jar-content" {
		t.Errorf("the file stored is %q", got)
	}
	if got := server.read(t, "org/sub/doc.txt"); got != "doc" {
		t.Errorf("the file below stored is %q", got)
	}
}

func TestRemoteMeasureCountsWhatAFetchStores(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "aaaa")
	host.Write(t, "photos/2024/b.jpg", "bb")
	host.Write(t, "photos/2024/deep/c.jpg", "c")
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		cfg.Users = append(cfg.Users, readOnlyUser("reader", "pw"))
	})

	status, view := remoteMeasured(t, server, remoteAt(host, "photos"), session)
	if status != http.StatusOK {
		t.Fatalf("the folder answered %d", status)
	}
	want := remoteMeasureJSON{Name: "photos", Dir: true, Files: 3, Folders: 2, Bytes: 7, Largest: 4}
	if view != want {
		t.Errorf("the folder measures %+v, want %+v", view, want)
	}
	status, view = remoteMeasured(t, server, remoteAt(host, "photos/a.jpg"), session)
	if want := (remoteMeasureJSON{Name: "a.jpg", Files: 1, Bytes: 4, Largest: 4}); status != http.StatusOK || view != want {
		t.Errorf("the file answered %d: %+v", status, view)
	}
	if status, _ := remoteMeasured(t, server, remoteAt(host, "nowhere"), session); status != http.StatusNotFound {
		t.Errorf("what is not there answered %d", status)
	}
	if status, _ := remoteMeasured(t, server, remoteAt(host, "photos"), nil); status != http.StatusUnauthorized &&
		status != http.StatusForbidden {
		t.Errorf("without a session the folder answered %d", status)
	}
	// measuring reads no more than browsing the server shows
	reader := login(t, server, "/", "reader", "pw")
	if status, _ := remoteMeasured(t, server, remoteAt(host, "photos"), reader); status != http.StatusOK {
		t.Errorf("a reader was answered %d", status)
	}
}

func TestRemoteMeasureIsRefusedWhereTheServerDoesNotAllowDownloads(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "aaaa")
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		cfg.Servers[0].AllowDownload = new(false)
	})
	if status, _ := remoteMeasured(t, server, remoteAt(host, "photos"), session); status != http.StatusForbidden {
		t.Errorf("the folder answered %d", status)
	}
}

func TestRemoteFetchMakesSureItFits(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "0123456789")
	host.Write(t, "photos/b.jpg", "0123456789")
	host.Write(t, "single.bin", "0123456789")
	server, session := remoteServer(t, host, nil)
	server.mkdir(t, "inbox")

	fullDisk(t, 15)
	if _, view := localBrowse(t, server, "/inbox", session); view.Free != 15 {
		t.Errorf("the folder says %d bytes are free", view.Free)
	}
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "photos"), "/inbox", session), "not enough room in /inbox/")
	if _, err := os.Stat(filepath.Join(server.base, "inbox", "photos")); err == nil {
		t.Error("a folder that does not fit was made")
	}
	// a single file of the folder fits, and so does the file alone
	expectDone(t, remoteFetched(t, server, remoteAt(host, "single.bin"), "/inbox", session))

	fullDisk(t, 5)
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "single.bin"), "/", session), "not enough room in /")
	if _, err := os.Stat(filepath.Join(server.base, "single.bin")); err == nil {
		t.Error("a file that does not fit was stored")
	}
}

func TestRemoteFetchOfAFolderNeedsTheRightToCreateFolders(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "a")
	server, _ := remoteServer(t, host, func(cfg *httpConfig) {
		flat := fullUser("adder", "pw")
		flat.AllowUserFolderCreate = new(false)
		cfg.Users = append(cfg.Users, flat)
	})

	flat := login(t, server, "/", "adder", "pw")
	if _, view := localBrowse(t, server, "/", flat); !view.Writable || view.Mkdir {
		t.Errorf("the root says %+v", view)
	}
	expectFailed(t, remoteFetched(t, server, remoteAt(host, "photos"), "/", flat), "may not create the folder")
	if _, err := os.Stat(filepath.Join(server.base, "photos")); err == nil {
		t.Error("the folder was made")
	}
	body := bodyOf(t, remoteDo(t, server, http.MethodGet, remoteURL("Backup", ""), flat, nil, nil))
	if !strings.Contains(body, `data-do="remote-fetch"`) || strings.Contains(body, `data-folders="1"`) {
		t.Error("the page offers Fetch on a folder to an account that may not create folders")
	}
}

func TestRemoteFetchLeavesOutAFolderItCannotRead(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a folder cannot be made unreadable here")
	}
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "photos/a.jpg", "a")
	host.Write(t, "photos/locked/b.jpg", "b")
	locked := filepath.Join(host.Dir, "photos", "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	server, session := remoteServer(t, host, nil)

	if _, view := remoteMeasured(t, server, remoteAt(host, "photos"), session); view.Files != 1 || view.Skipped != 1 {
		t.Errorf("the folder measures %+v", view)
	}
	job := remoteFetched(t, server, remoteAt(host, "photos"), "/", session)
	expectDone(t, job)
	if !strings.Contains(job.Message, "1 folder could not be read") {
		t.Errorf("the job says %q", job.Message)
	}
	if got := server.read(t, "photos/a.jpg"); got != "a" {
		t.Errorf("the file stored is %q", got)
	}
}
