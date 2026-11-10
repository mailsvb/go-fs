//go:build !light && !no_jellyfin

package httpd

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/store"
)

// While there is a jellyfin repository, the server speaks enough of the API
// of a Jellyfin server for its music clients to browse the libraries, play
// the tracks and download them, at the root of the server, beside the files.
// A Jellyfin client is told apart from everyone else by what it sends: the
// MediaBrowser scheme of its Authorization header, or its token in a header
// or the query, on every request but the first few it sends to a server it
// has not logged in to yet, which go to a handful of paths of its own. So a
// browser still finds a folder called Items, and only the folders those few
// paths go into are hidden.
//
// A client logs in with the name and password of an account that sets http,
// and is given a token, a JWT signed with the key of the session tokens for
// an audience of its own. A Jellyfin client keeps its token for good, so it
// lasts a year rather than a session; it stops being accepted when the
// account changes its password, or when the client logs out with it. What
// the account may see is looked up on every request: the tracks it may read,
// by its paths and its right to retrieve files, and the albums, artists and
// genres of those.
//
// What a client reports it played, marks a favorite and sets of its views is
// kept in the persistent data file, by account, so that every client of the
// account sees it, also after a restart; see jellyfin.Listeners.
const (
	jellyfinTokenAudience = "go-fs-jellyfin"
	jellyfinTokenLifetime = 365 * 24 * time.Hour
	// jellyfinVersion is the version of Jellyfin the server says it is, one
	// whose API the clients speak.
	jellyfinVersion = "10.10.7"
)

// jellyfinProbes are the paths a client asks before it has logged in, which
// carry nothing that says they come from a Jellyfin client.
var jellyfinProbes = []string{"/System/Info/Public", "/System/Ping", "/Users/Public",
	"/Branding/Configuration", "/Branding/Css", "/QuickConnect/Enabled"}

// jellyfinRoots are the first parts of the paths of the API, by which a
// preflight request of a client in a browser is told apart, which carries no
// credentials of its own.
var jellyfinRoots = map[string]bool{"system": true, "users": true, "items": true, "audio": true,
	"artists": true, "genres": true, "musicgenres": true, "search": true, "sessions": true,
	"userviews": true, "useritems": true, "userfavoriteitems": true, "userplayeditems": true,
	"displaypreferences": true, "branding": true, "quickconnect": true, "library": true,
	"playlists": true, "albums": true, "studios": true, "persons": true, "playingitems": true}

// jellyfinImagePath is the path of the image of an item, which a client asks
// for without a token: Jellyfin serves images to anyone, and Finamp's image
// loaders send none.
var jellyfinImagePath = regexp.MustCompile(`(?i)^/items/[0-9a-f-]{32,36}/images/[a-z]+(/\d+)?/?$`)

// isJellyfinRequest reports whether a request comes from a Jellyfin client.
func isJellyfinRequest(r *http.Request) bool {
	if scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "MediaBrowser") ||
		strings.EqualFold(scheme, "Emby") {
		return true
	}
	for _, header := range []string{"X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
		if r.Header.Get(header) != "" {
			return true
		}
	}
	query := r.URL.Query()
	if query.Get("api_key") != "" || query.Get("ApiKey") != "" {
		return true
	}
	for _, probe := range jellyfinProbes {
		if strings.EqualFold(strings.TrimSuffix(r.URL.Path, "/"), probe) {
			return true
		}
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && jellyfinImagePath.MatchString(r.URL.Path) {
		return true
	}
	if r.Method == http.MethodOptions {
		asked := strings.ToLower(r.Header.Get("Access-Control-Request-Headers"))
		if strings.Contains(asked, "x-emby-") || strings.Contains(asked, "x-mediabrowser-") {
			return true
		}
		root, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		return strings.Contains(asked, "authorization") && jellyfinRoots[strings.ToLower(root)]
	}
	return false
}

// warnAboutJellyfin says that the paths a Jellyfin client asks first hide the
// folders they go into.
func (s *Server) warnAboutJellyfin(set *settings) {
	if !set.jellyfin {
		return
	}
	for _, name := range []string{"System", "Users", "Branding", "QuickConnect"} {
		if info, err := os.Stat(filepath.Join(s.root.Base(), name)); err == nil && info.IsDir() {
			s.log.Warn("the Jellyfin API answers a few paths in the folder "+name+" of general.baseFolderFiles, "+
				"which cannot be reached over http", "baseFolderFiles", s.root.Base())
		}
	}
}

