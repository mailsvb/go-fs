package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mailsvb/go-fs/internal/features"
)

// trashServer is adminServer with a trash that keeps what is in it for 30
// days, and the folder of that trash.
func trashServer(t *testing.T, tune func(*httpConfig)) (*testServer, string) {
	t.Helper()
	trash := t.TempDir()
	server := adminServer(t, func(cfg *httpConfig) {
		cfg.TrashFolder = trash
		cfg.TrashRetentionDays = 30
		if tune != nil {
			tune(cfg)
		}
	})
	return server, trash
}

// inTrash reads a file of the trash, and fails the test when it is not there.
func inTrash(t *testing.T, trash, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(trash, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("the trash does not hold %s: %v", name, err)
	}
	return string(body)
}

// gone reports whether a path is not there.
func gone(at string) bool {
	_, err := os.Lstat(at)
	return os.IsNotExist(err)
}

// trashIndexOf reads the index of the trash.
func trashIndexOf(t *testing.T, trash string) trashIndex {
	t.Helper()
	return readTrashIndex(trash)
}

// trashPost sends what the Trash page sends: a JSON body, with the session and
// from the server's own origin unless origin says otherwise.
func trashPost(t *testing.T, server *testServer, path string, session *http.Cookie, body, origin string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if origin == "" {
		origin = server.url("")
	}
	req.Header.Set("Origin", strings.TrimSuffix(origin, "/"))
	if session != nil {
		req.AddCookie(session)
	}
	res := do(t, req)
	return res, bodyOf(t, res)
}

// A file deleted over http goes to the trash, at the path it had and with the
// date it had, and the index says when it was deleted.
func TestDeleteMovesIntoTheTrash(t *testing.T) {
	server, trash := trashServer(t, nil)
	server.write(t, "a/b.txt", "content")
	modified := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	touch(t, server, "a/b.txt", modified)

	before := time.Now()
	if res := basic(t, server, http.MethodDelete, "/a/b.txt", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: status %d, want 200", res.StatusCode)
	}
	if !gone(filepath.Join(server.base, "a", "b.txt")) {
		t.Error("the file is still in the served folder")
	}
	if got := inTrash(t, trash, "a/b.txt"); got != "content" {
		t.Errorf("the trash holds %q, want the file", got)
	}
	if info, err := os.Stat(filepath.Join(trash, "a", "b.txt")); err != nil || !info.ModTime().Equal(modified) {
		t.Errorf("the file in the trash was modified %v, want %v as before", info.ModTime(), modified)
	}
	if when := trashIndexOf(t, trash)["/a/b.txt"]; when.Before(before.Add(-time.Second)) {
		t.Errorf("the index says the file was deleted %v, want now", when)
	}
	if record := server.logs.find("http delete"); record == nil || record["trash"] != true {
		t.Errorf("the delete is not reported as one into the trash: %v", record)
	}

	// an empty folder goes the same way
	server.mkdir(t, "empty")
	if res := basic(t, server, http.MethodDelete, "/empty/", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE of a folder: status %d, want 200", res.StatusCode)
	}
	if info, err := os.Stat(filepath.Join(trash, "empty")); err != nil || !info.IsDir() {
		t.Error("the empty folder is not in the trash")
	}
}

// Without a trash, a delete is for good, as it always was.
func TestDeleteWithoutATrashIsForGood(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "a.txt", "content")
	if res := basic(t, server, http.MethodDelete, "/a.txt", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: status %d, want 200", res.StatusCode)
	}
	if !gone(filepath.Join(server.base, "a.txt")) {
		t.Error("the file is still there")
	}
	if record := server.logs.find("http delete"); record == nil || record["trash"] != false {
		t.Errorf("the delete is reported as one into the trash: %v", record)
	}
}

