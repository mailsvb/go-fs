package httpd

import (
	"net/http"
	"strings"
	"testing"

	"go-fs/internal/config"
)

// A path that is not protected and a method that is not protected are served
// to anyone, which is what the Node implementation does.
func TestPublicRequestNeedsNoCredentials(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.MethodsRequireAuth = []string{"PUT", "DELETE"}
	})
	server.write(t, "public/hello.txt", "hello")

	res, body := get(t, server, "/public/hello.txt")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body != "hello" {
		t.Errorf("body = %q", body)
	}
}

func TestProtectedPathIsChallenged(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")

	res, _ := get(t, server, "/private/secret.txt")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	challenge := res.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Digest ") {
		t.Fatalf("challenge = %q", challenge)
	}
	for _, want := range []string{`realm="go-fs"`, `qop="auth"`, "nonce=", "opaque=", "algorithm="} {
		if !strings.Contains(challenge, want) {
			t.Errorf("challenge %q is missing %s", challenge, want)
		}
	}
}

// A protected method is challenged whatever the path.
func TestProtectedMethodIsChallenged(t *testing.T) {
	server := newServer(t, nil)

	req, _ := http.NewRequest(http.MethodDelete, server.url("/public/x.txt"), nil)
	if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

func TestBasicAuthentication(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")

	if res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Errorf("the right password got %d", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "wrong", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password got %d, want 401", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/private/secret.txt", "stranger", "doe", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unknown user got %d, want 401", res.StatusCode)
	}
}

// Digest for both algorithms browsers use, and for the qop-less RFC 2069 form.
func TestDigestAuthentication(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")

	for _, agent := range []struct {
		name          string
		userAgent     string
		wantAlgorithm string
	}{
		{"chrome asks for sha-256", "Mozilla/5.0 Chrome/120.0", "SHA-256"},
		{"firefox asks for sha-256", "Mozilla/5.0 Firefox/121.0", "SHA-256"},
		{"anything else gets md5", "curl/8.4.0", "MD5"},
	} {
		t.Run(agent.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
			req.Header.Set("User-Agent", agent.userAgent)
			challenge := do(t, req)
			params := parseDigest(challenge.Header.Get("WWW-Authenticate"))
			if params["algorithm"] != agent.wantAlgorithm {
				t.Fatalf("algorithm = %q, want %q", params["algorithm"], agent.wantAlgorithm)
			}

			res := digestRequest(t, server, http.MethodGet, "/private/secret.txt",
				"john", "doe", agent.userAgent, nil)
			if res.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", res.StatusCode)
			}
		})
	}

	t.Run("without qop, as RFC 2069 has it", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
		challenge := do(t, req)
		params := parseDigest(challenge.Header.Get("WWW-Authenticate"))

		req, _ = http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
		req.Header.Set("Authorization",
			digestHeader(t, params, http.MethodGet, "/private/secret.txt", "john", "doe", false))
		if res := do(t, req); res.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", res.StatusCode)
		}
	})

	t.Run("a wrong password is refused", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
		challenge := do(t, req)
		params := parseDigest(challenge.Header.Get("WWW-Authenticate"))

		req, _ = http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
		req.Header.Set("Authorization",
			digestHeader(t, params, http.MethodGet, "/private/secret.txt", "john", "wrong", true))
		if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.StatusCode)
		}
	})
}

// The session cookie stands in for the credentials on the next request.
func TestSessionCookie(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		user := fullUser("john", "doe")
		user.Cookie = true
		user.CookiePath = "/private/"
		cfg.Users = []config.HTTPUser{user}
	})
	server.write(t, "private/secret.txt", "secret")

	res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "doe", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var session *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == sessionCookie {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set")
	}
	if session.Path != "/private/" || !session.HttpOnly {
		t.Errorf("cookie = %+v", session)
	}

	// and it works on its own, with no credentials at all
	req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
	req.AddCookie(session)
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Errorf("the session was not accepted: %d", res.StatusCode)
	}
}

func TestSessionCookieIsOptional(t *testing.T) {
	server := newServer(t, nil) // john does not ask for a cookie
	server.write(t, "private/secret.txt", "secret")

	res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "doe", nil)
	if len(res.Cookies()) != 0 {
		t.Errorf("an account without cookie = true got %v", res.Cookies())
	}
}

// A client that says it does not want a session does not get one.
func TestDisableSessionHeader(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		user := fullUser("john", "doe")
		user.Cookie = true
		cfg.Users = []config.HTTPUser{user}
	})
	server.write(t, "private/secret.txt", "secret")

	req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
	req.SetBasicAuth("john", "doe")
	req.Header.Set("X-Disable-Session", "1")
	res := do(t, req)
	if len(res.Cookies()) != 0 {
		t.Errorf("got %v, want no cookie", res.Cookies())
	}
}

// A session carries the rights of the account it was issued to and nothing
// more, so it cannot be used to reach a path that account may not reach.
func TestSessionKeepsTheAccountsLimits(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		cfg.PathsRequireAuth = []string{"^/private/.*", "^/other/.*"}
		cfg.Users = []config.HTTPUser{{
			Username: "john", Password: "doe",
			Paths:  []string{"^/private/.*"},
			Cookie: true,
		}}
	})
	server.write(t, "private/secret.txt", "secret")
	server.write(t, "other/secret.txt", "other")

	res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "doe", nil)
	session := res.Cookies()[0]

	req, _ := http.NewRequest(http.MethodGet, server.url("/other/secret.txt"), nil)
	req.AddCookie(session)
	if res := do(t, req); res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
}

