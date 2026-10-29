package httpd

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
)

// explorer is the User-Agent of the WebDAV client of Windows Explorer.
const explorer = "Microsoft-WebDAV-MiniRedir/10.0.22631"

// davServer is newServer with WebDAV on.
func davServer(t *testing.T, tune func(*httpConfig)) *testServer {
	t.Helper()
	skipWithout(t, features.WebDAV)
	return newServer(t, func(cfg *httpConfig) {
		cfg.WebDAV = true
		if tune != nil {
			tune(cfg)
		}
	})
}

// dav sends a request as Explorer does, with Basic credentials when a name is
// given.
func dav(t *testing.T, server *testServer, method, path, name, password string,
	headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", explorer)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if name != "" {
		req.SetBasicAuth(name, password)
	}
	return do(t, req)
}

// With http.webdav off nothing changes: WebDAV's own methods are not
// answered, and the methods it shares keep their meaning for every client.
func TestWebDAVOffAnswersNothing(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "box/a.txt", "a")
	if res := dav(t, server, methodPropfind, "/", "john", "doe", map[string]string{"Depth": "1"}, ""); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PROPFIND: status %d, want 405", res.StatusCode)
	}
	if res := dav(t, server, http.MethodDelete, "/box", "john", "doe", nil, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE of a folder with a file in it: status %d, want 404", res.StatusCode)
	}
	if !exists(server, "box/a.txt") {
		t.Error("the folder was removed with WebDAV off")
	}
}

// OPTIONS says the server speaks WebDAV with locks, which is what Finder
// mounts a folder writable for, and Office what it saves to.
func TestWebDAVOptions(t *testing.T) {
	server := davServer(t, nil)
	res := dav(t, server, http.MethodOptions, "/", "", "", nil, "")
	if res.StatusCode != http.StatusOK || res.Header.Get("DAV") != "1, 2" || res.Header.Get("MS-Author-Via") != "DAV" {
		t.Errorf("status %d DAV %q MS-Author-Via %q", res.StatusCode, res.Header.Get("DAV"), res.Header.Get("MS-Author-Via"))
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("OPTIONS answered a cross site preflight")
	}
}

// PROPFIND lists one level, and refuses to walk the whole tree.
func TestWebDAVPropfind(t *testing.T) {
	server := davServer(t, nil)
	server.write(t, "a.txt", "a")
	server.write(t, "sub/b.txt", "b")

	res := dav(t, server, methodPropfind, "/", "", "", map[string]string{"Depth": "1"}, "")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{"<D:href>/a.txt</D:href>", "<D:href>/sub/</D:href>", "text/plain"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing has no %s: %s", want, body)
		}
	}
	if strings.Contains(body, "b.txt") {
		t.Errorf("Depth 1 went deeper: %s", body)
	}

	for _, depth := range []string{"infinity", ""} {
		headers := map[string]string{}
		if depth != "" {
			headers["Depth"] = depth
		}
		res := dav(t, server, methodPropfind, "/", "", "", headers, "")
		if body := bodyOf(t, res); res.StatusCode != http.StatusForbidden || !strings.Contains(body, "propfind-finite-depth") {
			t.Errorf("Depth %q: status %d: %s", depth, res.StatusCode, body)
		}
	}
}

