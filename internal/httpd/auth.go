package httpd

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/secrets"
)

// account is one resolved entry of http.users: its credentials, the paths it
// may reach and what it may do there.
type account struct {
	name     string
	password string
	paths    []*regexp.Regexp

	upload bool
	delete bool

	cookie     bool
	cookiePath string
}

// allows reports whether this account may reach a normalized request path.
func (a *account) allows(virtual string) bool {
	for _, pattern := range a.paths {
		if pattern.MatchString(virtual) {
			return true
		}
	}
	return false
}

func buildAccounts(users []config.HTTPUser) ([]*account, error) {
	accounts := make([]*account, 0, len(users))
	for i, user := range users {
		resolved := &account{
			name:       user.Username,
			password:   user.Password,
			upload:     user.AllowUserFileUpload,
			delete:     user.AllowUserFileDelete,
			cookie:     user.Cookie,
			cookiePath: user.CookiePath,
		}
		if resolved.cookiePath == "" {
			resolved.cookiePath = "/"
		}
		for k, pattern := range user.Paths {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("http.users[%d].paths[%d]: %w", i, k, err)
			}
			resolved.paths = append(resolved.paths, compiled)
		}
		accounts = append(accounts, resolved)
	}
	return accounts, nil
}

// sessions hands out cookies that stand in for a set of credentials, so that a
// browser does not have to repeat them. A session names the account it was
// issued to; its rights are looked up again on every request, so a session can
// never reach further than the account behind it.
type sessions struct {
	mu       sync.Mutex
	lifetime time.Duration
	live     map[string]*sessionEntry
}

type sessionEntry struct {
	user    *account
	expires time.Time
}

func newSessions(lifetime time.Duration) *sessions {
	return &sessions{lifetime: lifetime, live: make(map[string]*sessionEntry)}
}

// issue mints a token for an account.
func (s *sessions) issue(user *account) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	s.live[token] = &sessionEntry{user: user, expires: time.Now().Add(s.lifetime)}
	return token, nil
}

// lookup resolves a token, and reports nothing for one that has expired.
func (s *sessions) lookup(token string) *account {
	s.mu.Lock()
	defer s.mu.Unlock()
	found, ok := s.live[token]
	if !ok {
		return nil
	}
	if time.Now().After(found.expires) {
		delete(s.live, token)
		return nil
	}
	return found.user
}

// prune drops what has expired. The Node implementation never did, so its map
// only ever grew.
func (s *sessions) prune() {
	now := time.Now()
	for token, found := range s.live {
		if now.After(found.expires) {
			delete(s.live, token)
		}
	}
}

// setLifetime changes how long a new session is good for.
func (s *sessions) setLifetime(lifetime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifetime = lifetime
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// authenticate resolves the account behind a request.
//
// The two return values are the account and whether the request may proceed. A
// public request proceeds with no account at all; anything else has to present
// credentials or a live session.
func (s *Server) authenticate(set *settings, w http.ResponseWriter, r *http.Request, virtual string) (*account, bool) {
	if !s.needsAuth(set, r.Method, virtual) {
		return nil, true
	}

	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if user := s.sessions.lookup(cookie.Value); user != nil {
			return user, true
		}
	}

	header := r.Header.Get("Authorization")
	var user *account
	switch {
	case strings.HasPrefix(header, "Digest "):
		user = s.checkDigest(set, r, header)
	case strings.HasPrefix(header, "Basic "):
		user = s.checkBasic(set, header)
	}

	if user == nil {
		s.challenge(set, w, r)
		return nil, false
	}

	s.log.Info("http login", "user", user.name, "address", addressOf(r), "path", virtual)
	if user.cookie && r.Header.Get("X-Disable-Session") == "" {
		s.setSession(set, w, r, user)
	}
	return user, true
}

// needsAuth decides whether credentials are required at all: because of the
// method, or because the path is one of the protected ones.
func (s *Server) needsAuth(set *settings, method, virtual string) bool {
	for _, protected := range set.cfg.MethodsRequireAuth {
		if strings.EqualFold(protected, method) {
			return true
		}
	}
	for _, pattern := range set.protectedPaths {
		if pattern.MatchString(virtual) {
			return true
		}
	}
	return false
}