// The buckets of the persistent data file the Jellyfin API keeps its own in,
// beside those of jellyfin.Listeners.
const (
	// jellyfinPreferences are the views a client sets, by account, by the
	// id the client gives them and by client.
	jellyfinPreferences = "jellyfin.preferences"
	// jellyfinRevoked are the ids of the tokens a client logged out with,
	// each until it would have run out.
	jellyfinRevoked = "jellyfin.revoked"
)

// jellyfinState is what the Jellyfin API keeps across requests.
type jellyfinState struct {
	listeners   *jellyfin.Listeners
	preferences *store.Bucket
	revoked     *store.Bucket
	// discovery answers the clients that look for a server; see
	// jellyfin_discovery.go.
	discovery *jellyfinDiscovery
}

func newJellyfinState(st *store.Store, logger *slog.Logger) jellyfinState {
	return jellyfinState{
		listeners:   jellyfin.NewListeners(st, "jellyfin", logger),
		preferences: st.Bucket(jellyfinPreferences),
		revoked:     st.Bucket(jellyfinRevoked),
		discovery:   &jellyfinDiscovery{port: jellyfinDiscoveryPort},
	}
}

// jellyfinClient is what a client says of itself, in the Authorization
// header Jellyfin defines: MediaBrowser Client="…", Device="…",
// DeviceId="…", Version="…", Token="…".
type jellyfinClient struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
	Token    string
}

var jellyfinParam = regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"`)

func jellyfinClientOf(r *http.Request) jellyfinClient {
	var c jellyfinClient
	header := r.Header.Get("Authorization")
	if scheme, _, _ := strings.Cut(header, " "); !strings.EqualFold(scheme, "MediaBrowser") &&
		!strings.EqualFold(scheme, "Emby") {
		header = r.Header.Get("X-Emby-Authorization")
	}
	for _, m := range jellyfinParam.FindAllStringSubmatch(header, -1) {
		// clients escape the values as a query is escaped
		value, err := url.QueryUnescape(m[2])
		if err != nil {
			value = m[2]
		}
		switch strings.ToLower(m[1]) {
		case "client":
			c.Client = value
		case "device":
			c.Device = value
		case "deviceid":
			c.DeviceID = value
		case "version":
			c.Version = value
		case "token":
			c.Token = value
		}
	}
	query := r.URL.Query()
	for _, token := range []string{r.Header.Get("X-Emby-Token"), r.Header.Get("X-MediaBrowser-Token"),
		query.Get("api_key"), query.Get("ApiKey")} {
		if c.Token == "" {
			c.Token = token
		}
	}
	return c
}

// jellyfinParser reads Jellyfin tokens, with every guard the session parser
// has.
var jellyfinParser = jwt.NewParser(
	jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	jwt.WithIssuer(tokenIssuer),
	jwt.WithAudience(jellyfinTokenAudience),
	jwt.WithExpirationRequired(),
	jwt.WithIssuedAt(),
	jwt.WithLeeway(tokenLeeway),
	jwt.WithStrictDecoding(),
)

// mintJellyfin signs the token a Jellyfin client logs in with.
func (s *signer) mintJellyfin(user *account) (string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	now := time.Now()
	claims := sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   user.name,
			Audience:  jwt.ClaimStrings{jellyfinTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(jellyfinTokenLifetime)),
			ID:        base64.RawURLEncoding.EncodeToString(id),
		},
		Credentials: s.fingerprint(user),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.key)
}

// jellyfinUser resolves the token of a request to its account, nil for none
// or one that is not accepted, including one whose account has gone or has
// changed its password since, and one a client logged out with.
func (s *Server) jellyfinUser(set *settings, raw string) *account {
	claims, user := s.jellyfinClaims(set, raw)
	if user == nil {
		return nil
	}
	revoked, err := s.jellyfin.revoked.Get(claims.ID, &struct{}{})
	if err != nil {
		s.log.Warn("jellyfin cannot read the tokens logged out with from the persistent data file",
			"error", err)
		return nil
	}
	if revoked {
		return nil
	}
	return user
}

