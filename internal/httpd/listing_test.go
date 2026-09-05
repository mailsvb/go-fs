package httpd

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-fs/internal/config"
)

func TestDirectoryListing(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.MethodsRequireAuth = nil
		cfg.PathsRequireAuth = nil
	})
	server.write(t, "sub/old.txt", "old")
	server.write(t, "sub/new.iso", "newer")
	server.write(t, "sub/deeper/kept.txt", "x")
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(server.base, "sub", "old.txt"), older, older); err != nil {
		t.Fatal(err)
	}

	res, body := get(t, server, "/sub/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=UTF-8" {
		t.Errorf("Content-Type = %q", got)
	}

	for _, want := range []string{
		`<a href="/">../</a>`,       // the way back up, an absolute href as the original writes it
		`<a href="deeper/">deeper/`, // folders carry a trailing slash
		`<a href="new.iso">new.iso`, //
		`<a href="old.txt">old.txt`, //
		"Directory",                 // the folder's Type cell
		"ISO",                       // from the extension table
		"Document",                  //
		`<div class="table">`,       // the grid the Node version renders
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}

	// folders come first, then files newest first
	deeper := strings.Index(body, "deeper/")
	newIso := strings.Index(body, "new.iso")
	oldTxt := strings.Index(body, "old.txt")
	if !(deeper < newIso && newIso < oldTxt) {
		t.Errorf("order is wrong: deeper %d, new.iso %d, old.txt %d", deeper, newIso, oldTxt)
	}
}

// The root has no way back up.
func TestListingRootHasNoParentLink(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.MethodsRequireAuth = nil
		cfg.PathsRequireAuth = nil
	})
	server.write(t, "hello.txt", "hello")

	_, body := get(t, server, "/")
	if strings.Contains(body, `<a href="../">`) {
		t.Error("the root should not link to a parent")
	}
}

// A name with characters that mean something in HTML or in a URL is escaped.
func TestListingEscapesNames(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.MethodsRequireAuth = nil
		cfg.PathsRequireAuth = nil
	})
	server.write(t, "a b&c<d>.txt", "x")

	_, body := get(t, server, "/")
	if strings.Contains(body, "<d>.txt") {
		t.Error("the name was not escaped into the page")
	}
	if !strings.Contains(body, "a%20b&amp;c&lt;d&gt;.txt") && !strings.Contains(body, "a%20b&c%3Cd%3E.txt") {
		t.Errorf("the link was not escaped: %s", body)
	}
}

func TestDirectoryReaderEndpoint(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.Users = []config.HTTPUser{fullUser("john", "doe")}
	})
	server.write(t, "area/listed/one.txt", "one")
	server.write(t, "area/listed/two.iso", "two")

	form := url.Values{"dir": {"listed"}}
	req, _ := http.NewRequest(http.MethodPost,
		server.url("/area/dls_directory_reader.php"), strings.NewReader(form.Encode()))
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	page := string(body)
	for _, want := range []string{
		"listing directory: listed",
		`<a href="one.txt">one.txt</a> - filetype: file filesize: 3<br/>`,
		`<a href="two.iso">two.iso</a> - filetype: file filesize: 3<br/>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the answer is missing %q\ngot: %s", want, page)
		}
	}
}

func TestDirectoryReaderRefusesEscapes(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "area/listed/one.txt", "one")

	for _, dir := range []string{"../../../etc", "/etc", "missing"} {
		form := url.Values{"dir": {dir}}
		req, _ := http.NewRequest(http.MethodPost,
			server.url("/area/dls_directory_reader.php"), strings.NewReader(form.Encode()))
		req.SetBasicAuth("john", "doe")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if res := do(t, req); res.StatusCode != http.StatusNotFound {
			t.Errorf("dir=%q got %d, want 404", dir, res.StatusCode)
		}
	}
}

// A POST anywhere else is not a thing this server does.
func TestOtherPostsAreNotFound(t *testing.T) {
	server := newServer(t, nil)

	req, _ := http.NewRequest(http.MethodPost, server.url("/private/anything"), nil)
	req.SetBasicAuth("john", "doe")
	if res := do(t, req); res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestUnsupportedMethod(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) { cfg.MethodsRequireAuth = nil })

	req, _ := http.NewRequest(http.MethodPatch, server.url("/"), nil)
	res := do(t, req)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); !strings.Contains(got, "GET") {
		t.Errorf("Allow = %q", got)
	}
}

func TestReadableSize(t *testing.T) {
	cases := map[int64]string{
		512:            "0.5 KB",
		2048:           "2 KB",
		5 * 1000000:    "4.8 MB",
		3 * 1000000000: "2.8 GB",
	}
	for size, want := range cases {
		if got := readableSize(size); got != want {
			t.Errorf("readableSize(%d) = %q, want %q", size, got, want)
		}
	}
}

func TestTypeOf(t *testing.T) {
	cases := map[string]contentType{
		"a.iso":     {"application/x-iso9660-image", "ISO"},
		"a.RPM":     {"application/x-rpm", "RPM"},
		"a.tar.gz":  {"application/gzip", "Archive"},
		"a.unknown": {"application/octet-stream", "Generic"},
		"noext":     {"application/octet-stream", "Generic"},
	}
	for name, want := range cases {
		if got := typeOf(name); got != want {
			t.Errorf("typeOf(%q) = %+v, want %+v", name, got, want)
		}
	}
}