// Deleting a path again replaces what the trash held for it, and a folder is
// merged into the one the trash holds.
func TestDeletingAgainReplacesWhatTheTrashHolds(t *testing.T) {
	skipWithout(t, features.WebDAV)
	server, trash := trashServer(t, func(cfg *httpConfig) { cfg.WebDAV = true })
	server.write(t, "x.txt", "first")
	basic(t, server, http.MethodDelete, "/x.txt", "john", "doe", nil)
	server.write(t, "x.txt", "second")
	basic(t, server, http.MethodDelete, "/x.txt", "john", "doe", nil)
	if got := inTrash(t, trash, "x.txt"); got != "second" {
		t.Errorf("the trash holds %q, want the newer file", got)
	}

	server.write(t, "box/old.txt", "old")
	server.write(t, "box/kept.txt", "kept")
	basic(t, server, http.MethodDelete, "/box/old.txt", "john", "doe", nil)
	basic(t, server, http.MethodDelete, "/box/kept.txt", "john", "doe", nil)
	server.write(t, "box/old.txt", "newer")
	server.write(t, "box/new.txt", "new")
	if res := dav(t, server, http.MethodDelete, "/box/", "john", "doe", nil, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("WebDAV DELETE: status %d, want 204", res.StatusCode)
	}
	for name, want := range map[string]string{"box/old.txt": "newer", "box/kept.txt": "kept", "box/new.txt": "new"} {
		if got := inTrash(t, trash, name); got != want {
			t.Errorf("the trash holds %q at %s, want %q", got, name, want)
		}
	}
	index := trashIndexOf(t, trash)
	if _, ok := index["/box/old.txt"]; ok {
		t.Error("the replaced file kept the date of the older delete")
	}
	if _, ok := index["/box/kept.txt"]; !ok {
		t.Error("the file the merge left alone lost the date it was deleted on")
	}

	// a file in the way of a folder is replaced by it
	server.write(t, "clash", "a file")
	basic(t, server, http.MethodDelete, "/clash", "john", "doe", nil)
	server.write(t, "clash/inner.txt", "inner")
	basic(t, server, http.MethodDelete, "/clash/inner.txt", "john", "doe", nil)
	if got := inTrash(t, trash, "clash/inner.txt"); got != "inner" {
		t.Errorf("the trash holds %q, want the file in the folder that replaced a file", got)
	}
}

// What WebDAV removes goes to the trash: a folder it deletes with everything
// in it, and what a MOVE or a COPY replaces.
func TestWebDAVDeletesIntoTheTrash(t *testing.T) {
	skipWithout(t, features.WebDAV)
	server, trash := trashServer(t, func(cfg *httpConfig) { cfg.WebDAV = true })
	server.write(t, "box/a.txt", "a")
	server.write(t, "box/sub/b.txt", "b")
	if res := dav(t, server, http.MethodDelete, "/box/", "john", "doe", nil, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status %d, want 204", res.StatusCode)
	}
	if !gone(filepath.Join(server.base, "box")) {
		t.Error("the folder is still in the served folder")
	}
	if inTrash(t, trash, "box/a.txt") != "a" || inTrash(t, trash, "box/sub/b.txt") != "b" {
		t.Error("the folder is not in the trash with what was in it")
	}

	server.write(t, "from.txt", "new")
	server.write(t, "to.txt", "old")
	res := dav(t, server, methodMove, "/from.txt", "john", "doe",
		map[string]string{"Destination": server.url("/to.txt"), "Overwrite": "T"}, "")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE: status %d, want 204", res.StatusCode)
	}
	if server.read(t, "to.txt") != "new" {
		t.Error("the MOVE did not replace the file")
	}
	if got := inTrash(t, trash, "to.txt"); got != "old" {
		t.Errorf("the trash holds %q, want the file the MOVE replaced", got)
	}
}

// S3 deletes into the trash as well.
func TestS3DeletesIntoTheTrash(t *testing.T) {
	trash := t.TempDir()
	server := newS3Server(t, func(cfg *httpConfig) { cfg.TrashFolder = trash })
	client := server.s3Client("john", "doe")
	put(t, client, "keep/k.txt", "kept")
	put(t, client, "dir/k.txt", "object")
	if _, err := client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("dir/k.txt")}); err != nil {
		t.Fatal(err)
	}
	if got := inTrash(t, trash, bucket+"/dir/k.txt"); got != "object" {
		t.Errorf("the trash holds %q, want the object", got)
	}
	if !gone(filepath.Join(server.base, bucket, "dir")) {
		t.Error("the folder the object left empty is still there")
	}
}