// The header parser has to survive values that hold a comma or an equals sign,
// which the Node implementation mangled.
func TestParseDigestHeader(t *testing.T) {
	header := `Digest username="john", realm="go-fs", ` +
		`uri="/a/b.php?dir=x,y&z=1", qop=auth, nc=00000001, ` +
		`cnonce="abc\"def", response="deadbeef", algorithm=SHA-256`
	params := parseDigest(header)

	for key, want := range map[string]string{
		"username":  "john",
		"realm":     "go-fs",
		"uri":       "/a/b.php?dir=x,y&z=1",
		"qop":       "auth",
		"nc":        "00000001",
		"cnonce":    `abc"def`,
		"response":  "deadbeef",
		"algorithm": "SHA-256",
	} {
		if params[key] != want {
			t.Errorf("%s = %q, want %q", key, params[key], want)
		}
	}
}

func TestHasherMatchesTheNamedAlgorithm(t *testing.T) {
	md5Hash, ok := hasher("MD5")
	if !ok || md5Hash("go-fs") != md5Hex("go-fs") {
		t.Error("MD5 is wrong")
	}
	shaHash, ok := hasher("SHA-256")
	if !ok || shaHash("go-fs") != sha256Hex("go-fs") {
		t.Error("SHA-256 is wrong")
	}
	if _, ok := hasher("SHA-512"); ok {
		t.Error("an algorithm that is not offered has to be refused")
	}
}

// A session names the account and nothing more, so a right taken away by a
// reload reaches a browser that already holds a cookie, and an account that is
// gone takes its sessions with it.
func TestSessionFollowsTheAccount(t *testing.T) {
	server := newServer(t, func(cfg *config.HTTP) {
		user := fullUser("john", "doe")
		user.Cookie = true
		cfg.Users = []config.HTTPUser{user}
	})
	server.write(t, "private/secret.txt", "secret")

	res := basic(t, server, http.MethodGet, "/private/secret.txt", "john", "doe", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var session *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == sessionCookie {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("no session cookie was issued")
	}

	withCookie := func(path string) int {
		req, err := http.NewRequest(http.MethodGet, server.url(path), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(session)
		return do(t, req).StatusCode
	}
	if got := withCookie("/private/secret.txt"); got != http.StatusOK {
		t.Fatalf("the cookie should work, got %d", got)
	}

	// the account keeps its name but loses the path it could reach
	next := server.settings().cfg
	narrowed := fullUser("john", "doe")
	narrowed.Cookie = true
	narrowed.Paths = []string{"^/public/.*"}
	next.Users = []config.HTTPUser{narrowed}
	if err := server.Reload(next, server.settings().https); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := withCookie("/private/secret.txt"); got != http.StatusForbidden {
		t.Errorf("the narrowed account should be refused, got %d", got)
	}

	// and an account that is gone leaves nothing behind for its cookie to name
	next.Users = []config.HTTPUser{fullUser("someone else", "doe")}
	if err := server.Reload(next, server.settings().https); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := withCookie("/private/secret.txt"); got != http.StatusUnauthorized {
		t.Errorf("the removed account's cookie should be refused, got %d", got)
	}
}

// A pattern that covers what is in a folder covers the folder itself: the
// listing of a protected folder names every file in it, so it cannot be the one
// public thing about it.
func TestProtectedFolderListingNeedsAuth(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")

	for _, path := range []string{"/private", "/private/"} {
		res, body := get(t, server, path)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401; body %q", path, res.StatusCode, body)
		}
	}

	// the account whose pattern it is can still read it
	res := basic(t, server, http.MethodGet, "/private/", "john", "doe", nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("the account that owns the path got %d", res.StatusCode)
	}
}

// The digest response is computed over the uri in the header, so that uri has
// to be the one being asked for. Without the check a header captured on one
// path would authorize any other path with the same method.
func TestDigestIsBoundToThePath(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")
	server.write(t, "private/other.txt", "other")

	first, err := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	challenge := do(t, first)
	params := parseDigest(challenge.Header.Get("WWW-Authenticate"))
	header := digestHeader(t, params, http.MethodGet, "/private/secret.txt", "john", "doe", true)

	// the header it was made for
	req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
	req.Header.Set("Authorization", header)
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Fatalf("the header has to work on its own path, got %d", res.StatusCode)
	}

	// the same header on another path
	req, _ = http.NewRequest(http.MethodGet, server.url("/private/other.txt"), nil)
	req.Header.Set("Authorization", header)
	if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a captured header worked on another path, got %d", res.StatusCode)
	}
}

// A nonce this server did not issue is not accepted, whatever the response
// computed over it says.
func TestDigestRefusesAForeignNonce(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/secret.txt", "secret")

	params := map[string]string{
		"realm":     server.settings().cfg.Realm,
		"nonce":     "1700000000:" + strings.Repeat("a", 64),
		"algorithm": "MD5",
	}
	req, _ := http.NewRequest(http.MethodGet, server.url("/private/secret.txt"), nil)
	req.Header.Set("Authorization",
		digestHeader(t, params, http.MethodGet, "/private/secret.txt", "john", "doe", true))
	if res := do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}
