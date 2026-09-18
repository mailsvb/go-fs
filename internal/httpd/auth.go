package httpd

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/secrets"
	"go-fs/internal/vfs"
)

// account is one resolved entry of [[users]] that sets http: its credentials,
// the paths it may reach and what it may do there.
type account struct {
	name     string
	password string
	paths    []*regexp.Regexp
	perms    config.Permissions

	cookie     bool
	cookiePath string
	// isAdmin lets the account into the admin interface, once it holds a
	// session.
	isAdmin bool
}

// allows reports whether this account may reach a normalized request path.
func (a *account) allows(virtual string) bool {
	return matchesPath(a.paths, virtual)
}

// matchesPath reports whether any of the patterns covers a request path.
//
// A folder is reached both as "/private" and as "/private/", and normalizing
// strips the trailing slash, so both forms are tested. Otherwise the documented
// "^/private/.*" would cover everything in the folder but not the listing of
// the folder itself, which names every file in it — and an account scoped to
// that pattern could read the files but not see the folder they are in.
func matchesPath(patterns []*regexp.Regexp, virtual string) bool {
	folder := vfs.AsFolder(virtual)
	for _, pattern := range patterns {
		if pattern.MatchString(virtual) || pattern.MatchString(folder) {
			return true
		}
	}
	return false
}

// buildAccounts resolves the accounts once, at startup and on every reload. An
// error names the account rather than its position: the list is the file's
// filtered down to this server. The base folder and the anonymous login of an
// entry belong to the other servers and are not read here.
func buildAccounts(users []config.User) ([]*account, error) {
	accounts := make([]*account, 0, len(users))
	seen := map[string]bool{}
	for _, user := range users {
		// a session names its account, and every request resolves that name
		// again, so two entries with the same name would make which rights
		// apply a matter of order
		if seen[user.Username] {
			return nil, fmt.Errorf("users %q is configured twice", user.Username)
		}
		seen[user.Username] = true
		resolved := &account{
			name:       user.Username,
			password:   user.Password,
			perms:      user.Permissions(),
			cookie:     user.Cookie,
			cookiePath: user.CookiePath,
			isAdmin:    user.IsAdmin,
		}
		if resolved.cookiePath == "" {
			resolved.cookiePath = "/"
		}
		for k, pattern := range user.Paths {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("users %q paths[%d]: %w", user.Username, k, err)
			}
			resolved.paths = append(resolved.paths, compiled)
		}
		accounts = append(accounts, resolved)
	}
	return accounts, nil
}

// credential is who a request came from and how it said so.
type credential struct {
	user *account
	// token says the identity came from a session token rather than from a
	// header, which is what decides whether there is anything to log out of.
	token bool
	// stale says a digest nonce had aged out while the credentials were right,
	// which is what lets a browser answer again by itself.
	stale bool
	// method is how the request identified itself, for the records: basic,
	// digest, token, none, or unsupported for a scheme this server does not
	// speak.
	method string
}

// authenticate resolves the account behind a request.
//
// The two return values are the credential and whether the request may
// proceed. A public request proceeds with no account at all; anything else has
// to present credentials or a live session token.
func (s *Server) authenticate(set *settings, w http.ResponseWriter, r *http.Request, virtual string) (credential, bool) {
	// who the request is from is resolved for every request, public ones
	// included: the page shows who is signed in, and it cannot do that if a
	// public folder discards the identity before looking at it
	cred := s.identify(set, w, r)
	if cred.user != nil {
		s.log.Debug("http authenticated", "user", cred.user.name, "method", cred.method,
			"address", addressOf(r), "path", virtual)
		return cred, true
	}
	if !s.needsAuth(set, r.Method, virtual) {
		return cred, true
	}
	s.log.Debug("http authentication required", "method", r.Method, "path", virtual,
		"address", addressOf(r), "credentials", cred.method, "stale", cred.stale)
	s.refuse(set, w, r, cred.stale)
	return credential{}, false
}

