package httpd

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/mailsvb/go-fs/internal/config"
)

// registryTokenServer is a registry with one go-fs bearer token, which sets
// allowRegistry unless tune says otherwise.
func registryTokenServer(t *testing.T, tune func(*httpConfig, *config.Token)) (*testServer, string) {
	t.Helper()
	token, plain := fullToken(t, "ci")
	token.AllowRegistry = new(true)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		if tune != nil {
			tune(cfg, &token)
		}
		cfg.Tokens = []config.Token{token}
	})
	return server, plain
}

// pushAs uploads a blob and pushes a manifest of an image whose other blobs
// the pusher uploaded, all with the credentials given.
func (s *testServer) pushAs(t *testing.T, repo string, auth reqOption) {
	t.Helper()
	content := []byte("pushed by a token")
	d := digest.FromBytes(content)
	res, body := s.reg(t, http.MethodPost, "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), content, auth)
	expectStatus(t, res, body, http.StatusCreated)
	img := s.buildImage(t, repo, "linux/amd64", "token", false)
	res, body = s.reg(t, http.MethodPut, "/v2/"+repo+"/manifests/1.0", img.manifest, auth,
		withHeader("Content-Type", img.mediaType))
	expectStatus(t, res, body, http.StatusCreated)
}

func expectCatalogHas(t *testing.T, s *testServer, repo string, auth reqOption) {
	t.Helper()
	res, body := s.reg(t, http.MethodGet, "/v2/_catalog", nil, auth)
	expectStatus(t, res, body, http.StatusOK)
	if !strings.Contains(string(body), `"`+repo+`"`) {
		t.Errorf("the catalog does not list %s: %s", repo, body)
	}
}

// docker login -u anything -p <token>: the token endpoint takes the token as
// the password and the registry token it hands out pushes.
func TestRegistryTokenAsDockerLoginPassword(t *testing.T) {
	server, plain := registryTokenServer(t, func(cfg *httpConfig, _ *config.Token) {
		cfg.RegistryAnonymousRead = false
	})
	res, jwt := server.fetchToken(t, "repository:ci/app:pull,push", as("whoever", plain))
	expectStatus(t, res, nil, http.StatusOK)
	server.pushAs(t, "ci/app", bearerToken(jwt))
	expectCatalogHas(t, server, "ci/app", bearerToken(jwt))
	if server.logs.find("http login refused") != nil {
		t.Error("a token login was recorded as refused")
	}
}

// curl -H "Authorization: Bearer <token>" and curl -u x:<token> work on every
// registry request, without the token endpoint.
func TestRegistryTokenOnEveryRequest(t *testing.T) {
	server, plain := registryTokenServer(t, func(cfg *httpConfig, _ *config.Token) {
		cfg.RegistryAnonymousRead = false
	})
	server.pushAs(t, "ci/raw", bearerToken(plain))
	expectCatalogHas(t, server, "ci/raw", bearerToken(plain))
	server.pushAs(t, "ci/basic", as("x", plain))
	expectCatalogHas(t, server, "ci/basic", as("x", plain))
}

// A token that does not set allowRegistry is like an account that does not
// set registry: it pulls where anyone may and pushes nowhere.
func TestRegistryTokenWithoutAllowRegistry(t *testing.T) {
	for _, anonymousRead := range []bool{true, false} {
		server, plain := registryTokenServer(t, func(cfg *httpConfig, token *config.Token) {
			cfg.RegistryAnonymousRead = anonymousRead
			token.AllowRegistry = nil
		})
		for name, auth := range map[string]reqOption{"bearer": bearerToken(plain), "basic": as("x", plain)} {
			res, body := server.reg(t, http.MethodPost, "/v2/ci/app/blobs/uploads/", nil, auth)
			expectStatus(t, res, body, http.StatusForbidden)
			res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, auth)
			want := http.StatusOK
			if !anonymousRead {
				want = http.StatusForbidden
			}
			if res.StatusCode != want {
				t.Errorf("anonymousRead %v, %s: the catalog got %d, want %d: %s", anonymousRead, name, res.StatusCode, want, body)
			}
		}
		res, jwt := server.fetchToken(t, "repository:ci/app:push", as("x", plain))
		if anonymousRead {
			expectStatus(t, res, nil, http.StatusOK)
			res, body := server.reg(t, http.MethodPost, "/v2/ci/app/blobs/uploads/", nil, bearerToken(jwt))
			expectStatus(t, res, body, http.StatusUnauthorized)
		} else {
			expectStatus(t, res, nil, http.StatusForbidden)
		}
		if server.logs.find("http login refused") != nil {
			t.Errorf("anonymousRead %v: a real token was recorded as a refused login", anonymousRead)
		}
	}
}

