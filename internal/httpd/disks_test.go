//go:build !light && !no_nbd

package httpd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/disks"
	"github.com/mailsvb/go-fs/internal/sparse"
)

const mib = 1 << 20

// disksServer is a server with a Disks page, an admin root/secret and an
// account john/doe that may do everything to files and is no admin, and the
// folder of the disks.
func disksServer(t *testing.T) (*testServer, string) {
	t.Helper()
	folder := t.TempDir()
	server := newServer(t, func(cfg *httpConfig) {
		cfg.Users = []config.User{adminUser("root", "secret"), fullUser("john", "doe")}
		cfg.DisksFolder = folder
		cfg.NBDPort = 10809
		cfg.MaxChunkSize = 4 * mib
	})
	return server, folder
}

// diskPost sends what the Disks page sends: JSON, with the session, from the
// server's own origin.
func diskPost(t *testing.T, server *testServer, action string, session *http.Cookie, payload any) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	return trashPost(t, server, "/?go-fs="+action, session, string(body), "")
}

func diskState(t *testing.T, server *testServer, session *http.Cookie) disksStateJSON {
	t.Helper()
	res := withSession(t, server, http.MethodGet, "/?go-fs=disks-state", session)
	var state disksStateJSON
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatalf("the state is not JSON (%d): %v", res.StatusCode, err)
	}
	res.Body.Close()
	return state
}

func TestTheDisksPageIsForAdminsWithASession(t *testing.T) {
	server, _ := disksServer(t)
	if res := browserGet(t, server, "/?go-fs=disks"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("an anonymous browser = %d, want 303 to the login page", res.StatusCode)
	}
	if res, _ := get(t, server, "/?go-fs=disks-state"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous fetch = %d", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/?go-fs=disks-state", "root", "secret", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the admin's password as a header = %d, want 401: the page takes a session", res.StatusCode)
	}
	john := login(t, server, "/", "john", "doe")
	for _, action := range []string{"disks", "disks-state", "disks-download&name=x.raw"} {
		if res := withSession(t, server, http.MethodGet, "/?go-fs="+action, john); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s for an account that is no admin = %d", action, res.StatusCode)
		}
	}
	if res, _ := diskPost(t, server, "disks-create", john, map[string]string{"name": "x.raw", "size": "1G"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a create by an account that is no admin = %d", res.StatusCode)
	}
	if _, listing := getWith(t, server, "/", john); strings.Contains(listing, "go-fs=disks") {
		t.Error("the menu offers the Disks page to an account that is no admin")
	}

	root := login(t, server, "/", "root", "secret")
	res := withSession(t, server, http.MethodGet, "/?go-fs=disks", root)
	page := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, `id="disks"`) || !strings.Contains(page, `"nbdPort":10809`) {
		t.Fatalf("the page = %d:\n%s", res.StatusCode, page)
	}
	if _, listing := getWith(t, server, "/", root); !strings.Contains(listing, "/?go-fs=disks") {
		t.Error("the menu does not offer the Disks page to an admin")
	}
	if res, _ := trashPost(t, server, "/?go-fs=disks-create", root, `{"name":"x.raw","size":"1G"}`, "https://elsewhere.example"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a create from another origin = %d", res.StatusCode)
	}
}

func TestWithoutAFolderThereIsNoDisksPage(t *testing.T) {
	server := adminServer(t, nil)
	root := login(t, server, "/", "root", "secret")
	if res := withSession(t, server, http.MethodGet, "/?go-fs=disks", root); res.StatusCode != http.StatusNotFound {
		t.Errorf("the page without a folder = %d", res.StatusCode)
	}
}

func getWith(t *testing.T, server *testServer, path string, session *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, server.url(path), nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(session)
	res := do(t, req)
	return res, bodyOf(t, res)
}