// jellyfinClaims reads a token, and resolves it to its account: nil, nil for
// a token that is not accepted.
func (s *Server) jellyfinClaims(set *settings, raw string) (*sessionClaims, *account) {
	if raw == "" {
		return nil, nil
	}
	claims := &sessionClaims{}
	_, err := jellyfinParser.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
		}
		return s.tokens.key, nil
	})
	if err != nil {
		return nil, nil
	}
	user := accountNamed(set.accounts, claims.Subject)
	if user == nil || !s.tokens.issuedFor(claims, user) {
		return nil, nil
	}
	return claims, user
}

// jellyfinID is an id of the form Jellyfin gives its ids, of something of
// this server: the server itself, or an account. It is keyed with the
// session key, so that it is the same on every start and says nothing.
func (s *Server) jellyfinID(parts ...string) string {
	mac := hmac.New(sha256.New, s.tokens.key)
	mac.Write([]byte("jellyfin\x00" + strings.Join(parts, "\x00")))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// jellyfinRequest is a request of a Jellyfin client, once it is known who
// sent it.
type jellyfinRequest struct {
	s      *Server
	set    *settings
	w      http.ResponseWriter
	r      *http.Request
	client jellyfinClient
	user   *account
	// path is the path in parts, with what Jellyfin offers both with and
	// without the account in front of it taken as without
	path   []string
	params map[string]string
	libs   []*jellyfin.Library
}

// serveJellyfin answers a request of a Jellyfin client.
func (s *Server) serveJellyfin(set *settings, w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, HEAD")
	h.Set("Access-Control-Allow-Headers", "Accept, Authorization, Cache-Control, Content-Type, Range, "+
		"If-None-Match, If-Modified-Since, X-Emby-Authorization, X-Emby-Token, X-MediaBrowser-Token")
	h.Set("Access-Control-Expose-Headers", "Content-Range, Content-Length, Accept-Ranges, ETag")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	j := &jellyfinRequest{s: s, set: set, w: w, r: r, client: jellyfinClientOf(r),
		path: jellyfinPath(r.URL.Path), params: map[string]string{}}
	for key, values := range r.URL.Query() {
		// the API reads its parameters whatever their case
		j.params[strings.ToLower(key)] = values[0]
	}

	switch {
	case j.is("system", "info", "public"):
		writeJSON(w, http.StatusOK, j.publicInfo())
		return
	case j.is("system", "ping"):
		writeJSON(w, http.StatusOK, "Jellyfin Server")
		return
	case j.is("users", "public"):
		writeJSON(w, http.StatusOK, []any{})
		return
	case j.is("branding", "configuration"):
		writeJSON(w, http.StatusOK, map[string]any{"LoginDisclaimer": "", "CustomCss": "", "SplashscreenEnabled": false})
		return
	case j.is("branding", "css"):
		w.Header().Set("Content-Type", "text/css")
		return
	case j.is("quickconnect", "enabled"):
		writeJSON(w, http.StatusOK, false)
		return
	case j.is("users", "authenticatebyname") && r.Method == http.MethodPost:
		j.login()
		return
	}

	j.user = s.jellyfinUser(set, j.client.Token)
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(j.is("items", "*", "images", "*") || j.is("items", "*", "images", "*", "*")) {
		// Jellyfin serves images to anyone, and Finamp's image loaders send
		// no token; one that comes with a token sees what its account may
		if j.user != nil {
			j.libs = j.libraries()
		} else {
			j.libs = jellyfin.Libraries(config.RepositoryTypeJellyfin)
		}
		j.image(j.path[1], j.path[3])
		return
	}
	if j.user == nil {
		if j.client.Token != "" {
			s.log.Debug("jellyfin token refused", "url", r.URL.Path, "address", clientAddress(set, r))
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	j.libs = j.libraries()
	j.serve()
}

// jellyfinPath splits a path into its parts, and takes away the account in
// front of the paths Jellyfin offers both with and without it.
func jellyfinPath(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) >= 3 && strings.EqualFold(parts[0], "users") {
		switch strings.ToLower(parts[2]) {
		case "items":
			return parts[2:]
		case "views":
			return append([]string{"UserViews"}, parts[3:]...)
		case "favoriteitems":
			return append([]string{"UserFavoriteItems"}, parts[3:]...)
		case "playeditems":
			return append([]string{"UserPlayedItems"}, parts[3:]...)
		case "playingitems":
			return parts[2:]
		}
	}
	return parts
}

// is reports whether the path is pattern, part for part whatever its case;
// "*" is any one part.
func (j *jellyfinRequest) is(pattern ...string) bool {
	if len(pattern) != len(j.path) {
		return false
	}
	for i, part := range pattern {
		if part != "*" && !strings.EqualFold(part, j.path[i]) {
			return false
		}
	}
	return true
}

// login takes the name and password of an account that sets http for a
// token.
func (j *jellyfinRequest) login() {
	s, set, w, r := j.s, j.set, j.w, j.r
	w.Header().Set("Cache-Control", "no-store")
	if remaining, locked := s.lockedOut(set, r); locked {
		s.log.Info("http login refused, the address is locked out", "method", "jellyfin",
			"address", clientAddress(set, r), "remaining", remaining.Round(time.Second))
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter(remaining)))
		http.Error(w, "too many failed logins from your address", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Username string
		Pw       string
		Password string
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "the body is not the JSON of a login", http.StatusBadRequest)
		return
	}
	if body.Pw == "" {
		body.Pw = body.Password
	}
	user := matchAccount(set.accounts, body.Username, body.Pw)
	if user == nil {
		s.log.Info("http login refused", "user", body.Username, "method", "jellyfin",
			"address", clientAddress(set, r))
		s.recordFailure(set, r)
		if delay := set.cfg.LoginFailureDelay; delay > 0 {
			time.Sleep(time.Duration(delay) * time.Second)
		}
		http.Error(w, "Invalid username or password entered.", http.StatusUnauthorized)
		return
	}
	s.logins.clear(clientAddress(set, r))
	token, err := s.tokens.mintJellyfin(user)
	if err != nil {
		s.log.Error("jellyfin cannot sign a token", "error", err)
		http.Error(w, "the token cannot be signed", http.StatusInternalServerError)
		return
	}
	j.user = user
	s.log.Info("jellyfin login", "user", user.name, "client", j.client.Client, "device", j.client.Device,
		"address", clientAddress(set, r))
	writeJSON(w, http.StatusOK, map[string]any{
		"User":        j.userInfo(),
		"SessionInfo": j.session(),
		"AccessToken": token,
		"ServerId":    s.jellyfinID("server"),
	})
}