func (s *Server) setSession(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	token, err := s.sessions.issue(user)
	if err != nil {
		s.log.Error("http cannot create a session", "error", err)
		return
	}
	s.log.Debug("http session created", "user", user.name, "path", user.cookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     user.cookiePath,
		MaxAge:   set.cfg.SessionTimeout,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// challenge answers a request that could not be authenticated. Digest is what
// is offered, as the Node implementation offers it, and Basic is accepted from
// a client that sends it anyway.
func (s *Server) challenge(set *settings, w http.ResponseWriter, r *http.Request) {
	if delay := set.cfg.LoginFailureDelay; delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
	opaque := make([]byte, 16)
	_, _ = rand.Read(opaque)
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Digest realm=%q, qop="auth", opaque=%q, nonce=%q, algorithm=%s`,
		set.cfg.Realm, hex.EncodeToString(opaque), s.nonce,
		defaultAlgorithm(r.Header.Get("User-Agent"))))
	w.WriteHeader(http.StatusUnauthorized)
}

// checkBasic verifies an RFC 7617 header.
func (s *Server) checkBasic(set *settings, header string) *account {
	name, password, ok := parseBasic(header)
	if !ok {
		return nil
	}
	for _, user := range set.accounts {
		if secrets.Match(name, user.name) && secrets.Match(password, user.password) {
			return user
		}
	}
	return nil
}

// checkDigest verifies an RFC 7616 header, including the RFC 2069 form that
// carries no qop.
func (s *Server) checkDigest(set *settings, r *http.Request, header string) *account {
	params := parseDigest(header)
	algorithm := params["algorithm"]
	if algorithm == "" {
		algorithm = defaultAlgorithm(r.Header.Get("User-Agent"))
	}
	digest, ok := hasher(algorithm)
	if !ok {
		return nil
	}
	if params["nonce"] != s.nonce {
		// a stale nonce is a failed attempt; the challenge that follows
		// carries the current one
		return nil
	}

	name := params["username"]
	uri := params["uri"]
	ha2 := digest(r.Method + ":" + uri)

	for _, user := range set.accounts {
		if user.name != name {
			continue
		}
		ha1 := digest(user.name + ":" + set.cfg.Realm + ":" + user.password)

		var expected string
		if params["qop"] == "auth" {
			expected = digest(strings.Join([]string{
				ha1, params["nonce"], params["nc"], params["cnonce"], params["qop"], ha2,
			}, ":"))
		} else {
			expected = digest(ha1 + ":" + params["nonce"] + ":" + ha2)
		}
		if secrets.Match(expected, params["response"]) {
			return user
		}
	}
	return nil
}

// hasher returns the hash the named algorithm calls for. MD5 and SHA-256 are
// the two browsers use; anything else is refused rather than guessed at.
func hasher(algorithm string) (func(string) string, bool) {
	switch strings.ToUpper(strings.TrimSuffix(algorithm, "-sess")) {
	case "MD5":
		return func(in string) string {
			sum := md5.Sum([]byte(in))
			return hex.EncodeToString(sum[:])
		}, true
	case "SHA-256", "SHA256":
		return func(in string) string {
			sum := sha256.Sum256([]byte(in))
			return hex.EncodeToString(sum[:])
		}, true
	}
	return nil, false
}

// defaultAlgorithm picks what to challenge with when the client has not said.
// Chromium and Firefox do SHA-256; everything else is offered MD5, which every
// client understands.
func defaultAlgorithm(userAgent string) string {
	lower := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lower, "chrome/"), strings.Contains(lower, "chromium/"),
		strings.Contains(lower, "firefox/"):
		return "SHA-256"
	default:
		return "MD5"
	}
}

// parseBasic splits a Basic header into its two halves.
func parseBasic(header string) (name, password string, ok bool) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return "", "", false
	}
	name, password, found := strings.Cut(string(decoded), ":")
	return name, password, found
}

// parseDigest reads the comma separated parameters of a Digest header.
//
// It walks the string rather than splitting it, because a value may hold a
// comma or an equals sign: a uri with a query string does, and the Node
// implementation mangled those.
func parseDigest(header string) map[string]string {
	params := make(map[string]string)
	rest := strings.TrimPrefix(header, "Digest ")

	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return params
		}
		key, after, found := strings.Cut(rest, "=")
		if !found {
			return params
		}
		key = strings.TrimSpace(key)
		after = strings.TrimLeft(after, " \t")

		var value string
		if strings.HasPrefix(after, `"`) {
			// a quoted value ends at the next unescaped quote
			var quoted strings.Builder
			index := 1
			for index < len(after) {
				if after[index] == '\\' && index+1 < len(after) {
					quoted.WriteByte(after[index+1])
					index += 2
					continue
				}
				if after[index] == '"' {
					index++
					break
				}
				quoted.WriteByte(after[index])
				index++
			}
			value, rest = quoted.String(), after[index:]
		} else {
			value, rest, _ = strings.Cut(after, ",")
			value = strings.TrimSpace(value)
		}
		params[key] = value
	}
}
