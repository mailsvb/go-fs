package httpd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
)

// With nothing in http.pathsWithoutAuth every path needs an account, the root
// and its listing included. What decides for itself before that — the login
// page and a share link — is still reachable without one.
func TestEveryPathNeedsAnAccountByDefault(t *testing.T) {
	server := newServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = config.Default().HTTP.PathsWithoutAuth
	})
	server.write(t, "file.txt", "content")
	server.write(t, "folder/inner.txt", "inner")

	for path, want := range map[string]int{"/": http.StatusOK, "/file.txt": http.StatusOK,
		"/folder": http.StatusMovedPermanently, "/folder/": http.StatusOK, "/folder/inner.txt": http.StatusOK} {
		if res, _ := get(t, server, path); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s got %d, want 401", path, res.StatusCode)
		}
		if res := basic(t, server, http.MethodGet, path, "john", "doe", nil); res.StatusCode != want {
			t.Errorf("GET %s with an account got %d, want %d", path, res.StatusCode, want)
		}
	}

	// a browser is sent to the login page, which it may then load
	res := browserGet(t, server, "/file.txt")
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "go-fs=login") {
		t.Errorf("a browser got %d to %q, want a redirect to the login page", res.StatusCode, res.Header.Get("Location"))
	}
	if res := browserGet(t, server, "/?go-fs=login"); res.StatusCode != http.StatusOK {
		t.Errorf("the login page got %d, want 200", res.StatusCode)
	}

	// a share link stands in for the account
	link := shareLink(t, server, "/file.txt")
	if res, body := get(t, server, link); res.StatusCode != http.StatusOK || body != "content" {
		t.Errorf("the share link got %d %q", res.StatusCode, body)
	}
}

// http.pathsWithoutAuth opens reading and nothing else: a GET or a HEAD of a
// path it lists needs no account, a write there still does, and every path it
// does not list needs one.
func TestPathsWithoutAuthOpensReadingOnly(t *testing.T) {
	server := newServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = []string{"^/pub/"}
	})
	server.write(t, "pub/open.txt", "open")
	server.write(t, "closed.txt", "closed")

	if res, body := get(t, server, "/pub/open.txt"); res.StatusCode != http.StatusOK || body != "open" {
		t.Errorf("anonymous GET of a listed path got %d %q", res.StatusCode, body)
	}
	if res, body := get(t, server, "/pub/"); res.StatusCode != http.StatusOK || !strings.Contains(body, "open.txt") {
		t.Errorf("anonymous listing of /pub/ got %d", res.StatusCode)
	}
	// the folder without its trailing slash is sent on to the listing rather
	// than asked for an account
	if res, _ := get(t, server, "/pub"); res.StatusCode != http.StatusMovedPermanently {
		t.Errorf("anonymous GET /pub got %d, want 301", res.StatusCode)
	}
	req, err := http.NewRequest(http.MethodHead, server.url("/pub/open.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Errorf("anonymous HEAD of a listed path got %d", res.StatusCode)
	}

	for _, path := range []string{"/", "/closed.txt", "/publication"} {
		if res, _ := get(t, server, path); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s got %d, want 401", path, res.StatusCode)
		}
	}

	// writing is never public
	req, err = http.NewRequest(http.MethodPut, server.url("/pub/new.txt"), strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous PUT in a listed path got %d, want 401", res.StatusCode)
	}
	req, err = http.NewRequest(http.MethodDelete, server.url("/pub/open.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE in a listed path got %d, want 401", res.StatusCode)
	}
	if got := server.read(t, "pub/open.txt"); got != "open" {
		t.Errorf("the file is now %q", got)
	}
}

// A server that nobody can read from is not refused, since its share links
// and pages still work, but it is worth a warning.
func TestNoReaderIsWarnedAbout(t *testing.T) {
	const warning = "no [[users]] entry sets http, there is no bearer token and http.pathsWithoutAuth is empty, " +
		"so nobody can read the files over http"
	nobody := newServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = nil
		cfg.Users = nil
	})
	if nobody.logs.find(warning) == nil {
		t.Error("a server nobody can read from was not warned about")
	}

	for name, tune := range map[string]func(*httpConfig){
		"an account":  func(cfg *httpConfig) { cfg.PathsWithoutAuth = nil },
		"public path": func(cfg *httpConfig) { cfg.Users = nil; cfg.PathsWithoutAuth = []string{"^/"} },
	} {
		if server := newServer(t, tune); server.logs.find(warning) != nil {
			t.Errorf("a server with %s was warned about", name)
		}
	}
}
