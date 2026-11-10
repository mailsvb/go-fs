//go:build !light && !no_subsonic

package httpd

import (
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/secrets"
	"github.com/mailsvb/go-fs/internal/store"
	"github.com/mailsvb/go-fs/internal/subsonic"
)

// While there is a subsonic repository, the server speaks the OpenSubsonic
// API for its music clients to browse the libraries, play the tracks and
// download them, at /rest, below the root of the server, as every other
// server of the API has it. A request is one of the API when its path is
// /rest/<method> or /rest/<method>.view and the method is one of the API's, or
// any path below /rest that ends in .view: so a browser still finds the files
// of a folder called rest, but for those few names.
//
// A client sends its credentials with every request, in the query or in the
// body of a form: the name and the password of an account that sets http,
// the password as it is, hex encoded, or as the MD5 of it and a salt the
// client picks, which go-fs can check because it keeps the passwords as they
// are; or, as OpenSubsonic adds, an API key, which is a bearer token of
// [[tokens]], with the paths and the rights that token has. What the account
// may see is looked up on every request, as for a Jellyfin client: the tracks
// it may read, and the albums, artists and genres of those.
//
// What a client stars and scrobbles is kept in the persistent data file, by
// account, apart from what Jellyfin clients keep.

// subsonicRoot is the folder the API is answered below.
const subsonicRoot = "/rest/"

// subsonicLoginLog is how long a login of an account by a client is not
// logged again: a client logs in with every request.
const subsonicLoginLog = time.Hour

// subsonicState is what the OpenSubsonic API keeps across requests.
type subsonicState struct {
	listeners *jellyfin.Listeners

	mu sync.Mutex
	// logins are when an account was last logged as logging in with a
	// client, by account and client.
	logins map[string]time.Time
}

func newSubsonicState(st *store.Store, logger *slog.Logger) *subsonicState {
	return &subsonicState{listeners: jellyfin.NewListeners(st, "subsonic", logger), logins: map[string]time.Time{}}
}

// subsonicMethod is the method of the API a path names, "" for a path of no
// method; known says the method is one this server answers.
func subsonicMethod(p string) (method string, known bool) {
	rest, ok := strings.CutPrefix(p, subsonicRoot)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	name, view := strings.CutSuffix(rest, ".view")
	_, known = subsonicMethods[strings.ToLower(name)]
	if !known && !view {
		return "", false
	}
	return name, known
}

// isSubsonicRequest reports whether a request is one of the API.
func isSubsonicRequest(r *http.Request) bool {
	method, _ := subsonicMethod(r.URL.Path)
	return method != ""
}

// warnAboutSubsonic says that the API hides a folder called rest of the
// served folder, as far as its methods go.
func (s *Server) warnAboutSubsonic(set *settings) {
	if !set.subsonic {
		return
	}
	if info, err := os.Stat(filepath.Join(s.root.Base(), "rest")); err == nil && info.IsDir() {
		s.log.Warn("the OpenSubsonic API is answered below /rest, so the files of the folder rest of "+
			"general.baseFolderFiles that are named after its methods cannot be reached over http",
			"baseFolderFiles", s.root.Base())
	}
}

// subsonicRequest is a request of an OpenSubsonic client.
type subsonicRequest struct {
	s      *Server
	set    *settings
	w      http.ResponseWriter
	r      *http.Request
	method string
	params url.Values
	// format and callback are how the client wants its answers: xml, json
	// or jsonp, by the function a JSONP answer calls.
	format   string
	callback string
	// client is the name the client gives itself.
	client string
	user   *account
	libs   []*jellyfin.Library
}

// serveSubsonic answers a request of an OpenSubsonic client.
func (s *Server) serveSubsonic(set *settings, w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, HEAD")
	h.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Range, If-None-Match, If-Modified-Since")
	h.Set("Access-Control-Expose-Headers", "Content-Range, Content-Length, Accept-Ranges, ETag")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	method, known := subsonicMethod(r.URL.Path)
	q := &subsonicRequest{s: s, set: set, w: w, r: r, method: method}
	// the arguments come in the query, or in the body of a form, as the
	// extension formPost has it
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	if err := r.ParseForm(); err != nil {
		q.params = r.URL.Query()
		q.format = q.params.Get("f")
		q.fail(subsonic.ErrGeneric, "the arguments cannot be read: "+err.Error())
		return
	}
	q.params = r.Form
	q.format, q.callback, q.client = q.params.Get("f"), q.params.Get("callback"), q.params.Get("c")
	if !known {
		q.fail(subsonic.ErrGeneric, "go-fs does not answer "+method)
		return
	}
	handler := subsonicMethods[strings.ToLower(method)]
	if handler.public {
		handler.serve(q)
		return
	}
	if !q.login() {
		return
	}
	q.libs = musicLibraries(config.RepositoryTypeSubsonic, q.user)
	handler.serve(q)
}

