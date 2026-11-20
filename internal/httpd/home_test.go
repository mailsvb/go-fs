package httpd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
)

// newPrivateRegistryServer is a server whose every path needs an account, with
// the registry on and its own account beside john.
func newPrivateRegistryServer(t *testing.T, tune func(*httpConfig)) *testServer {
	t.Helper()
	return newRegistryServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = nil
		cfg.RegistryAnonymousRead = false
		if tune != nil {
			tune(cfg)
		}
	})
}

func TestTheMainURLAsksWhatToLogInTo(t *testing.T) {
	server := newPrivateRegistryServer(t, nil)
	server.write(t, "secret.txt", "x")

	res, body := pageRequest(t, server, http.MethodGet, "/", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	for _, want := range []string{`href="/?go-fs=login"`, `href="/?go-fs=registry"`, ">Files<", ">Registry<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret.txt") {
		t.Error("the page shows the folder to someone who is not signed in")
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := res.Header.Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q", got)
	}
	policy := res.Header.Get("Content-Security-Policy")
	if strings.Contains(policy, "form-action") {
		t.Errorf("the policy allows a form the page does not have: %s", policy)
	}
	nonce := strings.TrimPrefix(strings.Fields(policy[strings.Index(policy, "style-src"):])[1], "'nonce-")
	nonce = strings.TrimSuffix(strings.TrimSuffix(nonce, ";"), "'")
	if !strings.Contains(body, `<style nonce="`+nonce+`">`) {
		t.Errorf("the style does not carry the policy's nonce %q", nonce)
	}

	head, body := pageRequest(t, server, http.MethodHead, "/", nil)
	if head.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD = %d with %d bytes, want 200 and none", head.StatusCode, len(body))
	}
}

func TestTheMainURLOffersAnAnonymousRegistry(t *testing.T) {
	// nobody has an account of the registry, but anyone may look at it
	server := newServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = nil
		cfg.RegistryBaseFolder = t.TempDir()
		cfg.RegistryAnonymousRead = true
	})
	res, body := pageRequest(t, server, http.MethodGet, "/", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `href="/?go-fs=registry"`) {
		t.Errorf("status = %d, want 200 with the registry offered:\n%s", res.StatusCode, body)
	}
}

func TestTheMainURLGoesStraightToTheOnlyPlace(t *testing.T) {
	cases := []struct {
		name   string
		server func(t *testing.T) *testServer
		want   string
	}{
		{"registry off", func(t *testing.T) *testServer {
			return newServer(t, func(cfg *httpConfig) { cfg.PathsWithoutAuth = nil })
		}, "/?go-fs=login"},
		{"registry accounts only", func(t *testing.T) *testServer {
			return newPrivateRegistryServer(t, func(cfg *httpConfig) {
				cfg.Users = []config.User{registryUser(pusher, pusherPassword)}
			})
		}, "/?go-fs=registry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := pageRequest(t, c.server(t), http.MethodGet, "/", nil)
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != c.want {
				t.Errorf("answer = %d to %q, want 303 to %q", res.StatusCode, res.Header.Get("Location"), c.want)
			}
		})
	}
}

func TestTheMainURLOfASessionIsWhatItAlwaysWas(t *testing.T) {
	server := newPrivateRegistryServer(t, nil)
	server.write(t, "secret.txt", "x")

	files := login(t, server, "/", "john", "doe")
	res, body := pageRequest(t, server, http.MethodGet, "/", files)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "secret.txt") {
		t.Errorf("an http session got %d without the listing", res.StatusCode)
	}

	registry := login(t, server, "/", pusher, pusherPassword)
	res, _ = pageRequest(t, server, http.MethodGet, "/", registry)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?go-fs=registry" {
		t.Errorf("a registry-only session got %d to %q, want 303 to the registry page",
			res.StatusCode, res.Header.Get("Location"))
	}
}

func TestOnlyThePlainMainURLAsks(t *testing.T) {
	server := newPrivateRegistryServer(t, nil)
	server.mkdir(t, "private")
	for path, want := range map[string]string{
		"/?sort=name": "/?go-fs=login&sort=name",
		"/private/":   "/private/?go-fs=login",
	} {
		res, _ := pageRequest(t, server, http.MethodGet, path, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want {
			t.Errorf("%s: answer = %d to %q, want 303 to %q", path, res.StatusCode, res.Header.Get("Location"), want)
		}
	}

	// a program is challenged, as it always was
	res, _ := get(t, server, "/")
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("a program got %d, want a 401 challenge", res.StatusCode)
	}
}

func TestAPublicRootIsStillItsListing(t *testing.T) {
	server := newRegistryServer(t, func(cfg *httpConfig) { cfg.PathsWithoutAuth = []string{"^/"} })
	server.write(t, "open.txt", "x")
	res, body := pageRequest(t, server, http.MethodGet, "/", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "open.txt") {
		t.Errorf("a public root got %d without its listing", res.StatusCode)
	}
}

// The main URL offers the admin interface where it is on and an account may
// open it, beside the files.
func TestTheMainURLOffersAdmin(t *testing.T) {
	server := adminServer(t, func(cfg *httpConfig) { cfg.PathsWithoutAuth = nil })
	res, body := pageRequest(t, server, http.MethodGet, "/", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	for _, want := range []string{`href="/?go-fs=login"`, `href="/?go-fs=admin"`, ">Admin<", `id="i-gear"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %s:\n%s", want, body)
		}
	}

	// the tile leads to the login, which returns to the interface
	res = browserGet(t, server, "/?go-fs=admin")
	if location := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther ||
		!strings.Contains(location, "go-fs=login") {
		t.Errorf("the admin tile answered %d to %q, want 303 to the login", res.StatusCode, location)
	}
}

func TestTheMainURLOffersNoAdminWhereThereIsNone(t *testing.T) {
	cases := map[string]func(*httpConfig){
		"switched off": func(cfg *httpConfig) { cfg.EnableAdminInterface = false },
		"no admin account": func(cfg *httpConfig) {
			cfg.Users = []config.User{fullUser("john", "doe")}
		},
	}
	for name, tune := range cases {
		t.Run(name, func(t *testing.T) {
			server := adminServer(t, func(cfg *httpConfig) {
				cfg.PathsWithoutAuth = nil
				tune(cfg)
			})
			// with the files alone to offer, / goes straight to their login
			res, _ := pageRequest(t, server, http.MethodGet, "/", nil)
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?go-fs=login" {
				t.Errorf("answer = %d to %q, want 303 to the login", res.StatusCode, res.Header.Get("Location"))
			}
		})
	}
}