func TestCreateResizeRenameDelete(t *testing.T) {
	server, folder := disksServer(t)
	root := login(t, server, "/", "root", "secret")
	if res, body := diskPost(t, server, "disks-create", root, map[string]string{"name": "web01.raw", "size": "10G"}); res.StatusCode != http.StatusOK {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	info, err := os.Stat(filepath.Join(folder, "web01.raw"))
	if err != nil || info.Size() != 10<<30 {
		t.Fatalf("the disk is %v, %v", info, err)
	}
	if allocated, _ := sparse.Allocated(filepath.Join(folder, "web01.raw")); allocated > 10*mib {
		t.Errorf("a new 10 GiB disk takes %d bytes", allocated)
	}
	for _, tc := range []struct {
		payload map[string]string
		status  int
		says    string
	}{
		{map[string]string{"name": "web01.raw", "size": "1G"}, http.StatusConflict, "choose another name"},
		{map[string]string{"name": "bad name", "size": "1G"}, http.StatusBadRequest, "not a valid disk name"},
		{map[string]string{"name": "x.raw", "size": "lots"}, http.StatusBadRequest, "not a size"},
	} {
		if res, body := diskPost(t, server, "disks-create", root, tc.payload); res.StatusCode != tc.status || !strings.Contains(body, tc.says) {
			t.Errorf("create %v = %d %q", tc.payload, res.StatusCode, body)
		}
	}
	state := diskState(t, server, root)
	if len(state.Disks) != 1 || state.Disks[0].Name != "web01.raw" || state.Disks[0].Size != 10<<30 || state.NBDPort != 10809 {
		t.Errorf("the state is %+v", state)
	}

	if res, body := diskPost(t, server, "disks-resize", root, map[string]any{"name": "web01.raw", "size": "5G"}); res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "shrinking") {
		t.Errorf("a shrink without asking = %d %q", res.StatusCode, body)
	}
	if res, _ := diskPost(t, server, "disks-resize", root, map[string]any{"name": "web01.raw", "size": "5G", "shrink": true}); res.StatusCode != http.StatusOK {
		t.Errorf("a shrink = %d", res.StatusCode)
	}
	if res, _ := diskPost(t, server, "disks-rename", root, map[string]string{"name": "web01.raw", "to": "web02.raw"}); res.StatusCode != http.StatusOK {
		t.Errorf("rename = %d", res.StatusCode)
	}
	if res, body := diskPost(t, server, "disks-delete", root, map[string]string{"name": "web02.raw", "confirm": "web01.raw"}); res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Type the name") {
		t.Errorf("a delete with the wrong name typed = %d %q", res.StatusCode, body)
	}
	if res, _ := diskPost(t, server, "disks-delete", root, map[string]string{"name": "web02.raw", "confirm": "web02.raw"}); res.StatusCode != http.StatusOK {
		t.Errorf("delete = %d", res.StatusCode)
	}
	if res, _ := diskPost(t, server, "disks-delete", root, map[string]string{"name": "web02.raw", "confirm": "web02.raw"}); res.StatusCode != http.StatusNotFound {
		t.Errorf("deleting twice = %d", res.StatusCode)
	}
	if server.logs.find("http disk created") == nil || server.logs.find("http disk deleted") == nil {
		t.Error("the changes are not logged")
	}
}

// attachTo attaches a disk the way the NBD server does, through a folder of
// its own on the same path, which shares the locks with the page's.
func attachTo(t *testing.T, folder, name string, writable bool) *disks.Session {
	t.Helper()
	f, err := disks.Open(folder)
	if err != nil {
		t.Fatal(err)
	}
	var lease *disks.Lease
	var file *os.File
	session := disks.NewSession(func(string) {
		go func() { file.Close(); lease.Release() }()
	})
	session.Account, session.Client, session.Writable, session.Fingerprint = "vm", "10.0.0.5:4711", writable, "ab"
	file, lease, err = f.Attach(name, session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close("the test is over") })
	return session
}

