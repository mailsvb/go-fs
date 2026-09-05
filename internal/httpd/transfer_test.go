package httpd

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-fs/internal/config"
)

func TestDownload(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/hello.iso", "iso payload")

	res := basic(t, server, http.MethodGet, "/private/hello.iso", "john", "doe", nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "iso payload" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if got := res.Header.Get("Content-Type"); got != "application/x-iso9660-image" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="hello.iso"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := res.Header.Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q", got)
	}
	if record := server.logs.find("http download"); record == nil {
		t.Error("a download has to be reported")
	} else if record["file"] != "/private/hello.iso" {
		t.Errorf("download record = %v", record)
	}
}

// ServeContent answers a range request, which the Node implementation could
// not, so a large download can be resumed.
func TestRangeRequest(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/big.bin", "0123456789")

	req, _ := http.NewRequest(http.MethodGet, server.url("/private/big.bin"), nil)
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Range", "bytes=2-5")
	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Errorf("status %d body %q", res.StatusCode, body)
	}
}

func TestUploadBinary(t *testing.T) {
	server := newServer(t, nil)

	req, _ := http.NewRequest(http.MethodPut, server.url("/private/sub/new.txt"),
		strings.NewReader("uploaded"))
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Content-Type", "application/octet-stream")
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	// the folders above the file are created
	if got := server.read(t, "private/sub/new.txt"); got != "uploaded" {
		t.Errorf("stored %q", got)
	}
	if record := server.logs.find("http upload"); record == nil {
		t.Error("an upload has to be reported")
	} else if record["bytes"] != int64(8) {
		t.Errorf("upload record = %v", record)
	}
}

func TestUploadMultipart(t *testing.T) {
	server := newServer(t, nil)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "whatever.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("from a form")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()

	req, _ := http.NewRequest(http.MethodPut, server.url("/private/form.txt"), &body)
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := server.read(t, "private/form.txt"); got != "from a form" {
		t.Errorf("stored %q", got)
	}
}

// A PUT does not replace what is already there, as in the original.
func TestUploadDoesNotOverwrite(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/exists.txt", "original")

	req, _ := http.NewRequest(http.MethodPut, server.url("/private/exists.txt"),
		strings.NewReader("replacement"))
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Content-Type", "application/octet-stream")
	if res := do(t, req); res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if got := server.read(t, "private/exists.txt"); got != "original" {
		t.Errorf("the file was changed to %q", got)
	}
}

func TestUploadSizeLimit(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) { cfg.MaxUploadSize = 8 })

	req, _ := http.NewRequest(http.MethodPut, server.url("/private/big.txt"),
		strings.NewReader("far more than eight bytes"))
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Content-Type", "application/octet-stream")
	if res := do(t, req); res.StatusCode == http.StatusOK {
		t.Error("an upload past maxUploadSize has to fail")
	}
	if _, err := os.Stat(filepath.Join(server.base, "private", "big.txt")); !os.IsNotExist(err) {
		t.Error("the partial file has to be removed")
	}
}