// libraries are the libraries as much of each as the account may see.
func (j *jellyfinRequest) libraries() []*jellyfin.Library {
	return musicLibraries(config.RepositoryTypeJellyfin, j.user)
}

// readable reports whether the account may read a file.
func (j *jellyfinRequest) readable(virtual string) bool {
	return musicReadable(j.user, virtual)
}

func (j *jellyfinRequest) describer() jellyfin.Describer {
	return jellyfin.Describer{ServerID: j.s.jellyfinID("server"), Data: j.data()}
}

// data is what is kept of the account and an item.
func (j *jellyfinRequest) data() func(jellyfin.Item) jellyfin.Record {
	return j.s.jellyfin.listeners.Data(j.user.name)
}

// jellyfinServerName is the name clients show of the server: the names of
// its libraries, or the host's where there are none, and the version of go-fs.
// The version the server says it is stays one of Jellyfin, which clients
// check, so this is where go-fs says its own.
func (s *Server) jellyfinServerName(set *settings) string {
	name := set.cfg.JellyfinName
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "go-fs"
	}
	if s.updater != nil {
		if version := s.updater.Current().Version; version != "" {
			name += " · go-fs " + version
		}
	}
	return name
}

func (j *jellyfinRequest) publicInfo() map[string]any {
	scheme := "http"
	if secureRequest(j.set, j.r) {
		scheme = "https"
	}
	return map[string]any{
		"LocalAddress":           scheme + "://" + j.r.Host,
		"ServerName":             j.s.jellyfinServerName(j.set),
		"Version":                jellyfinVersion,
		"ProductName":            "Jellyfin Server",
		"OperatingSystem":        "",
		"Id":                     j.s.jellyfinID("server"),
		"StartupWizardCompleted": true,
	}
}