// login finds the account a request comes from, by the credentials it sends,
// and answers the request when they are not accepted.
func (q *subsonicRequest) login() bool {
	s, set, r := q.s, q.set, q.r
	name, password, token, salt, key := q.params.Get("u"), q.params.Get("p"), q.params.Get("t"),
		q.params.Get("s"), q.params.Get("apiKey")
	if key != "" {
		if name != "" || password != "" || token != "" || salt != "" {
			q.fail(subsonic.ErrConflicting, "an API key is sent without a name, a password or a token")
			return false
		}
		// a bearer token: checkBearer counts what it refuses against the
		// address, and logs it
		q.user = s.checkBearer(set, r, key)
		if q.user == nil {
			q.delay()
			q.fail(subsonic.ErrAPIKey, "the API key is not accepted")
			return false
		}
		q.logLogin("apikey")
		return true
	}
	if name == "" {
		q.fail(subsonic.ErrMissing, "required parameter is missing: u, or apiKey")
		return false
	}
	if password == "" && (token == "" || salt == "") {
		q.fail(subsonic.ErrMissing, "required parameter is missing: p, or t and s")
		return false
	}
	if remaining, locked := s.lockedOut(set, r); locked {
		s.log.Info("http login refused, the address is locked out", "method", "subsonic",
			"address", clientAddress(set, r), "remaining", remaining.Round(time.Second))
		q.w.Header().Set("Retry-After", strconv.Itoa(retryAfter(remaining)))
		q.fail(subsonic.ErrCredentials, lockoutMessage(remaining))
		return false
	}
	how := "password"
	if password != "" {
		if hexed, ok := strings.CutPrefix(password, "enc:"); ok {
			decoded, err := hex.DecodeString(hexed)
			if err != nil {
				decoded = nil
			}
			password = string(decoded)
		}
		if password != "" {
			q.user = matchAccount(set.accounts, name, password)
		}
	} else {
		how = "token"
		q.user = matchSaltedToken(set.accounts, name, token, salt)
	}
	if q.user == nil {
		s.log.Info("http login refused", "user", name, "method", "subsonic", "client", q.client,
			"address", clientAddress(set, r))
		s.recordFailure(set, r)
		q.delay()
		q.fail(subsonic.ErrCredentials, "Wrong username or password.")
		return false
	}
	s.logins.clear(clientAddress(set, r))
	q.logLogin(how)
	return true
}

// matchSaltedToken resolves a name and the MD5 of a password and a salt to an
// account, in time that does not depend on which name was sent, as
// matchAccount does. An account without a password is never matched.
func matchSaltedToken(accounts []*account, name, token, salt string) *account {
	token = strings.ToLower(token)
	var found *account
	for _, user := range accounts {
		sum := md5.Sum([]byte(user.password + salt))
		nameMatches := secrets.Match(name, user.name)
		tokenMatches := secrets.Match(token, hex.EncodeToString(sum[:]))
		if nameMatches && tokenMatches && user.password != "" && found == nil {
			found = user
		}
	}
	return found
}

// delay waits as long as http.loginFailureDelay says after a refused login.
func (q *subsonicRequest) delay() {
	if delay := q.set.cfg.LoginFailureDelay; delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
}

// logLogin logs that an account logged in with a client, once an hour,
// since a client logs in with every request it sends.
func (q *subsonicRequest) logLogin(how string) {
	state := q.s.subsonic
	key := q.user.name + "\x00" + q.client
	now := time.Now()
	state.mu.Lock()
	last, seen := state.logins[key]
	if !seen || now.Sub(last) >= subsonicLoginLog {
		state.logins[key] = now
	}
	for k, at := range state.logins {
		if now.Sub(at) >= subsonicLoginLog {
			delete(state.logins, k)
		}
	}
	state.mu.Unlock()
	if !seen || now.Sub(last) >= subsonicLoginLog {
		q.s.log.Info("subsonic login", "user", q.user.name, "client", q.client, "by", how,
			"address", clientAddress(q.set, q.r))
	}
}