// An expired token is refused everywhere, without counting against the
// address; an unknown one counts.
func TestRegistryExpiredAndUnknownTokens(t *testing.T) {
	server, plain := registryTokenServer(t, func(cfg *httpConfig, token *config.Token) {
		cfg.LoginAttempts = 2
		cfg.LoginLockout = 60
		token.Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	})
	fixClock(server)
	for range 3 {
		res, _ := server.fetchToken(t, "repository:ci/app:push", as("x", plain))
		expectStatus(t, res, nil, http.StatusUnauthorized)
		res, body := server.reg(t, http.MethodPost, "/v2/ci/app/blobs/uploads/", nil, bearerToken(plain))
		expectStatus(t, res, body, http.StatusUnauthorized)
		res, body = server.reg(t, http.MethodPost, "/v2/ci/app/blobs/uploads/", nil, as("x", plain))
		expectStatus(t, res, body, http.StatusUnauthorized)
	}
	if server.logs.find("http bearer token expired") == nil {
		t.Error("the expired token was not logged")
	}
	if server.logs.find("http login refused") != nil {
		t.Error("an expired token was recorded as a refused login")
	}
	// the address is not locked: a registry account still logs in
	res, body := server.reg(t, http.MethodGet, "/v2/_catalog", nil, asPusher)
	expectStatus(t, res, body, http.StatusOK)

	for range 2 {
		res, body := server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken("gofs_wrong"))
		expectStatus(t, res, body, http.StatusUnauthorized)
	}
	res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, asPusher)
	expectStatus(t, res, body, http.StatusTooManyRequests)
}

// A registry token minted for a token ends when the token is replaced, or
// loses allowRegistry, on a reload.
func TestRegistryTokenEndsWithItsToken(t *testing.T) {
	token, plain := fullToken(t, "ci")
	token.AllowRegistry = new(true)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.RegistryAnonymousRead = false
		cfg.Tokens = []config.Token{token}
	})
	_, jwt := server.fetchToken(t, "registry:catalog:*", as("x", plain))
	res, body := server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken(jwt))
	expectStatus(t, res, body, http.StatusOK)
	// the account's JWT, whose fingerprint has no token hash, still works
	_, accountJWT := server.fetchToken(t, "registry:catalog:*", asPusher)

	reload := func(tokens ...config.Token) {
		t.Helper()
		set := server.settings()
		if err := server.Reload(set.cfg, set.https, set.ssh, []config.User{fullUser("john", "doe"),
			registryUser(pusher, pusherPassword)}, tokens, nil); err != nil {
			t.Fatal(err)
		}
	}
	reload(token)
	res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken(jwt))
	expectStatus(t, res, body, http.StatusOK)

	without := token
	without.AllowRegistry = nil
	reload(without)
	res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken(jwt))
	expectStatus(t, res, body, http.StatusUnauthorized)

	replaced, _ := fullToken(t, "ci")
	replaced.AllowRegistry = new(true)
	reload(replaced)
	res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken(jwt))
	expectStatus(t, res, body, http.StatusUnauthorized)

	res, body = server.reg(t, http.MethodGet, "/v2/_catalog", nil, bearerToken(accountJWT))
	expectStatus(t, res, body, http.StatusOK)
}