// identify reads whatever credentials a request carries, without deciding
// whether it needed any.
//
// The Authorization header is read before the cookie. An explicit header is
// the client saying who it is now; a cookie is ambient, sent by the browser
// whether or not anyone meant it to be, and ambient credentials must never
// shadow explicit ones.
func (s *Server) identify(set *settings, w http.ResponseWriter, r *http.Request) credential {
	header := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(header, "Digest "):
		user, stale := s.checkDigest(set, r, header)
		return credential{user: user, stale: stale, method: "digest"}
	case strings.HasPrefix(header, "Basic "):
		return credential{user: s.checkBasic(set, r, header), method: "basic"}
	case header != "":
		scheme, _, _ := strings.Cut(header, " ")
		s.log.Debug("http authorization scheme not supported", "scheme", scheme,
			"address", addressOf(r))
		return credential{method: "unsupported"}
	}

	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return credential{method: "none"}
	}
	if user := s.checkToken(set, w, r, cookie.Value); user != nil {
		return credential{user: user, token: true, method: "token"}
	}
	return credential{method: "token"}
}

// checkToken verifies a session token against the accounts as they are
// configured right now.
//
// Every way of failing clears the cookie and returns nothing rather than
// refusing the request outright, so the request falls through to whatever else
// it carries: a browser that presents a token for an account that has been
// removed is simply signed out, and lands on the login page.
func (s *Server) checkToken(set *settings, w http.ResponseWriter, r *http.Request, raw string) *account {
	claims, err := s.tokens.read(raw)
	if err != nil {
		s.log.Debug("http refused a session token", "error", err, "address", addressOf(r))
		s.clearSession(w, "/")
		return nil
	}
	user := accountNamed(set.accounts, claims.Subject)
	if user == nil {
		s.log.Info("http session token names an account that is no longer configured",
			"user", claims.Subject, "address", addressOf(r))
		s.clearSession(w, "/")
		return nil
	}
	if !user.cookie {
		s.log.Info("http session token names an account that may no longer log in",
			"user", user.name, "address", addressOf(r))
		s.clearSession(w, user.cookiePath)
		return nil
	}
	if !s.tokens.issuedFor(claims, user) {
		s.log.Info("http session token was issued for other credentials",
			"user", user.name, "address", addressOf(r))
		s.clearSession(w, user.cookiePath)
		return nil
	}
	return user
}

// refuse answers a request that could not be authenticated.
//
// A program is challenged as it always was. A browser is sent to the login
// page instead, because the one header that must not be sent to it is
// WWW-Authenticate: that is what raises the browser's own password box, and
// this server now has a page of its own to ask on. RFC 9110 section 15.5.2
// makes that header mandatory on a 401, which is why the answer is a redirect
// to a page that comes back 200 rather than a 401 carrying HTML.
func (s *Server) refuse(set *settings, w http.ResponseWriter, r *http.Request, stale bool) {
	if !browserRequest(r) || !canLogIn(set) {
		s.challenge(set, w, r, stale)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, loginURL(r.URL), http.StatusSeeOther)
		return
	}
	// a fetch from the page cannot follow a redirect to a login form usefully,
	// and a challenge here would raise the password box the login page exists
	// to avoid, so it is told plainly that the session is gone
	http.Error(w, "Not authenticated", http.StatusUnauthorized)
}