func TestDelete(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/gone.txt", "x")
	server.write(t, "private/full/keep.txt", "x")
	if err := os.Mkdir(filepath.Join(server.base, "private", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	if res := basic(t, server, http.MethodDelete, "/private/gone.txt", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Errorf("deleting a file got %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(server.base, "private", "gone.txt")); !os.IsNotExist(err) {
		t.Error("the file is still there")
	}

	if res := basic(t, server, http.MethodDelete, "/private/empty", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Errorf("deleting an empty folder got %d", res.StatusCode)
	}
	// a folder with something in it is left alone
	if res := basic(t, server, http.MethodDelete, "/private/full", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("deleting a full folder got %d, want 404", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(server.base, "private", "full")); err != nil {
		t.Error("the folder that is not empty was removed")
	}
}

// Each right is granted explicitly, so an account without one is refused.
func TestPermissionsAreEnforced(t *testing.T) {
	cases := []struct {
		name   string
		user   config.HTTPUser
		method string
		body   io.Reader
		want   int
	}{
		{"upload denied", config.HTTPUser{
			Username: "john", Password: "doe", Paths: []string{"^/.*"},
			AllowUserFileDelete: true,
		}, http.MethodPut, strings.NewReader("x"), http.StatusForbidden},
		{"delete denied", config.HTTPUser{
			Username: "john", Password: "doe", Paths: []string{"^/.*"},
			AllowUserFileUpload: true,
		}, http.MethodDelete, nil, http.StatusForbidden},
		{"path denied", config.HTTPUser{
			Username: "john", Password: "doe", Paths: []string{"^/elsewhere/.*"},
			AllowUserFileUpload: true, AllowUserFileDelete: true,
		}, http.MethodGet, nil, http.StatusForbidden},
		{"no path at all", config.HTTPUser{
			Username: "john", Password: "doe",
		}, http.MethodGet, nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, func(cfg *config.HTTP) {
				cfg.Users = []config.HTTPUser{tc.user}
			})
			server.write(t, "private/hello.txt", "hello")

			path := "/private/hello.txt"
			if tc.method == http.MethodPut {
				path = "/private/new.txt"
			}
			req, _ := http.NewRequest(tc.method, server.url(path), tc.body)
			req.SetBasicAuth("john", "doe")
			req.Header.Set("Content-Type", "application/octet-stream")
			if res := do(t, req); res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// The served folder is the whole of the filesystem a client can reach, whether
// it climbs out with .. or follows a symbolic link out.
func TestConfinement(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.MethodsRequireAuth = nil
		cfg.PathsRequireAuth = nil
	})
	server.write(t, "hello.txt", "hello")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(server.base, "escape.txt")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/../../etc/hosts", "/sub/../../../etc/hosts", "/escape.txt"} {
		res, body := get(t, server, path)
		if res.StatusCode == http.StatusOK && strings.Contains(body, "secret") {
			t.Errorf("%s leaked content", path)
		}
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, res.StatusCode)
		}
	}
	if res, body := get(t, server, "/hello.txt"); res.StatusCode != http.StatusOK || body != "hello" {
		t.Errorf("a file inside the folder got %d %q", res.StatusCode, body)
	}
}

// A path regex is matched after normalization, so ".." cannot be used to get
// inside a pattern and then climb out of it again.
func TestPathPatternsCannotBeWalkedAround(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.PathsRequireAuth = []string{"^/.*"}
		cfg.Users = []config.HTTPUser{{
			Username: "john", Password: "doe", Paths: []string{"^/public/.*"},
		}}
	})
	server.write(t, "public/fine.txt", "fine")
	server.write(t, "secret.txt", "secret")

	if res := basic(t, server, http.MethodGet, "/public/fine.txt", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Errorf("the allowed path got %d", res.StatusCode)
	}
	// this normalizes to /secret.txt, which the pattern does not cover
	res := basic(t, server, http.MethodGet, "/public/../secret.txt", "john", "doe", nil)
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "secret") {
		t.Fatal("the pattern was walked around")
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
}

func TestCleanupKeepsTheNewest(t *testing.T) {
	base := t.TempDir()
	for i, name := range []string{"old3.iso", "old2.iso", "old1.iso", "new2.iso", "new1.iso"} {
		path := filepath.Join(base, "iso", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}

	server := newServer(t, func(cfg *config.HTTP) {
		cfg.Basefolder = base
		cfg.Cleanup = []config.Cleanup{{Path: "/iso", Keep: 2}}
	})
	// the sweep runs at startup; wait for it to report
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && server.logs.find("http cleanup removed a file") == nil {
		time.Sleep(20 * time.Millisecond)
	}

	left, err := os.ReadDir(filepath.Join(base, "iso"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("%d files left, want 2", len(left))
	}
	kept := map[string]bool{left[0].Name(): true, left[1].Name(): true}
	if !kept["new1.iso"] || !kept["new2.iso"] {
		t.Errorf("the wrong files were kept: %v", kept)
	}
}