// A listing leaves out what the account may not reach, and what it may not
// reach is refused, as it is to GET.
func TestWebDAVPropfindKeepsToTheAccount(t *testing.T) {
	server := davServer(t, func(cfg *httpConfig) {
		cfg.MethodsRequireAuth = append(cfg.MethodsRequireAuth, http.MethodGet)
		scoped := readOnlyUser("scoped", "pw")
		scoped.Paths = []string{"^/$", "^/public(/.*)?$"}
		cfg.Users = append(cfg.Users, scoped)
	})
	server.write(t, "public/a.txt", "a")
	server.write(t, "secret/b.txt", "b")

	res := dav(t, server, methodPropfind, "/", "scoped", "pw", map[string]string{"Depth": "1"}, "")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusMultiStatus || !strings.Contains(body, "/public/") {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if strings.Contains(body, "secret") {
		t.Errorf("the listing names a folder the account may not reach: %s", body)
	}
	if res := dav(t, server, methodPropfind, "/secret", "scoped", "pw", map[string]string{"Depth": "0"}, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("PROPFIND of a folder out of reach: status %d, want 403", res.StatusCode)
	}
	if res := dav(t, server, methodPropfind, "/", "", "", map[string]string{"Depth": "1"}, ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("PROPFIND without an account where GET needs one: status %d, want 401", res.StatusCode)
	}
}

// DELETE removes a folder with what is in it for a WebDAV client, and only
// an empty one for everyone else.
func TestWebDAVDeleteIsRecursiveForWebDAVClientsOnly(t *testing.T) {
	server := davServer(t, nil)
	server.write(t, "box/inner/a.txt", "a")

	if res := basic(t, server, http.MethodDelete, "/box", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE from a script: status %d, want 404", res.StatusCode)
	}
	if !exists(server, "box/inner/a.txt") {
		t.Fatal("a script removed a folder that is not empty")
	}
	if res := dav(t, server, http.MethodDelete, "/box", "john", "doe", nil, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE from Explorer: status %d, want 204", res.StatusCode)
	}
	if exists(server, "box") {
		t.Error("the folder is still there")
	}
	if record := server.logs.find("webdav delete"); record == nil || record["user"] != "john" {
		t.Errorf("no record of the delete: %v", record)
	}
}

// A recursive DELETE takes the right to delete everything in the folder, at
// a path the account may reach; one thing it may not touch keeps all of it.
func TestWebDAVDeleteNeedsEveryPath(t *testing.T) {
	server := davServer(t, func(cfg *httpConfig) {
		scoped := fullUser("scoped", "pw")
		scoped.Paths = []string{"^/box$", "^/box/open.*"}
		noFolders := fullUser("files", "pw")
		noFolders.AllowUserFolderDelete = new(false)
		cfg.Users = append(cfg.Users, scoped, noFolders)
	})
	server.write(t, "box/open.txt", "open")
	server.write(t, "box/secret.txt", "secret")
	server.write(t, "files/sub/a.txt", "a")

	if res := dav(t, server, http.MethodDelete, "/box", "scoped", "pw", nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE with a file out of reach: status %d, want 403", res.StatusCode)
	}
	if !exists(server, "box/open.txt") || !exists(server, "box/secret.txt") {
		t.Error("a refused DELETE removed something")
	}
	if res := dav(t, server, http.MethodDelete, "/files", "files", "pw", nil, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE of folders without allowUserFolderDelete: status %d, want 403", res.StatusCode)
	}
	if !exists(server, "files/sub/a.txt") {
		t.Error("a refused DELETE removed something")
	}
}

// MOVE moves across folders, and onto a name that is taken when asked to,
// for a WebDAV client; a script still only renames in place.
func TestWebDAVMove(t *testing.T) {
	server := davServer(t, nil)
	server.write(t, "from/a.txt", "a")
	server.write(t, "to/b.txt", "old")
	server.write(t, "from/b.txt", "new")

	if res := move(t, server, "/from/a.txt", "/to/a.txt", "john", "doe"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("MOVE across folders from a script: status %d, want 400", res.StatusCode)
	}
	res := dav(t, server, methodMove, "/from/a.txt", "john", "doe",
		map[string]string{"Destination": server.url("/to/a.txt")}, "")
	if res.StatusCode != http.StatusCreated || server.read(t, "to/a.txt") != "a" || exists(server, "from/a.txt") {
		t.Errorf("MOVE across folders from Explorer: status %d", res.StatusCode)
	}

	res = dav(t, server, methodMove, "/from/b.txt", "john", "doe",
		map[string]string{"Destination": server.url("/to/b.txt"), "Overwrite": "F"}, "")
	if res.StatusCode != http.StatusPreconditionFailed || server.read(t, "to/b.txt") != "old" {
		t.Errorf("MOVE onto a taken name with Overwrite F: status %d", res.StatusCode)
	}
	res = dav(t, server, methodMove, "/from/b.txt", "john", "doe",
		map[string]string{"Destination": server.url("/to/b.txt"), "Overwrite": "T"}, "")
	if res.StatusCode != http.StatusNoContent || server.read(t, "to/b.txt") != "new" {
		t.Errorf("MOVE onto a taken name with Overwrite T: status %d", res.StatusCode)
	}

	// a destination in the source, or holding it, would be removed with it
	server.write(t, "tree/inner/c.txt", "c")
	for _, pair := range [][2]string{{"/tree", "/tree/inner/moved"}, {"/tree/inner", "/tree"}} {
		res := dav(t, server, methodMove, pair[0], "john", "doe",
			map[string]string{"Destination": server.url(pair[1]), "Overwrite": "T"}, "")
		if res.StatusCode != http.StatusForbidden || server.read(t, "tree/inner/c.txt") != "c" {
			t.Errorf("MOVE %s onto %s: status %d", pair[0], pair[1], res.StatusCode)
		}
	}
}

// Renaming to another spelling of the same name, on a filesystem that
// ignores case, renames rather than removing the file as the destination it
// also is.
func TestWebDAVMoveChangesCase(t *testing.T) {
	server := davServer(t, nil)
	if !server.root.CaseInsensitive() {
		t.Skip("the filesystem of the test folder tells case apart")
	}
	server.write(t, "notes.txt", "keep me")
	res := dav(t, server, methodMove, "/notes.txt", "john", "doe",
		map[string]string{"Destination": server.url("/Notes.txt"), "Overwrite": "T"}, "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", res.StatusCode)
	}
	entries, err := os.ReadDir(server.base)
	if err != nil || len(entries) != 1 || entries[0].Name() != "Notes.txt" {
		t.Fatalf("folder holds %v (%v)", entries, err)
	}
	if server.read(t, "Notes.txt") != "keep me" {
		t.Error("the file lost what was in it")
	}
}