// browserRequest reports whether the client is a browser.
//
// Sec-Fetch-Mode is sent by every current browser on every request, including
// the ones this page's own script makes, and by no program; Accept covers a
// browser too old to send it. A client that says X-Disable-Session is taken at
// its word and treated as a program, which is what that header was for.
func browserRequest(r *http.Request) bool {
	if r.Header.Get("X-Disable-Session") != "" {
		return false
	}
	if r.Header.Get("Sec-Fetch-Mode") != "" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// canLogIn reports whether any account may use the login form. Where none may,
// there is no login page and nothing about this server has changed: a browser
// is challenged exactly as it was before.
func canLogIn(set *settings) bool {
	for _, user := range set.accounts {
		if user.cookie {
			return true
		}
	}
	return false
}

// accountNamed finds a configured account by name.
func accountNamed(accounts []*account, name string) *account {
	for _, user := range accounts {
		if user.name == name {
			return user
		}
	}
	return nil
}

// needsAuth decides whether credentials are required at all: because of the
// method, or because the path is one of the protected ones.
func (s *Server) needsAuth(set *settings, method, virtual string) bool {
	implied := impliedBy(method)
	for _, protected := range set.cfg.MethodsRequireAuth {
		if strings.EqualFold(protected, method) {
			return true
		}
		for _, older := range implied {
			if strings.EqualFold(protected, older) {
				return true
			}
		}
	}
	return matchesPath(set.protectedPaths, virtual)
}

// impliedBy is the older methods a newer one is protected along with.
//
// MKCOL and MOVE arrived after methodsRequireAuth had been written into the
// configurations that exist, so a file on disk names PUT and DELETE and not
// them. They are the same two writes under another verb — creating something,
// and creating it while removing what was there — so an upgrade that left them
// to be listed by hand would quietly open them to anyone.
func impliedBy(method string) []string {
	switch strings.ToUpper(method) {
	case methodMkcol:
		return []string{http.MethodPut}
	case methodMove:
		return []string{http.MethodPut, http.MethodDelete}
	default:
		return nil
	}
}

// action is what a request is about to do to a path. It is finer than the
// method: a PUT creates or replaces depending on what is there, and a DELETE
// removes a file or a folder, and those are different rights.
type action int

const (
	actRead         action = iota // GET, HEAD and the legacy POST reader
	actCreate                     // PUT on a name that is free
	actOverwrite                  // PUT on a name that is taken
	actMkdir                      // MKCOL
	actDeleteFile                 // DELETE of a file
	actDeleteFolder               // DELETE of a folder
	actRename                     // MOVE, for its source and its destination alike
)

func (a action) String() string {
	switch a {
	case actRead:
		return "read"
	case actCreate:
		return "create"
	case actOverwrite:
		return "overwrite"
	case actMkdir:
		return "mkdir"
	case actDeleteFile:
		return "delete"
	case actDeleteFolder:
		return "rmdir"
	case actRename:
		return "rename"
	}
	return "unknown"
}

// actionOf reads the target once, before the permission check, so that the
// check and the handler agree on what the request is: the handlers stat again
// for their own refusals, but the right is decided here.
func actionOf(method string, target vfs.Target) action {
	switch strings.ToUpper(method) {
	case http.MethodPut:
		if _, err := os.Stat(target.Path); err == nil {
			return actOverwrite
		}
		return actCreate
	case methodMkcol:
		return actMkdir
	case http.MethodDelete:
		if info, err := os.Stat(target.Path); err == nil && info.IsDir() {
			return actDeleteFolder
		}
		return actDeleteFile
	case methodMove:
		return actRename
	default:
		return actRead
	}
}

// permits reports whether the account behind a request may do act to a path.
// It is the one place the things that can refuse a request are put together:
// whether the method needs an account there at all, whether the account may
// reach the path, and the right the action needs.
//
// A request that needs no account for that method is permitted whoever is
// signed in. An identity adds to what a request may do and never takes
// anything away: on a server whose GET is public and whose PUT is not, logging
// in must not be what stops someone reading a folder. Replacing a file is the
// one exception: a public PUT never replaced what was there, and it still does
// not — only an account granted allowUserFileOverwrite may.
func (s *Server) permits(set *settings, user *account, method, virtual string, act action) bool {
	if act != actOverwrite && !s.needsAuth(set, method, virtual) {
		return true
	}
	if user == nil || !user.allows(virtual) {
		return false
	}
	perms := user.perms
	switch act {
	case actRead:
		return perms.FileRetrieve
	case actCreate:
		return perms.FileCreate
	case actOverwrite:
		return perms.FileOverwrite
	case actMkdir:
		return perms.FolderCreate
	case actDeleteFile:
		return perms.FileDelete
	case actDeleteFolder:
		return perms.FolderDelete
	case actRename:
		// renaming leaves a name behind and takes one away, so it needs both
		return perms.FileCreate && perms.FileDelete
	}
	return false
}

// rightsFor is what the account behind a request may do in a folder, which is
// what the listing page renders its controls from. Asking permits the same
// question the dispatch asks is what keeps the buttons on the page and the
// checks in ServeHTTP saying the same thing.
func (s *Server) rightsFor(set *settings, user *account, virtual string) rights {
	return rights{
		Upload:       s.permits(set, user, http.MethodPut, virtual, actCreate),
		Mkdir:        s.permits(set, user, methodMkcol, virtual, actMkdir),
		DeleteFile:   s.permits(set, user, http.MethodDelete, virtual, actDeleteFile),
		DeleteFolder: s.permits(set, user, http.MethodDelete, virtual, actDeleteFolder),
		Rename:       s.permits(set, user, methodMove, virtual, actRename),
	}
}

// sameSite guards the methods that change something, and the login and logout
// forms, against a request made by another site.
//
// A session token is ambient: the browser sends it whether or not anyone meant
// it to, which is what makes cross site requests worth refusing here. The
// cookie is SameSite=Lax, which already withholds it from every cross site
// request that is not a top level navigation, so this is a second lock rather
// than the only one.
//
// Two headers are looked at because neither is always there. Origin is sent by
// a fetch and by a cross site form post, but not by every same site form post;
// Sec-Fetch-Site is sent by every current browser on every request and by no
// program, so a client that sends neither — curl, a script — is left alone.
func (s *Server) sameSite(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !sameHost(origin, r.Host) {
		s.log.Warn("http refused a request from another origin",
			"origin", origin, "method", r.Method, "address", addressOf(r))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	// "none" is the browser saying nothing initiated this but the user
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "same-origin", "none":
		return true
	default:
		s.log.Warn("http refused a request from another site",
			"site", site, "method", r.Method, "address", addressOf(r))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
}

// sameHost reports whether an Origin header names the host the request was made
// to. The host is compared rather than the whole origin, so the check still
// holds when the server is reached through something that terminates TLS in
// front of it.
func sameHost(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host == host
}

// setSession hands a browser the token it carries from here on.
func (s *Server) setSession(set *settings, w http.ResponseWriter, r *http.Request, user *account) error {
	lifetime := time.Duration(set.cfg.SessionTokenLifetime) * time.Second
	token, expires, err := s.tokens.mint(user, lifetime)
	if err != nil {
		return err
	}
	s.log.Info("http login", "user", user.name, "address", addressOf(r),
		"path", user.cookiePath)
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  user.cookiePath,
		// both, because a client that ignores one honours the other
		MaxAge:   int(lifetime.Seconds()),
		Expires:  expires,
		HttpOnly: true,
		// there is no trusted proxy setting here and X-Forwarded-Proto is
		// written by the client, so honouring it would let a client talk this
		// server out of the flag
		Secure: r.TLS != nil,
		// Lax already withholds the cookie from every cross site request that
		// is not a top level navigation, which is the whole of the CSRF
		// surface: nothing here changes anything on a GET. Strict would also
		// withhold it from following a link to a protected folder, for nothing
		// in return.
		SameSite: http.SameSiteLaxMode,
	})
	// a browser upgraded into this version still holds the opaque session
	// cookie, which nothing will read again
	clearCookie(w, legacyCookie, user.cookiePath)
	return nil
}