// serverVersion is the version of go-fs, which the answers carry.
func (s *Server) serverVersion() string {
	if s.updater != nil {
		return s.updater.Current().Version
	}
	return ""
}

// ok is a response that says the call worked, for the handler to fill in.
func (q *subsonicRequest) ok() *subsonic.Response {
	return subsonic.OK(q.s.serverVersion())
}

// reply sends a response, in the format the client asked for.
func (q *subsonicRequest) reply(resp *subsonic.Response) {
	q.replyWith(http.StatusOK, resp)
}

func (q *subsonicRequest) replyWith(status int, resp *subsonic.Response) {
	body, media, err := subsonic.Marshal(q.format, q.callback, resp)
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusBadRequest)
		return
	}
	q.w.Header().Set("Content-Type", media+"; charset=utf-8")
	q.w.Header().Set("Cache-Control", "no-store")
	q.w.WriteHeader(status)
	_, _ = q.w.Write(body)
}

// fail answers that a call failed. The answer is a 200, as the other servers
// of the API send it: the clients read the error in the body.
func (q *subsonicRequest) fail(code int, message string) {
	q.replyWith(http.StatusOK, subsonic.Failed(q.s.serverVersion(), code, message))
}

// failed answers a problem a list or a search ran into.
func (q *subsonicRequest) failed(err error) {
	if p, ok := err.(*subsonic.Problem); ok {
		q.fail(p.Code, p.Message)
		return
	}
	q.fail(subsonic.ErrGeneric, err.Error())
}

// str is a parameter, "" when not sent.
func (q *subsonicRequest) str(name string) string { return q.params.Get(name) }

// number is a parameter that is a number, fallback when it is not sent; ok
// is false when it is not a number, which is answered.
func (q *subsonicRequest) number(name string, fallback int) (int, bool) {
	raw := q.params.Get(name)
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		q.fail(subsonic.ErrGeneric, "parameter "+name+" is not a number: "+raw)
		return 0, false
	}
	return n, true
}

// required is a parameter that has to be sent; ok is false when it was not,
// which is answered.
func (q *subsonicRequest) required(name string) (string, bool) {
	value := q.params.Get(name)
	if value == "" {
		q.fail(subsonic.ErrMissing, "required parameter is missing: "+name)
		return "", false
	}
	return value, true
}

// folders are the libraries a call is about: those the account sees, or the
// one of them musicFolderId names. ok is false for a folder it does not see,
// which is answered.
func (q *subsonicRequest) folders() ([]*jellyfin.Library, bool) {
	raw := q.params.Get("musicFolderId")
	if raw == "" {
		return q.libs, true
	}
	for _, l := range q.libs {
		if strconv.Itoa(subsonic.FolderID(l)) == raw {
			return []*jellyfin.Library{l}, true
		}
	}
	q.fail(subsonic.ErrNotFound, "music folder not found: "+raw)
	return nil, false
}

// item finds an item the account sees by the id a parameter names; ok is
// false when there is none, which is answered.
func (q *subsonicRequest) item(param string) (jellyfin.Item, bool) {
	id, ok := q.required(param)
	if !ok {
		return nil, false
	}
	item := jellyfin.Lookup(q.libs, id)
	if item == nil {
		q.fail(subsonic.ErrNotFound, "not found: "+id)
		return nil, false
	}
	return item, true
}

// track finds a track the account may read by its id.
func (q *subsonicRequest) track(param string) (*jellyfin.Track, bool) {
	item, ok := q.item(param)
	if !ok {
		return nil, false
	}
	t, isTrack := item.(*jellyfin.Track)
	if !isTrack || !musicReadable(q.user, t.Virtual()) {
		q.fail(subsonic.ErrNotFound, "song not found: "+item.ItemID())
		return nil, false
	}
	return t, true
}

// describer describes items with what is kept of the account.
func (q *subsonicRequest) describer() subsonic.Describer {
	return subsonic.Describer{Data: q.s.subsonic.listeners.Data(q.user.name)}
}