func TestAnAttachedDiskIsLeftAloneUntilItIsDisconnected(t *testing.T) {
	server, folder := disksServer(t)
	root := login(t, server, "/", "root", "secret")
	diskPost(t, server, "disks-create", root, map[string]string{"name": "vm.raw", "size": "1G"})
	attachTo(t, folder, "vm.raw", true)

	for action, payload := range map[string]map[string]any{
		"disks-resize": {"name": "vm.raw", "size": "2G"},
		"disks-rename": {"name": "vm.raw", "to": "other.raw"},
		"disks-delete": {"name": "vm.raw", "confirm": "vm.raw"},
		"disks-clone":  {"name": "vm.raw", "to": "copy.raw"},
	} {
		res, body := diskPost(t, server, action, root, payload)
		if res.StatusCode != http.StatusConflict || !strings.Contains(body, "attached read-write by vm") {
			t.Errorf("%s of an attached disk = %d %q", action, res.StatusCode, body)
		}
	}
	if res := withSession(t, server, http.MethodGet, "/?go-fs=disks-download&name=vm.raw", root); res.StatusCode != http.StatusConflict {
		t.Errorf("a download of a disk attached read-write = %d", res.StatusCode)
	}

	state := diskState(t, server, root)
	sessions := state.Disks[0].Sessions
	if len(sessions) != 1 || sessions[0].Account != "vm" || !sessions[0].Writable || sessions[0].Client != "10.0.0.5:4711" {
		t.Fatalf("the sessions are %+v", sessions)
	}
	if res, _ := diskPost(t, server, "disks-disconnect", root, map[string]string{"id": sessions[0].ID}); res.StatusCode != http.StatusOK {
		t.Fatalf("disconnect = %d", res.StatusCode)
	}
	if res, _ := diskPost(t, server, "disks-disconnect", root, map[string]string{"id": sessions[0].ID}); res.StatusCode != http.StatusNotFound {
		t.Errorf("disconnecting twice = %d", res.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, _ := diskPost(t, server, "disks-resize", root, map[string]any{"name": "vm.raw", "size": "2G"})
		if res.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the disk is still held after the disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// putChunk sends one piece of a disk upload.
func putDiskChunk(t *testing.T, server *testServer, session *http.Cookie, name string, replace bool, data []byte, start, total int) *http.Response {
	t.Helper()
	target := "/?go-fs=disks-upload&name=" + name
	if replace {
		target += "&replace=1"
	}
	req, _ := http.NewRequest(http.MethodPut, server.url(target), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+len(data)-1, total))
	req.Header.Set("Origin", strings.TrimSuffix(server.url(""), "/"))
	req.AddCookie(session)
	return do(t, req)
}

func TestUploadAndDownload(t *testing.T) {
	server, folder := disksServer(t)
	root := login(t, server, "/", "root", "secret")
	image := append(bytes.Repeat([]byte("boot"), mib/4), make([]byte, 6*mib)...)
	image = append(image, bytes.Repeat([]byte{0xfe}, 4096)...)
	total := len(image)
	chunk := 4 * mib

	if res := putDiskChunk(t, server, root, "up.raw", false, image[:chunk], 0, total); res.StatusCode != http.StatusOK {
		t.Fatalf("the first piece = %d %s", res.StatusCode, bodyOf(t, res))
	}
	// a piece out of place is told where to go on
	res := putDiskChunk(t, server, root, "up.raw", false, image[chunk+1:chunk+1000], chunk+1, total)
	if res.StatusCode != http.StatusRequestedRangeNotSatisfiable || res.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", chunk) {
		t.Fatalf("a piece out of place = %d %q", res.StatusCode, res.Header.Get("Content-Range"))
	}
	if res := putDiskChunk(t, server, root, "up.raw", false, image[chunk:2*mib+chunk], chunk, total); res.StatusCode != http.StatusOK {
		t.Fatalf("the second piece = %d", res.StatusCode)
	}
	if res := putDiskChunk(t, server, root, "up.raw", false, image[2*mib+chunk:], 2*mib+chunk, total); res.StatusCode != http.StatusCreated {
		t.Fatalf("the last piece = %d %s", res.StatusCode, bodyOf(t, res))
	}
	got, err := os.ReadFile(filepath.Join(folder, "up.raw"))
	if err != nil || !bytes.Equal(got, image) {
		t.Fatalf("the uploaded disk differs: %v", err)
	}
	if res := putDiskChunk(t, server, root, "up.raw", false, image[:chunk], 0, total); res.StatusCode != http.StatusConflict {
		t.Errorf("an upload onto a disk without replace = %d", res.StatusCode)
	}
	if res := putDiskChunk(t, server, root, "up.raw", true, []byte("tiny"), 0, 4); res.StatusCode != http.StatusCreated {
		t.Errorf("a replacing upload = %d %s", res.StatusCode, bodyOf(t, res))
	}
	if res := putDiskChunk(t, server, root, "up.raw", true, make([]byte, chunk+1), 0, chunk+1); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a piece above maxChunkSize = %d", res.StatusCode)
	}

	// the download is the disk, whole or a range of it
	res = withSession(t, server, http.MethodGet, "/?go-fs=disks-download&name=up.raw", root)
	if body := bodyOf(t, res); res.StatusCode != http.StatusOK || body != "tiny" ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "up.raw") {
		t.Errorf("download = %d %q %q", res.StatusCode, body, res.Header.Get("Content-Disposition"))
	}
	req, _ := http.NewRequest(http.MethodGet, server.url("/?go-fs=disks-download&name=up.raw"), nil)
	req.Header.Set("Range", "bytes=1-2")
	req.AddCookie(root)
	if res := do(t, req); res.StatusCode != http.StatusPartialContent || bodyOf(t, res) != "in" {
		t.Errorf("a range = %d", res.StatusCode)
	}
	if res := withSession(t, server, http.MethodGet, "/?go-fs=disks-download&name=missing.raw", root); res.StatusCode != http.StatusNotFound {
		t.Errorf("a missing disk = %d", res.StatusCode)
	}
}