// clearSession tells the browser to drop its session token. The path has to be
// the one the cookie was set with, or the browser keeps it and clears nothing.
func (s *Server) clearSession(w http.ResponseWriter, path string) {
	clearCookie(w, sessionCookie, path)
}

func clearCookie(w http.ResponseWriter, name, path string) {
	if path == "" {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// nonceLifetime is how long a digest nonce is accepted for. It bounds how long
// a captured Authorization header can be replayed; a client that still holds
// the credentials answers the stale challenge without asking anyone.
const nonceLifetime = 5 * time.Minute

// newNonce issues a nonce that carries the time it was made and a signature
// over it. Nothing has to be remembered: the timestamp says how old it is and
// the signature is what stops a client writing its own.
func (s *Server) newNonce() string {
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	return issued + ":" + s.signNonce(issued)
}

func (s *Server) signNonce(issued string) string {
	mac := hmac.New(sha256.New, s.nonceKey)
	mac.Write([]byte(issued))
	return hex.EncodeToString(mac.Sum(nil))
}

// nonceState reports whether a nonce was issued by this server, and whether it
// is still young enough to accept. One this server did not issue is not
// answered with a stale challenge, because there is nothing stale about it.
func (s *Server) nonceState(nonce string) (fresh, ours bool) {
	issued, signature, found := strings.Cut(nonce, ":")
	if !found {
		return false, false
	}
	expected := s.signNonce(issued)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return false, false
	}
	seconds, err := strconv.ParseInt(issued, 10, 64)
	if err != nil {
		return false, false
	}
	age := time.Since(time.Unix(seconds, 0))
	return age >= -time.Minute && age <= nonceLifetime, true
}

// challenge answers a request that could not be authenticated. Digest is what
// is offered, as the Node implementation offers it, and Basic is accepted from
// a client that sends it anyway.
//
// stale says the credentials were right and only the nonce had aged out, which
// is what lets a browser answer again by itself instead of asking for the
// password once every nonceLifetime.
func (s *Server) challenge(set *settings, w http.ResponseWriter, r *http.Request, stale bool) {
	if delay := set.cfg.LoginFailureDelay; delay > 0 && !stale {
		// a stale nonce is not a failed attempt, so it is not slowed down
		time.Sleep(time.Duration(delay) * time.Second)
	}
	opaque := make([]byte, 16)
	_, _ = rand.Read(opaque)
	challenge := fmt.Sprintf(
		`Digest realm=%q, qop="auth", opaque=%q, nonce=%q, algorithm=%s`,
		set.cfg.Realm, hex.EncodeToString(opaque), s.newNonce(),
		defaultAlgorithm(r.Header.Get("User-Agent")))
	if stale {
		challenge += ", stale=true"
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.WriteHeader(http.StatusUnauthorized)
}

// checkBasic verifies an RFC 7617 header.
func (s *Server) checkBasic(set *settings, r *http.Request, header string) *account {
	name, password, ok := parseBasic(header)
	if !ok {
		s.log.Debug("http basic credentials are malformed", "address", addressOf(r))
		return nil
	}
	for _, user := range set.accounts {
		if secrets.Match(name, user.name) && secrets.Match(password, user.password) {
			return user
		}
	}
	// the same record the login form writes, so every refused password is
	// found under one message whichever way it arrived
	s.log.Info("http login refused", "user", name, "method", "basic", "address", addressOf(r))
	return nil
}

// checkDigest verifies an RFC 7616 header, including the RFC 2069 form that
// carries no qop. The second result asks for a stale challenge: the credentials
// were right and only the nonce had aged out.
//
// The response is computed over the uri the client puts in the header, so that
// uri has to be the one being asked for. Without that check a header captured
// for a public path would authorize any other path with the same method, which
// is the whole point of taking it.
func (s *Server) checkDigest(set *settings, r *http.Request, header string) (*account, bool) {
	params := parseDigest(header)
	algorithm := params["algorithm"]
	if algorithm == "" {
		algorithm = defaultAlgorithm(r.Header.Get("User-Agent"))
	}
	digest, ok := hasher(algorithm)
	if !ok {
		s.log.Debug("http digest algorithm not supported", "algorithm", algorithm,
			"user", params["username"], "address", addressOf(r))
		return nil, false
	}
	// clients differ on whether the query string is part of it, so both forms
	// are accepted; either way the path is bound to the response
	uri := params["uri"]
	if uri != r.URL.RequestURI() && uri != r.URL.Path {
		s.log.Debug("http digest uri does not match the request", "uri", uri,
			"requested", r.URL.RequestURI(), "user", params["username"], "address", addressOf(r))
		return nil, false
	}
	fresh, ours := s.nonceState(params["nonce"])
	if !ours {
		// a nonce another server issued, or one from before a restart
		s.log.Debug("http digest nonce was not issued by this server",
			"user", params["username"], "address", addressOf(r))
		return nil, false
	}

	name := params["username"]
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
			if !fresh {
				// right credentials, old nonce: ask again with a new one
				// rather than letting it through
				s.log.Debug("http digest nonce is stale, challenging again",
					"user", name, "address", addressOf(r))
				return nil, true
			}
			return user, false
		}
	}
	s.log.Info("http login refused", "user", name, "method", "digest", "address", addressOf(r))
	return nil, false
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