// The Trash page and its endpoints are there for a session of an admin
// account, and only while there is a trash.
func TestTheTrashNeedsAnAdminSession(t *testing.T) {
	server, _ := trashServer(t, func(cfg *httpConfig) { publicServer(cfg) })
	server.write(t, "b.txt", "b")
	basic(t, server, http.MethodDelete, "/b.txt", "john", "doe", nil)

	res := browserGet(t, server, "/?go-fs=trash")
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("an anonymous browser was answered %d, want 303 to the login page", res.StatusCode)
	}
	if location := res.Header.Get("Location"); !strings.Contains(location, "go-fs=login") ||
		!strings.Contains(location, "go-fs-then=trash") {
		t.Errorf("the browser was sent to %q, want the login page that returns to the trash", location)
	}
	if res, _ := trashPost(t, server, "/?go-fs=trash-empty", nil, "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous empty was answered %d, want 401", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/?go-fs=trash", "root", "secret", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("root's password as a header was answered %d, want 401", res.StatusCode)
	}

	john := login(t, server, "/", "john", "doe")
	if res := withSession(t, server, http.MethodGet, "/?go-fs=trash", john); res.StatusCode != http.StatusForbidden {
		t.Errorf("john's session was answered %d, want 403", res.StatusCode)
	}
	if body := bodyOf(t, withSession(t, server, http.MethodGet, "/", john)); strings.Contains(body, "go-fs=trash") {
		t.Error("john's listing offers the trash")
	}
	if res, _ := trashPost(t, server, "/?go-fs=trash-empty", john, "", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("john's empty was answered %d, want 403", res.StatusCode)
	}

	root := login(t, server, "/", "root", "secret")
	res = withSession(t, server, http.MethodGet, "/?go-fs=trash", root)
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `data-name="b.txt"`) {
		t.Fatalf("root's session was answered %d without the deleted file:\n%s", res.StatusCode, body)
	}
	if strings.Contains(body, trashIndexName) {
		t.Error("the page lists the index of the trash")
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "nonce-") {
		t.Error("the page carries no content policy")
	}
	if listing := bodyOf(t, withSession(t, server, http.MethodGet, "/", root)); !strings.Contains(listing, `href="/?go-fs=trash"`) {
		t.Error("root's listing does not offer the trash")
	}
	if res, _ := trashPost(t, server, "/?go-fs=trash-empty", root, "", "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Errorf("an empty from another site was answered %d, want 403", res.StatusCode)
	}
	if res := withSession(t, server, http.MethodDelete, "/?go-fs=trash", root); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on the page was answered %d, want 405", res.StatusCode)
	}

	// without a trash the marker names nothing, and nothing offers it
	plain := adminServer(t, func(cfg *httpConfig) { publicServer(cfg) })
	session := login(t, plain, "/", "root", "secret")
	if res := withSession(t, plain, http.MethodGet, "/?go-fs=trash", session); res.StatusCode != http.StatusNotFound {
		t.Errorf("the page without a trash was answered %d, want 404", res.StatusCode)
	}
	if body := bodyOf(t, withSession(t, plain, http.MethodGet, "/", session)); strings.Contains(body, "go-fs=trash") {
		t.Error("the listing offers a trash that is off")
	}
}

// Restoring puts what the trash holds back where it was deleted from. What is
// in its place is kept unless the restore is told to overwrite it, and what
// is kept stays in the trash.
func TestTrashRestores(t *testing.T) {
	server, trash := trashServer(t, nil)
	server.write(t, "a/b.txt", "trashed b")
	server.write(t, "a/c.txt", "trashed c")
	basic(t, server, http.MethodDelete, "/a/b.txt", "john", "doe", nil)
	basic(t, server, http.MethodDelete, "/a/c.txt", "john", "doe", nil)
	server.write(t, "a/b.txt", "live b")
	root := login(t, server, "/", "root", "secret")

	res, body := trashPost(t, server, "/?go-fs=trash-restore", root, `{"name":"a","overwrite":false}`, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("restore: status %d: %s", res.StatusCode, body)
	}
	var done trashRestoreJSON
	if err := json.Unmarshal([]byte(body), &done); err != nil || done.Restored != 1 || done.Skipped != 1 {
		t.Errorf("restore answered %s, want 1 restored and 1 skipped", body)
	}
	if server.read(t, "a/b.txt") != "live b" || server.read(t, "a/c.txt") != "trashed c" {
		t.Error("the restore without overwrite did not keep the live file and restore the other")
	}
	if inTrash(t, trash, "a/b.txt") != "trashed b" || !gone(filepath.Join(trash, "a", "c.txt")) {
		t.Error("the trash does not hold just what was skipped")
	}

	res, body = trashPost(t, server, "/a/?go-fs=trash-restore", root, `{"name":"b.txt","overwrite":true}`, "")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"restored":1`) {
		t.Fatalf("restore with overwrite: status %d: %s", res.StatusCode, body)
	}
	if server.read(t, "a/b.txt") != "trashed b" {
		t.Error("the restore with overwrite did not replace the live file")
	}
	if !gone(filepath.Join(trash, "a")) {
		t.Error("the folder the restore left empty is still in the trash")
	}
	if index := trashIndexOf(t, trash); len(index) != 0 {
		t.Errorf("the index still names %v", index)
	}

	// the folders above what is restored are made again
	server.write(t, "deep/er/f.txt", "f")
	basic(t, server, http.MethodDelete, "/deep/er/f.txt", "john", "doe", nil)
	if err := os.RemoveAll(filepath.Join(server.base, "deep")); err != nil {
		t.Fatal(err)
	}
	if res, body := trashPost(t, server, "/deep/er/?go-fs=trash-restore", root, `{"name":"f.txt"}`, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("restore into a folder that is gone: status %d: %s", res.StatusCode, body)
	}
	if server.read(t, "deep/er/f.txt") != "f" {
		t.Error("the file was not restored into the folders it was in")
	}

	// what the trash does not hold, and names that are not one entry
	if res, _ := trashPost(t, server, "/?go-fs=trash-restore", root, `{"name":"nothing"}`, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("restoring nothing was answered %d, want 404", res.StatusCode)
	}
	for _, name := range []string{"", "..", "a/b", trashIndexName} {
		body, _ := json.Marshal(trashRequest{Name: name})
		if res, _ := trashPost(t, server, "/?go-fs=trash-restore", root, string(body), ""); res.StatusCode == http.StatusOK {
			t.Errorf("restoring %q was answered 200", name)
		}
	}
}

// Deleting from the trash and emptying it are for good.
func TestTrashDeletesAndEmpties(t *testing.T) {
	server, trash := trashServer(t, nil)
	server.write(t, "a/one.txt", "1")
	server.write(t, "two.txt", "2")
	server.write(t, "three.txt", "3")
	for _, name := range []string{"/a/one.txt", "/two.txt", "/three.txt"} {
		basic(t, server, http.MethodDelete, name, "john", "doe", nil)
	}
	root := login(t, server, "/", "root", "secret")

	if res, body := trashPost(t, server, "/a/?go-fs=trash-delete", root, `{"name":"one.txt"}`, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("delete: status %d: %s", res.StatusCode, body)
	}
	if !gone(filepath.Join(trash, "a")) {
		t.Error("the file and the folder it left empty are still in the trash")
	}
	if _, ok := trashIndexOf(t, trash)["/a/one.txt"]; ok {
		t.Error("the index still names the deleted file")
	}

	res, body := trashPost(t, server, "/?go-fs=trash-empty", root, "", "")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"items":2`) {
		t.Fatalf("empty: status %d: %s", res.StatusCode, body)
	}
	if items, err := os.ReadDir(trash); err != nil || len(items) != 0 {
		t.Errorf("the trash still holds %v", items)
	}
	if page := bodyOf(t, withSession(t, server, http.MethodGet, "/?go-fs=trash", root)); !strings.Contains(page, "The trash is empty.") {
		t.Error("the page of an empty trash does not say so")
	}
}