func TestCloneRunsInTheBackground(t *testing.T) {
	server, folder := disksServer(t)
	root := login(t, server, "/", "root", "secret")
	diskPost(t, server, "disks-create", root, map[string]string{"name": "base.raw", "size": "64M"})
	if err := os.WriteFile(filepath.Join(folder, "base.raw"), bytes.Repeat([]byte{9}, 3*mib), 0o640); err != nil {
		t.Fatal(err)
	}
	if res, body := diskPost(t, server, "disks-clone", root, map[string]string{"name": "base.raw", "to": "copy.raw"}); res.StatusCode != http.StatusOK {
		t.Fatalf("clone = %d %s", res.StatusCode, body)
	}
	var job jobJSON
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := diskState(t, server, root)
		if len(state.Jobs) != 1 {
			t.Fatalf("the jobs are %+v", state.Jobs)
		}
		if job = state.Jobs[0]; job.State != disks.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the clone never ends")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.State != disks.JobDone || job.Source != "base.raw" || job.Target != "copy.raw" {
		t.Fatalf("the clone ended as %+v", job)
	}
	original, _ := os.ReadFile(filepath.Join(folder, "base.raw"))
	copied, _ := os.ReadFile(filepath.Join(folder, "copy.raw"))
	if !bytes.Equal(original, copied) {
		t.Error("the clone differs from the original")
	}
	if res, _ := diskPost(t, server, "disks-job", root, map[string]string{"id": job.ID}); res.StatusCode != http.StatusOK {
		t.Errorf("dismissing the finished clone = %d", res.StatusCode)
	}
	if state := diskState(t, server, root); len(state.Jobs) != 0 {
		t.Errorf("the dismissed clone is still listed: %+v", state.Jobs)
	}
	deadline = time.Now().Add(5 * time.Second)
	for server.logs.find("http disk clone ended") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the end of the clone is not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