// A MOVE that would carry a file out of the account's reach is refused,
// for every file in a folder.
func TestWebDAVMoveNeedsEveryPath(t *testing.T) {
	server := davServer(t, func(cfg *httpConfig) {
		scoped := fullUser("scoped", "pw")
		scoped.Paths = []string{"^/mine(/.*)?$", "^/other$"}
		cfg.Users = append(cfg.Users, scoped)
	})
	server.write(t, "mine/a.txt", "a")
	res := dav(t, server, methodMove, "/mine", "scoped", "pw",
		map[string]string{"Destination": server.url("/other")}, "")
	if res.StatusCode != http.StatusForbidden || !exists(server, "mine/a.txt") || exists(server, "other") {
		t.Errorf("status %d", res.StatusCode)
	}
}

// COPY needs to see what it copies and to create the copies; replacing a
// taken name removes it first, which takes the right to delete it.
func TestWebDAVCopy(t *testing.T) {
	server := davServer(t, func(cfg *httpConfig) {
		maker := readOnlyUser("maker", "pw")
		maker.AllowUserFileCreate = new(true)
		maker.AllowUserFolderCreate = new(true)
		cfg.Users = append(cfg.Users, maker)
	})
	server.write(t, "a.txt", "a")
	server.write(t, "c.txt", "c")
	server.write(t, "tree/inner/d.txt", "d")

	if res := dav(t, server, methodCopy, "/a.txt", "maker", "pw",
		map[string]string{"Destination": server.url("/b.txt")}, ""); res.StatusCode != http.StatusCreated || server.read(t, "b.txt") != "a" {
		t.Errorf("COPY to a free name: status %d", res.StatusCode)
	}
	if res := dav(t, server, methodCopy, "/a.txt", "maker", "pw",
		map[string]string{"Destination": server.url("/c.txt")}, ""); res.StatusCode != http.StatusForbidden || server.read(t, "c.txt") != "c" {
		t.Errorf("COPY onto a taken name without allowUserFileDelete: status %d", res.StatusCode)
	}
	if res := dav(t, server, methodCopy, "/tree", "maker", "pw",
		map[string]string{"Destination": server.url("/copy")}, ""); res.StatusCode != http.StatusCreated || server.read(t, "copy/inner/d.txt") != "d" {
		t.Errorf("COPY of a folder: status %d", res.StatusCode)
	}
	if res := dav(t, server, methodCopy, "/tree", "john", "doe",
		map[string]string{"Destination": server.url("/tree/inner/again")}, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("COPY of a folder into itself: status %d", res.StatusCode)
	}
}