// The hourly sweep deletes what has been in the trash longer than it keeps
// things, and leaves the rest; a retention of 0 keeps everything.
func TestTrashIsPurgedAfterTheRetention(t *testing.T) {
	server, trash := trashServer(t, nil)
	for _, name := range []string{"old.txt", "new.txt", "box/early.txt", "box/late.txt"} {
		server.write(t, name, name)
	}
	basic(t, server, http.MethodDelete, "/old.txt", "john", "doe", nil)
	basic(t, server, http.MethodDelete, "/new.txt", "john", "doe", nil)
	basic(t, server, http.MethodDelete, "/box/early.txt", "john", "doe", nil)
	basic(t, server, http.MethodDelete, "/box/late.txt", "john", "doe", nil)
	if err := os.WriteFile(filepath.Join(trash, "manual.txt"), []byte("by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(trash, "manual.txt"), long, long); err != nil {
		t.Fatal(err)
	}
	index := trashIndexOf(t, trash)
	index["/old.txt"] = long
	index["/box"] = long
	delete(index, "/box/early.txt")
	if err := writeTrashIndex(trash, index); err != nil {
		t.Fatal(err)
	}

	// kept for ever, nothing goes
	withRetention(server, 0)
	server.purgeTrash()
	if gone(filepath.Join(trash, "old.txt")) {
		t.Fatal("a retention of 0 purged the trash")
	}

	withRetention(server, 30)
	server.purgeTrash()
	for _, name := range []string{"old.txt", "manual.txt", "box/early.txt"} {
		if !gone(filepath.Join(trash, filepath.FromSlash(name))) {
			t.Errorf("%s outlived the retention", name)
		}
	}
	for _, name := range []string{"new.txt", "box/late.txt"} {
		if gone(filepath.Join(trash, filepath.FromSlash(name))) {
			t.Errorf("%s was purged before its time", name)
		}
	}
	index = trashIndexOf(t, trash)
	if _, ok := index["/old.txt"]; ok {
		t.Error("the index still names what was purged")
	}
	if record := server.logs.find("trash purge"); record == nil {
		t.Error("the purge is not reported")
	}
}

// withRetention swaps the server's settings for a copy with another
// retention, as a reload of the file does.
func withRetention(server *testServer, days int) {
	next := *server.settings()
	next.cfg.TrashRetentionDays = days
	server.snapshot.Store(&next)
}

// The script is inlined into the page, so nothing in it may end the element.
func TestTrashScriptCanBeInlined(t *testing.T) {
	if strings.Contains(strings.ToLower(string(trashScript)), "</script") {
		t.Error("trash.js contains </script, which would end the inline script early")
	}
}