func (j *jellyfinRequest) info() map[string]any {
	info := j.publicInfo()
	for key, value := range map[string]any{
		"OperatingSystemDisplayName": "", "HasPendingRestart": false, "IsShuttingDown": false,
		"SupportsLibraryMonitor": false, "CanSelfRestart": false, "CanLaunchWebBrowser": false,
		"HasUpdateAvailable": false, "CompletedInstallations": []any{}, "CastReceiverApplications": []any{},
		"EncoderLocation": "NotFound", "SystemArchitecture": "",
	} {
		info[key] = value
	}
	return info
}

// userInfo is the account as Jellyfin describes a user.
func (j *jellyfinRequest) userInfo() map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"Name":                      j.user.name,
		"ServerId":                  j.s.jellyfinID("server"),
		"Id":                        j.s.jellyfinID("user", j.user.name),
		"HasPassword":               true,
		"HasConfiguredPassword":     true,
		"HasConfiguredEasyPassword": false,
		"EnableAutoLogin":           false,
		"LastLoginDate":             now,
		"LastActivityDate":          now,
		"Configuration": map[string]any{
			"PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "", "DisplayMissingEpisodes": false,
			"GroupedFolders": []string{}, "SubtitleMode": "Default", "DisplayCollectionsView": false,
			"EnableLocalPassword": false, "OrderedViews": []string{}, "LatestItemsExcludes": []string{},
			"MyMediaExcludes": []string{}, "HidePlayedInLatest": true, "RememberAudioSelections": true,
			"RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": false,
		},
		"Policy": map[string]any{
			"IsAdministrator": false, "IsHidden": true, "IsDisabled": false, "EnableUserPreferenceAccess": true,
			"EnableRemoteControlOfOtherUsers": false, "EnableSharedDeviceControl": false,
			"EnableRemoteAccess": true, "EnableLiveTvManagement": false, "EnableLiveTvAccess": false,
			"EnableMediaPlayback": true, "EnableAudioPlaybackTranscoding": false,
			"EnableVideoPlaybackTranscoding": false, "EnablePlaybackRemuxing": false,
			"EnableContentDeletion": false, "EnableContentDownloading": true, "EnableSyncTranscoding": false,
			"EnableMediaConversion": false, "EnableAllDevices": true, "EnableAllChannels": false,
			"EnableAllFolders": true, "EnabledFolders": []string{}, "EnablePublicSharing": false,
			"InvalidLoginAttemptCount": 0, "RemoteClientBitrateLimit": 0,
			"AuthenticationProviderId":         "go-fs",
			"PasswordResetProviderId":          "go-fs",
			"SyncPlayAccess":                   "None",
			"BlockedTags":                      []string{},
			"AllowedTags":                      []string{},
			"EnableContentDeletionFromFolders": []string{},
			"BlockUnratedItems":                []string{},
			"AccessSchedules":                  []any{},
		},
	}
}

// session is the session of the client, which is not kept: there is one per
// request, made of what the client says of itself.
func (j *jellyfinRequest) session() map[string]any {
	return map[string]any{
		"Id":                    j.s.jellyfinID("session", j.user.name, j.client.DeviceID),
		"UserId":                j.s.jellyfinID("user", j.user.name),
		"UserName":              j.user.name,
		"Client":                j.client.Client,
		"DeviceName":            j.client.Device,
		"DeviceId":              j.client.DeviceID,
		"ApplicationVersion":    j.client.Version,
		"ServerId":              j.s.jellyfinID("server"),
		"IsActive":              true,
		"LastActivityDate":      time.Now().UTC().Format(time.RFC3339),
		"PlayableMediaTypes":    []string{"Audio"},
		"SupportedCommands":     []string{},
		"SupportsMediaControl":  false,
		"SupportsRemoteControl": false,
		"HasCustomDeviceName":   false,
		"PlayState": map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false,
			"RepeatMode": "RepeatNone", "PlaybackOrder": "Default"},
		"Capabilities": map[string]any{"PlayableMediaTypes": []string{"Audio"}, "SupportedCommands": []string{},
			"SupportsMediaControl": false},
	}
}