// Explorer and Finder create a file with an empty PUT before they send what
// is in it, so an account that may create files and not replace them can
// fill an empty one; a file with something in it is still not replaced.
func TestWebDAVPutFillsAnEmptyFile(t *testing.T) {
	server := davServer(t, func(cfg *httpConfig) {
		dropper := readOnlyUser("dropper", "pw")
		dropper.AllowUserFileCreate = new(true)
		cfg.Users = append(cfg.Users, dropper)
	})
	for i, step := range []struct {
		body   string
		status int
	}{{"", http.StatusCreated}, {"data", http.StatusCreated}, {"again", http.StatusForbidden}} {
		if res := dav(t, server, http.MethodPut, "/new.txt", "dropper", "pw", nil, step.body); res.StatusCode != step.status {
			t.Fatalf("PUT %d: status %d, want %d", i, res.StatusCode, step.status)
		}
	}
	if server.read(t, "new.txt") != "data" {
		t.Errorf("file holds %q", server.read(t, "new.txt"))
	}
	if record := server.logs.find("webdav upload"); record == nil || record["user"] != "dropper" {
		t.Errorf("no record of the upload: %v", record)
	}
}

// Office saves with the type of its document, which the page's PUT refuses.
func TestWebDAVPutTakesAnyContentType(t *testing.T) {
	server := davServer(t, nil)
	res := dav(t, server, http.MethodPut, "/report.docx", "john", "doe", map[string]string{
		"User-Agent":   "Microsoft Office Word 2014",
		"Content-Type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	}, "document")
	if res.StatusCode != http.StatusCreated || server.read(t, "report.docx") != "document" {
		t.Errorf("status %d", res.StatusCode)
	}
	// a folder that is not there is a conflict, rather than made for the file
	if res := dav(t, server, http.MethodPut, "/missing/a.txt", "john", "doe", nil, "a"); res.StatusCode != http.StatusConflict {
		t.Errorf("PUT into a missing folder: status %d, want 409", res.StatusCode)
	}
}

// An upload whose body breaks off leaves nothing behind, neither at its name
// nor in the staging folder.
func TestWebDAVPutCutShortLeavesNothing(t *testing.T) {
	staging := t.TempDir()
	server := davServer(t, func(cfg *httpConfig) { cfg.UploadStagingFolder = staging })
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.url(""), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(conn, "PUT /cut.txt HTTP/1.1\r\nHost: go-fs\r\nUser-Agent: %s\r\n"+
		"Authorization: Basic %s\r\nContent-Length: 100\r\n\r\n0123456789",
		explorer, base64.StdEncoding.EncodeToString([]byte("john:doe")))
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for server.logs.find("webdav upload failed") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the broken upload was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exists(server, "cut.txt") {
		t.Error("a broken upload left a file")
	}
	if entries, _ := os.ReadDir(staging); len(entries) != 0 {
		t.Errorf("the staging folder holds %v", entries)
	}
}

// A file a WebDAV client has locked is not written by anyone without the
// lock, through WebDAV or the page's own PUT, until it is unlocked.
func TestWebDAVLocks(t *testing.T) {
	server := davServer(t, nil)
	server.write(t, "doc.txt", "original")

	res := dav(t, server, methodLock, "/doc.txt", "john", "doe",
		map[string]string{"Depth": "0", "Timeout": "Second-3600"},
		`<?xml version="1.0" encoding="utf-8"?><D:lockinfo xmlns:D="DAV:">`+
			`<D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype>`+
			`<D:owner>finder</D:owner></D:lockinfo>`)
	token := res.Header.Get("Lock-Token")
	if res.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("LOCK: status %d token %q", res.StatusCode, token)
	}

	if res := basic(t, server, http.MethodPut, "/doc.txt", "john", "doe", strings.NewReader("script")); res.StatusCode != http.StatusLocked {
		t.Errorf("PUT from a script: status %d, want 423", res.StatusCode)
	}
	if res := dav(t, server, http.MethodPut, "/doc.txt", "john", "doe", nil, "other"); res.StatusCode != http.StatusLocked {
		t.Errorf("PUT from WebDAV without the lock: status %d, want 423", res.StatusCode)
	}
	if res := dav(t, server, http.MethodPut, "/doc.txt", "john", "doe",
		map[string]string{"If": "(" + token + ")"}, "holder"); res.StatusCode != http.StatusCreated {
		t.Errorf("PUT with the lock: status %d, want 201", res.StatusCode)
	}
	if server.read(t, "doc.txt") != "holder" {
		t.Errorf("file holds %q", server.read(t, "doc.txt"))
	}

	if res := dav(t, server, methodUnlock, "/doc.txt", "john", "doe",
		map[string]string{"Lock-Token": token}, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("UNLOCK: status %d", res.StatusCode)
	}
	if res := basic(t, server, http.MethodPut, "/doc.txt", "john", "doe", strings.NewReader("script")); res.StatusCode != http.StatusOK {
		t.Errorf("PUT from a script after the UNLOCK: status %d, want 200", res.StatusCode)
	}
}

// PROPPATCH is answered as done, which is what Explorer needs after each
// file it writes, and keeps nothing.
func TestWebDAVProppatch(t *testing.T) {
	server := davServer(t, nil)
	server.write(t, "a.txt", "a")
	res := dav(t, server, methodProppatch, "/a.txt", "john", "doe", nil,
		`<?xml version="1.0" encoding="utf-8"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:">`+
			`<D:set><D:prop><Z:Win32LastModifiedTime>Thu, 29 Oct 2026 10:00:00 GMT</Z:Win32LastModifiedTime></D:prop></D:set>`+
			`</D:propertyupdate>`)
	if body := bodyOf(t, res); res.StatusCode != http.StatusMultiStatus || !strings.Contains(body, "200 OK") {
		t.Errorf("status %d: %s", res.StatusCode, body)
	}
}

// An account is asked for by the methods that change something wherever
// they do for the page, and Basic is offered beside Digest over TLS only,
// since Explorer answers nothing else and sends it nowhere else.
func TestWebDAVChallenge(t *testing.T) {
	skipWithout(t, features.WebDAV)
	server := newServerWith(t, func(cfg *httpConfig) { cfg.WebDAV = true },
		func(https *config.HTTPS) { https.Enabled = true })

	res := dav(t, server, http.MethodPut, "/a.txt", "", "", nil, "a")
	if res.StatusCode != http.StatusUnauthorized || len(res.Header.Values("WWW-Authenticate")) != 1 {
		t.Errorf("plain: status %d challenges %q", res.StatusCode, res.Header.Values("WWW-Authenticate"))
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	port := server.SecureAddr().(*net.TCPAddr).Port
	req, _ := http.NewRequest(http.MethodPut,
		"https://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/a.txt", strings.NewReader("a"))
	req.Header.Set("User-Agent", explorer)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	challenges := strings.Join(res.Header.Values("WWW-Authenticate"), " | ")
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(challenges, "Digest ") ||
		!strings.Contains(challenges, `Basic realm="go-fs"`) {
		t.Errorf("tls: status %d challenges %q", res.StatusCode, challenges)
	}
}
