//go:build !light && !no_jellyfin

package httpd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/store"
)

// newJellyfinServer serves a library in /music of two albums: one of a band,
// with a cover beside it, and one of someone else, whose track carries its
// picture, its ReplayGain and its composer. john reads everything; anna reads
// the band only.
func newJellyfinServer(t *testing.T, tune func(*httpConfig)) (*testServer, *jellyfin.Library) {
	t.Helper()
	anna := readOnlyUser("anna", "secret")
	anna.Paths = []string{"^/music/Band/.*"}
	server := newServer(t, func(cfg *httpConfig) {
		publicServer(cfg)
		cfg.JellyfinFolders = []string{"/music"}
		cfg.Users = append(cfg.Users, anna)
		if tune != nil {
			tune(cfg)
		}
	})
	files := map[string][]byte{
		"Band/First/01 One.mp3": audiotest.MP3(audiotest.Tags{Title: "One", Artist: "The Band",
			AlbumArtist: "The Band", Album: "First", Track: 1, Year: 2001, Genre: "Rock"}, 200, true),
		"Band/First/02 Two.mp3": audiotest.MP3(audiotest.Tags{Title: "Two", Artist: "The Band",
			AlbumArtist: "The Band", Album: "First", Track: 2, Year: 2001, Genre: "Rock"}, 100, true),
		"Band/First/cover.png": audiotest.PNG,
		"Other/Solo/song.flac": audiotest.FLAC(audiotest.Tags{Title: "Solo song", Artist: "Someone",
			Album: "Solo", Genre: "Jazz", Composer: "Writer", Picture: audiotest.PNG,
			Extra: map[string]string{"REPLAYGAIN_TRACK_GAIN": "-6.50 dB", "REPLAYGAIN_ALBUM_GAIN": "-5.00 dB"}}, 30),
	}
	var sources []jellyfin.Source
	var hrefs []string
	for href, content := range files {
		path := server.write(t, "music/"+href, string(content))
		if !strings.HasSuffix(href, ".png") {
			track, err := audio.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			sources = append(sources, jellyfin.Source{Href: href, Size: int64(len(content)),
				ModTime: time.Now(), Track: track})
			hrefs = append(hrefs, href)
		}
	}
	onDisk := filepath.Join(server.base, "music")
	library := jellyfin.Build("music", "Our music", "/music", onDisk, sources,
		jellyfin.Covers(onDisk, jellyfin.Folders(hrefs)), time.Now())
	jellyfin.Publish(library)
	t.Cleanup(func() { jellyfin.Retire("music") })
	return server, library
}

const jellyfinHeader = `MediaBrowser Client="Test", Device="Desk", DeviceId="d1", Version="1.0"`

// jf sends a request as a Jellyfin client does, with its token when it has
// one.
func jf(t *testing.T, server *testServer, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.url(path), body)
	if err != nil {
		t.Fatal(err)
	}
	header := jellyfinHeader
	if token != "" {
		header += `, Token="` + token + `"`
	}
	req.Header.Set("Authorization", header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(t, req)
}

func jfLogin(t *testing.T, server *testServer, name, password string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"Username": name, "Pw": password})
	return jf(t, server, http.MethodPost, "/Users/AuthenticateByName", "", bytes.NewReader(body))
}

func jfToken(t *testing.T, server *testServer, name, password string) string {
	t.Helper()
	res := jfLogin(t, server, name, password)
	var answer struct {
		AccessToken string
		ServerId    string
		User        struct{ Name, Id string }
	}
	decode(t, res, &answer)
	if answer.AccessToken == "" || answer.User.Name != name || answer.ServerId == "" || answer.User.Id == "" {
		t.Fatalf("login answered %+v", answer)
	}
	return answer.AccessToken
}

func decode(t *testing.T, res *http.Response, v any) {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("%s: status %d: %s", res.Request.URL.Path, res.StatusCode, body)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

type jfItems struct {
	Items []struct {
		Name, Id, Type, AlbumId string
		ImageTags               map[string]string
		MediaSources            []jellyfin.MediaSource
	}
	TotalRecordCount int
}

func (items jfItems) names() []string {
	var out []string
	for _, item := range items.Items {
		out = append(out, item.Name)
	}
	return out
}

func jfList(t *testing.T, server *testServer, path, token string) jfItems {
	t.Helper()
	var items jfItems
	decode(t, jf(t, server, http.MethodGet, path, token, nil), &items)
	return items
}

func TestJellyfinIsToldApart(t *testing.T) {
	server, _ := newJellyfinServer(t, nil)
	server.write(t, "Items/readme.txt", "a folder of files")

	// a browser finds the folder, a Jellyfin client the API
	if res, body := get(t, server, "/Items/readme.txt"); res.StatusCode != http.StatusOK || body != "a folder of files" {
		t.Errorf("a browser got %d %q", res.StatusCode, body)
	}
	if res := jf(t, server, http.MethodGet, "/Items", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a client without a token got %d", res.StatusCode)
	}

	var info struct{ ProductName, Version, Id, ServerName string }
	res, _ := get(t, server, "/System/Info/Public")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("the public info answered %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = get(t, server, "/system/info/public")
	if res.StatusCode != http.StatusOK {
		t.Errorf("the API is not answered whatever its case: %d", res.StatusCode)
	}
	decode(t, jf(t, server, http.MethodGet, "/System/Info/Public", "", nil), &info)
	if info.ProductName != "Jellyfin Server" || info.Version == "" || len(info.Id) != 32 {
		t.Errorf("public info %+v", info)
	}

	req, _ := http.NewRequest(http.MethodOptions, server.url("/Users/AuthenticateByName"), nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Headers", "content-type,x-emby-authorization")
	if res := do(t, req); res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("a preflight got %d %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
}

// TestJellyfinServerName checks the name clients show of the server is that
// of its libraries and the version of go-fs, while the version it says it is
// stays one of Jellyfin, which clients check.
func TestJellyfinServerName(t *testing.T) {
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) { cfg.JellyfinName = "Our music" })
	var info struct{ Version, ServerName string }
	decode(t, jf(t, server, http.MethodGet, "/System/Info/Public", "", nil), &info)
	if info.ServerName != "Our music" || info.Version != jellyfinVersion {
		t.Errorf("without an updater the server is %+v", info)
	}
	server.SetUpdater(newStubUpdater())
	decode(t, jf(t, server, http.MethodGet, "/System/Info/Public", "", nil), &info)
	if info.ServerName != "Our music · go-fs 1.0.0" || info.Version != jellyfinVersion {
		t.Errorf("with an updater the server is %+v", info)
	}
}

// reloadJellyfin reloads the server with its settings as tune changes them.
func reloadJellyfin(t *testing.T, server *testServer, tune func(*config.HTTP)) {
	t.Helper()
	set := server.settings()
	cfg := set.cfg
	tune(&cfg)
	if err := server.Reload(cfg, set.https, set.ssh, []config.User{fullUser("john", "doe")}, nil, nil); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// askForJellyfin broadcasts, as a client does, to the server's discovery
// socket, and returns the answer, nil for none.
func askForJellyfin(t *testing.T, server *testServer, question string) map[string]any {
	t.Helper()
	conn, err := net.Dial("udp", server.discoveryAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(question)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return nil
	}
	var answer map[string]any
	if err := json.Unmarshal(buf[:n], &answer); err != nil {
		t.Fatalf("the answer %q is no JSON: %v", buf[:n], err)
	}
	return answer
}

func TestJellyfinDiscovery(t *testing.T) {
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) { cfg.JellyfinName = "Our music" })
	server.SetUpdater(newStubUpdater())
	if server.discoveryAddr() != nil {
		t.Fatal("the discovery socket is open while jellyfinDiscovery is off")
	}
	// any port, since a Jellyfin server on the host may hold 7359
	server.jellyfin.discovery.mu.Lock()
	server.jellyfin.discovery.port = 0
	server.jellyfin.discovery.mu.Unlock()
	reloadJellyfin(t, server, func(cfg *config.HTTP) { cfg.JellyfinDiscovery = true })
	if server.discoveryAddr() == nil {
		t.Fatal("the discovery socket is not open after jellyfinDiscovery was switched on")
	}

	var info struct{ Id, ServerName string }
	decode(t, jf(t, server, http.MethodGet, "/System/Info/Public", "", nil), &info)
	answer := askForJellyfin(t, server, "Who is JellyfinServer?")
	want := map[string]any{"Address": "http://" + server.Addr().String(), "Id": info.Id,
		"Name": info.ServerName, "EndpointAddress": nil}
	if !reflect.DeepEqual(answer, want) {
		t.Errorf("the answer is %v, want %v", answer, want)
	}
	if answer := askForJellyfin(t, server, "who is EmbyServer?"); answer != nil {
		t.Errorf("another question was answered with %v", answer)
	}

	// the url of a library is the address it is reached at
	reloadJellyfin(t, server, func(cfg *config.HTTP) { cfg.JellyfinURL = "https://music.example.com" })
	if answer := askForJellyfin(t, server, "who is JellyfinServer?"); answer["Address"] != "https://music.example.com" {
		t.Errorf("with a url the answer is %v", answer)
	}

	reloadJellyfin(t, server, func(cfg *config.HTTP) { cfg.JellyfinDiscovery = false })
	if server.discoveryAddr() != nil {
		t.Error("the discovery socket stays open after jellyfinDiscovery was switched off")
	}
}

// TestJellyfinDiscoveryOnEveryInterface checks a server bound to every
// interface answers with the address of the one the client is reached
// through.
func TestJellyfinDiscoveryOnEveryInterface(t *testing.T) {
	server, _ := newJellyfinServer(t, nil)
	set := *server.settings()
	set.cfg.Address = ""
	client := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	if got, want := server.discoveryAddress(&set, client), "http://"+server.Addr().String(); got != want {
		t.Errorf("the address is %q, want %q", got, want)
	}
}

func TestJellyfinIsOffWithoutALibrary(t *testing.T) {
	server := newServer(t, publicServer)
	server.write(t, "System/Info/Public", "a file")
	if res, body := get(t, server, "/System/Info/Public"); res.StatusCode != http.StatusOK || body != "a file" {
		t.Errorf("without a jellyfin repository the path got %d %q", res.StatusCode, body)
	}
}

func TestJellyfinTheFolderNeedsAnAccount(t *testing.T) {
	server, _ := newJellyfinServer(t, nil)
	server.write(t, "public.txt", "public")
	if res, _ := get(t, server, "/music/Band/First/01%20One.mp3"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a track was read without an account: %d", res.StatusCode)
	}
	if res, _ := get(t, server, "/music"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the folder was listed without an account: %d", res.StatusCode)
	}
	if res, _ := get(t, server, "/musical/x"); res.StatusCode == http.StatusUnauthorized {
		t.Error("a folder whose name begins like the library's is protected")
	}
	if res := basic(t, server, http.MethodGet, "/music/Band/First/01%20One.mp3", "john", "doe", nil); res.StatusCode != http.StatusOK {
		t.Errorf("an account got %d", res.StatusCode)
	}
	if res, _ := get(t, server, "/public.txt"); res.StatusCode != http.StatusOK {
		t.Errorf("a public file got %d", res.StatusCode)
	}
}

func TestJellyfinLogin(t *testing.T) {
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) {
		cfg.LoginAttempts = 2
		cfg.LoginLockout = 60
	})
	token := jfToken(t, server, "john", "doe")
	var me struct{ Name string }
	decode(t, jf(t, server, http.MethodGet, "/Users/Me", token, nil), &me)
	if me.Name != "john" {
		t.Errorf("me is %q", me.Name)
	}
	// the token is also taken from the query, as a player is given a URL
	req, _ := http.NewRequest(http.MethodGet, server.url("/System/Info?api_key="+token), nil)
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Errorf("a token in the query got %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, server.url("/System/Info"), nil)
	req.Header.Set("X-Emby-Token", token)
	if res := do(t, req); res.StatusCode != http.StatusOK {
		t.Errorf("a token in X-Emby-Token got %d", res.StatusCode)
	}

	// a token of another audience is no Jellyfin token
	john := accountNamed(server.settings().accounts, "john")
	registry, _, err := server.tokens.mintRegistry(john, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res := jf(t, server, http.MethodGet, "/System/Info", registry, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a registry token got %d", res.StatusCode)
	}

	if res := jfLogin(t, server, "john", "wrong"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password got %d", res.StatusCode)
	}
	if record := server.logs.find("http login refused"); record == nil || record["method"] != "jellyfin" {
		t.Errorf("the refusal is recorded as %v", record)
	}
	jfLogin(t, server, "john", "wrong")
	if res := jfLogin(t, server, "john", "doe"); res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a locked address got %d", res.StatusCode)
	}

	// a new password signs the clients out
	set := server.settings()
	user := fullUser("john", "changed")
	if err := server.Reload(set.cfg, set.https, set.ssh, []config.User{user}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if res := jf(t, server, http.MethodGet, "/System/Info", token, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token of the old password got %d", res.StatusCode)
	}
}

// TestJellyfinLoginAnswersWhatClientsRequire checks the answer to a login has
// every flag a client such as Finamp reads as a bool that must be there: one
// that is missing fails its login.
func TestJellyfinLoginAnswersWhatClientsRequire(t *testing.T) {
	server, _ := newJellyfinServer(t, nil)
	var answer map[string]any
	decode(t, jfLogin(t, server, "john", "doe"), &answer)
	required := map[string][]string{
		"User": {"HasPassword", "HasConfiguredPassword", "HasConfiguredEasyPassword"},
		"User.Configuration": {"PlayDefaultAudioTrack", "DisplayMissingEpisodes", "DisplayCollectionsView",
			"EnableLocalPassword", "HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections",
			"EnableNextEpisodeAutoPlay"},
		"User.Policy": {"IsAdministrator", "IsHidden", "IsDisabled", "EnableUserPreferenceAccess",
			"EnableRemoteControlOfOtherUsers", "EnableSharedDeviceControl", "EnableRemoteAccess",
			"EnableLiveTvManagement", "EnableLiveTvAccess", "EnableMediaPlayback", "EnableAudioPlaybackTranscoding",
			"EnableVideoPlaybackTranscoding", "EnablePlaybackRemuxing", "EnableContentDeletion",
			"EnableContentDownloading", "EnableSyncTranscoding", "EnableMediaConversion", "EnableAllDevices",
			"EnableAllChannels", "EnableAllFolders", "EnablePublicSharing"},
		"SessionInfo":           {"SupportsRemoteControl", "IsActive", "SupportsMediaControl", "HasCustomDeviceName"},
		"SessionInfo.PlayState": {"CanSeek", "IsPaused", "IsMuted"},
	}
	for path, flags := range required {
		object := answer
		for _, key := range strings.Split(path, ".") {
			object, _ = object[key].(map[string]any)
		}
		if object == nil {
			t.Errorf("the login answers no %s", path)
			continue
		}
		for _, flag := range flags {
			if _, ok := object[flag].(bool); !ok {
				t.Errorf("%s.%s is %v, not a bool", path, flag, object[flag])
			}
		}
	}
}

// Finamp reads these fields of a track without allowing for them to be
// missing, and shows no track of an album when one is.
func TestJellyfinTracksAnswerWhatClientsRequire(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")
	album, track := library.Albums[0], library.Albums[0].Tracks[0]
	source := []string{"Protocol", "Type", "IsRemote", "SupportsTranscoding", "SupportsDirectStream",
		"SupportsDirectPlay", "IsInfiniteStream", "RequiresOpening", "RequiresClosing", "RequiresLooping",
		"SupportsProbing", "ReadAtNativeFramerate", "IgnoreDts", "IgnoreIndex", "GenPtsInput", "MediaStreams"}
	stream := []string{"IsInterlaced", "IsDefault", "IsForced", "Type", "Index", "IsExternal",
		"IsTextSubtitleStream", "SupportsExternalStream"}
	has := func(what string, object map[string]any, keys ...string) {
		t.Helper()
		for _, key := range keys {
			if object[key] == nil {
				t.Errorf("%s has no %s", what, key)
			}
		}
	}
	check := func(what string, sources []any) {
		t.Helper()
		if len(sources) != 1 {
			t.Fatalf("%s has %d media sources", what, len(sources))
		}
		s, _ := sources[0].(map[string]any)
		has(what+" media source", s, source...)
		streams, _ := s["MediaStreams"].([]any)
		if len(streams) != 1 {
			t.Fatalf("%s has %d media streams", what, len(streams))
		}
		ms, _ := streams[0].(map[string]any)
		has(what+" media stream", ms, stream...)
	}

	var page map[string]any
	decode(t, jf(t, server, http.MethodGet, "/Users/x/Items?ParentId="+album.ID+"&IncludeItemTypes=Audio&Recursive=true",
		token, nil), &page)
	has("the page", page, "TotalRecordCount", "StartIndex")
	items, _ := page["Items"].([]any)
	if len(items) == 0 {
		t.Fatal("the album has no tracks")
	}
	item, _ := items[0].(map[string]any)
	has("the track", item, "Id")
	data, _ := item["UserData"].(map[string]any)
	has("the user data", data, "PlaybackPositionTicks", "PlayCount", "IsFavorite", "Played")
	sources, _ := item["MediaSources"].([]any)
	check("the track", sources)

	var info map[string]any
	decode(t, jf(t, server, http.MethodPost, "/Items/"+track.ID+"/PlaybackInfo", token, strings.NewReader("{}")), &info)
	sources, _ = info["MediaSources"].([]any)
	check("the playback info", sources)
}

func TestJellyfinBrowses(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")

	views := jfList(t, server, "/UserViews", token)
	if len(views.Items) != 1 || views.Items[0].Name != "Our music" || views.Items[0].Id != library.ID {
		t.Fatalf("views %+v", views)
	}
	albums := jfList(t, server, "/Users/x/Items?ParentId="+library.ID+"&IncludeItemTypes=MusicAlbum&Recursive=true&SortBy=SortName", token)
	if got := albums.names(); !slices.Equal(got, []string{"First", "Solo"}) || albums.TotalRecordCount != 2 {
		t.Errorf("albums %q of %d", got, albums.TotalRecordCount)
	}
	first := albums.Items[0]
	if first.ImageTags["Primary"] == "" {
		t.Error("the album has no cover")
	}
	tracks := jfList(t, server, "/Items?ParentId="+first.Id, token)
	if got := tracks.names(); !slices.Equal(got, []string{"One", "Two"}) {
		t.Errorf("tracks %q", got)
	}
	if ms := tracks.Items[0].MediaSources; len(ms) != 1 || ms[0].Container != "mp3" || !ms[0].SupportsDirectPlay {
		t.Errorf("media sources %+v", ms)
	}
	page := jfList(t, server, "/Items?IncludeItemTypes=Audio&Recursive=true&StartIndex=1&Limit=1&SortBy=Album,SortName", token)
	if got := page.names(); !slices.Equal(got, []string{"Two"}) || page.TotalRecordCount != 3 {
		t.Errorf("page %q of %d", got, page.TotalRecordCount)
	}
	artists := jfList(t, server, "/Artists/AlbumArtists?ParentId="+library.ID, token)
	if got := artists.names(); !slices.Equal(got, []string{"The Band", "Someone"}) {
		t.Errorf("album artists %q", got)
	}
	genres := jfList(t, server, "/MusicGenres", token)
	if got := genres.names(); !slices.Equal(got, []string{"Jazz", "Rock"}) {
		t.Errorf("genres %q", got)
	}
	var latest []struct{ Name string }
	decode(t, jf(t, server, http.MethodGet, "/Items/Latest?Limit=1", token, nil), &latest)
	if len(latest) != 1 {
		t.Errorf("latest %+v", latest)
	}
	var hints struct {
		SearchHints []struct{ Name, Type string }
	}
	decode(t, jf(t, server, http.MethodGet, "/Search/Hints?SearchTerm=solo", token, nil), &hints)
	if len(hints.SearchHints) != 2 {
		t.Errorf("hints %+v", hints)
	}
	var item struct{ Name, Type string }
	// a client may send an id as a GUID
	id := first.Id
	guid := strings.ToUpper(id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:])
	decode(t, jf(t, server, http.MethodGet, "/Users/x/Items/"+guid, token, nil), &item)
	if item.Name != "First" || item.Type != "MusicAlbum" {
		t.Errorf("item %+v", item)
	}
	if res := jf(t, server, http.MethodGet, "/Items/0123456789abcdef0123456789abcdef", token, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown item got %d", res.StatusCode)
	}

	for _, id := range []string{first.Id, albums.Items[1].Id} {
		res := jf(t, server, http.MethodGet, "/Items/"+id+"/Images/Primary?maxWidth=300", token, nil)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || !bytes.Equal(body, audiotest.PNG) || res.Header.Get("Content-Type") != "image/png" {
			t.Errorf("the cover of %s: %d %s, %d bytes", id, res.StatusCode, res.Header.Get("Content-Type"), len(body))
		}
	}
	if res := jf(t, server, http.MethodGet, "/Items/"+first.Id+"/Images/Backdrop", token, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a backdrop got %d", res.StatusCode)
	}

	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Capabilities/Full"} {
		if res := jf(t, server, http.MethodPost, path, token, strings.NewReader("{}")); res.StatusCode != http.StatusNoContent {
			t.Errorf("%s got %d", path, res.StatusCode)
		}
	}
	var data jellyfin.UserData
	decode(t, jf(t, server, http.MethodPost, "/Users/x/FavoriteItems/"+first.Id, token, nil), &data)
	if !data.IsFavorite || data.ItemID != first.Id {
		t.Errorf("favorite %+v", data)
	}
	if res := jf(t, server, http.MethodGet, "/Nothing/Here", token, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown endpoint got %d", res.StatusCode)
	}
}

func TestJellyfinShowsWhatTheAccountMayRead(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "anna", "secret")
	albums := jfList(t, server, "/Items?IncludeItemTypes=MusicAlbum&Recursive=true", token)
	if got := albums.names(); !slices.Equal(got, []string{"First"}) {
		t.Errorf("anna sees %q", got)
	}
	solo := library.Albums[1]
	if solo.Name != "Solo" {
		t.Fatalf("album %q", solo.Name)
	}
	for _, path := range []string{"/Items/" + solo.ID, "/Audio/" + solo.Tracks[0].ID + "/stream",
		"/Items/" + solo.ID + "/Images/Primary"} {
		if res := jf(t, server, http.MethodGet, path, token, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("anna got %d for %s", res.StatusCode, path)
		}
	}
}

func TestJellyfinStreams(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")
	track := library.Albums[0].Tracks[0]
	content, err := os.ReadFile(track.OnDisk())
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, server.url("/Audio/"+track.ID+"/stream.mp3?static=true&api_key="+token), nil)
	req.Header.Set("Range", "bytes=100-199")
	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[100:200]) ||
		res.Header.Get("Content-Type") != "audio/mpeg" || res.Header.Get("Content-Disposition") != "" {
		t.Errorf("a range of the stream: %d %s, %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(body))
	}

	res = jf(t, server, http.MethodGet, "/Items/"+track.ID+"/Download", token, nil)
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, content) ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "01 One.mp3") {
		t.Errorf("the download: %d %q, %d bytes", res.StatusCode, res.Header.Get("Content-Disposition"), len(body))
	}
	record := server.logs.find("http download")
	if record == nil || record["via"] != "jellyfin" || record["user"] != "john" || record["file"] != "/music/Band/First/01 One.mp3" {
		t.Errorf("the download is recorded as %v", record)
	}

	var info struct {
		MediaSources  []jellyfin.MediaSource
		PlaySessionId string
	}
	decode(t, jf(t, server, http.MethodPost, "/Items/"+track.ID+"/PlaybackInfo", token, strings.NewReader("{}")), &info)
	if len(info.MediaSources) != 1 || info.MediaSources[0].SupportsTranscoding || info.MediaSources[0].Size != int64(len(content)) ||
		info.MediaSources[0].RunTimeTicks == 0 || info.PlaySessionId == "" {
		t.Errorf("playback info %+v", info)
	}
	album := library.Albums[0]
	if res := jf(t, server, http.MethodGet, "/Audio/"+album.ID+"/stream", token, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an album was streamed: %d", res.StatusCode)
	}
}

// jfTrack finds a track of the library by its name.
func jfTrack(t *testing.T, library *jellyfin.Library, name string) *jellyfin.Track {
	t.Helper()
	for _, track := range library.Tracks {
		if track.Name == name {
			return track
		}
	}
	t.Fatalf("no track %q", name)
	return nil
}

func TestJellyfinStreamsMP3AsHLS(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")
	track := library.Albums[0].Tracks[0]
	content, err := os.ReadFile(track.OnDisk())
	if err != nil {
		t.Fatal(err)
	}

	// as Finamp asks when it is told to transcode: the token in the query
	playlist := server.url("/Audio/" + track.ID + "/main.m3u8?ApiKey=" + token + "&audioCodec=aac&segmentContainer=ts")
	req, _ := http.NewRequest(http.MethodGet, playlist, nil)
	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		!strings.HasPrefix(string(body), "#EXTM3U\n") || !strings.Contains(string(body), "#EXT-X-ENDLIST") {
		t.Fatalf("the playlist: %d %s\n%s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	var segment string
	for _, line := range strings.Split(string(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			segment = line
		}
	}
	base, _ := url.Parse(playlist)
	ref, err := url.Parse(segment)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, base.ResolveReference(ref).String(), nil)
	res = do(t, req)
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Errorf("the segment %q: %d, %d bytes", segment, res.StatusCode, len(body))
	}

	flac := jfTrack(t, library, "Solo song")
	if res := jf(t, server, http.MethodGet, "/Audio/"+flac.ID+"/main.m3u8", token, nil); res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a FLAC as HLS: %d", res.StatusCode)
	}
	if record := server.logs.find("jellyfin cannot stream as HLS without transcoding, turn transcoding off in the client"); record == nil ||
		record["container"] != "flac" {
		t.Errorf("the refusal is logged as %v", record)
	}
}

func TestJellyfinDescribesGainsAndFormat(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")
	flac, mp3 := jfTrack(t, library, "Solo song"), library.Albums[0].Tracks[0]

	var item struct {
		NormalizationGain, AlbumNormalizationGain *float64
		People                                    []jellyfin.Person
		MediaSources                              []jellyfin.MediaSource
	}
	decode(t, jf(t, server, http.MethodGet, "/Users/x/Items/"+flac.ID, token, nil), &item)
	if item.NormalizationGain == nil || *item.NormalizationGain != -6.5 ||
		item.AlbumNormalizationGain == nil || *item.AlbumNormalizationGain != -5 {
		t.Errorf("the gains %v %v", item.NormalizationGain, item.AlbumNormalizationGain)
	}
	if len(item.People) != 1 || item.People[0].Name != "Writer" || item.People[0].Type != "Composer" {
		t.Errorf("people %+v", item.People)
	}
	if len(item.MediaSources) != 1 || item.MediaSources[0].MediaStreams[0].BitDepth != 16 {
		t.Errorf("media sources %+v", item.MediaSources)
	}
	var info struct{ MediaSources []jellyfin.MediaSource }
	decode(t, jf(t, server, http.MethodGet, "/Items/"+flac.ID+"/PlaybackInfo", token, nil), &info)
	if len(info.MediaSources) != 1 || info.MediaSources[0].MediaStreams[0].BitDepth != 16 {
		t.Errorf("playback info %+v", info.MediaSources)
	}

	var album struct{ NormalizationGain *float64 }
	decode(t, jf(t, server, http.MethodGet, "/Users/x/Items/"+flac.Album.ID, token, nil), &album)
	if album.NormalizationGain == nil || *album.NormalizationGain != -5 {
		t.Errorf("the album gain %v", album.NormalizationGain)
	}

	var plain map[string]any
	decode(t, jf(t, server, http.MethodGet, "/Users/x/Items/"+mp3.ID, token, nil), &plain)
	if _, ok := plain["NormalizationGain"]; ok {
		t.Errorf("an MP3 without ReplayGain has a gain: %v", plain["NormalizationGain"])
	}
}

func TestJellyfinMixesTracksOnly(t *testing.T) {
	server, library := newJellyfinServer(t, nil)
	token := jfToken(t, server, "john", "doe")
	band := library.Albums[0].Artist
	// as Finamp asks for the tracks of an artist's mix
	mix := jfList(t, server, "/Users/x/Items?Filters=IsNotFolder&Recursive=true&SortBy=Random&ArtistIds="+band.ID, token)
	if names := mix.names(); len(names) != 2 || !slices.Contains(names, "One") || !slices.Contains(names, "Two") {
		t.Errorf("the mix %q", names)
	}
	for _, item := range mix.Items {
		if item.Type != "Audio" {
			t.Errorf("the mix has a %s", item.Type)
		}
	}
}

// jfSend sends a request of a Jellyfin client and answers its status.
func jfSend(t *testing.T, server *testServer, method, path, token, body string) int {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	res := jf(t, server, method, path, token, reader)
	_ = res.Body.Close()
	return res.StatusCode
}

func TestJellyfinKeepsWhatWasPlayed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) { cfg.Store = st })
	token := jfToken(t, server, "john", "doe")
	tracks := "/Users/x/Items?IncludeItemTypes=Audio&Recursive=true&SortBy=SortName"
	one := jfList(t, server, tracks, token).Items[0]
	album := one.AlbumId
	var item struct {
		RunTimeTicks int64
		UserData     jellyfin.UserData
	}
	decode(t, jf(t, server, http.MethodGet, "/Items/"+one.Id, token, nil), &item)
	if item.RunTimeTicks == 0 || item.UserData.Played {
		t.Fatalf("the track before it is played: %+v", item)
	}

	// played to the end, reported as Jellyfin reports it
	for _, report := range []struct{ path, body string }{
		{"/Sessions/Playing", `{"ItemId":"` + one.Id + `","PositionTicks":0}`},
		{"/Sessions/Playing/Progress", fmt.Sprintf(`{"ItemId":"%s","PositionTicks":%d}`, one.Id, item.RunTimeTicks/2)},
		{"/Sessions/Playing/Stopped", fmt.Sprintf(`{"ItemId":"%s","PositionTicks":%d}`, one.Id, item.RunTimeTicks)},
	} {
		if status := jfSend(t, server, http.MethodPost, report.path, token, report.body); status != http.StatusNoContent {
			t.Fatalf("%s got %d", report.path, status)
		}
	}
	decode(t, jf(t, server, http.MethodGet, "/Items/"+one.Id, token, nil), &item)
	if !item.UserData.Played || item.UserData.PlayCount != 1 || item.UserData.LastPlayedDate == "" {
		t.Errorf("the track after it was played: %+v", item.UserData)
	}
	if got := jfList(t, server, tracks+"&Filters=IsPlayed", token).names(); !slices.Equal(got, []string{"One"}) {
		t.Errorf("played %q", got)
	}
	// the older reports, in the query
	if status := jfSend(t, server, http.MethodPost, "/Users/x/PlayingItems/"+one.Id, token, ""); status != http.StatusNoContent {
		t.Errorf("a legacy start got %d", status)
	}
	if status := jfSend(t, server, http.MethodDelete, fmt.Sprintf("/PlayingItems/%s?positionTicks=%d", one.Id,
		item.RunTimeTicks), token, ""); status != http.StatusNoContent {
		t.Errorf("a legacy stop got %d", status)
	}
	decode(t, jf(t, server, http.MethodGet, "/UserItems/"+one.Id+"/UserData", token, nil), &item.UserData)
	if item.UserData.PlayCount != 2 {
		t.Errorf("after a legacy report: %+v", item.UserData)
	}
	var played int
	if err := server.jellyfin.listeners.History("john", time.Time{}, func(jellyfin.Play) error {
		played++
		return nil
	}); err != nil || played != 2 {
		t.Errorf("history of %d plays, %v", played, err)
	}

	// a favorite album, and taken back
	var data jellyfin.UserData
	decode(t, jf(t, server, http.MethodPost, "/Users/x/FavoriteItems/"+album, token, nil), &data)
	if !data.IsFavorite || data.ItemID != album {
		t.Errorf("favorite %+v", data)
	}
	albums := "/Items?IncludeItemTypes=MusicAlbum&Recursive=true&"
	if got := jfList(t, server, albums+"IsFavorite=true", token).names(); !slices.Equal(got, []string{"First"}) {
		t.Errorf("favorite albums %q", got)
	}
	if got := jfList(t, server, albums+"IsFavorite=false", token).names(); !slices.Equal(got, []string{"Solo"}) {
		t.Errorf("other albums %q", got)
	}
	decode(t, jf(t, server, http.MethodDelete, "/UserFavoriteItems/"+album, token, nil), &data)
	if got := jfList(t, server, albums+"Filters=IsFavorite", token).names(); len(got) != 0 || data.IsFavorite {
		t.Errorf("favorite albums after one was taken back %q", got)
	}

	// an album marked played on a date marks its tracks
	decode(t, jf(t, server, http.MethodPost, "/Users/x/PlayedItems/"+album+"?datePlayed=2026-01-02T03:04:05.000Z",
		token, nil), &data)
	if !data.Played || data.PlayCount != 4 || data.UnplayedItemCount == nil || *data.UnplayedItemCount != 0 ||
		data.LastPlayedDate == "" {
		t.Errorf("an album marked played %+v", data)
	}
	decode(t, jf(t, server, http.MethodDelete, "/UserPlayedItems/"+album, token, nil), &data)
	if data.Played || data.PlayCount != 0 {
		t.Errorf("an album marked unplayed %+v", data)
	}
	if status := jfSend(t, server, http.MethodPost, "/UserPlayedItems/"+album+"?datePlayed=yesterday", token, ""); status != http.StatusBadRequest {
		t.Errorf("a date that is none got %d", status)
	}

	// a position set by hand is resumed from
	decode(t, jf(t, server, http.MethodPost, "/UserItems/"+one.Id+"/UserData", token,
		strings.NewReader(`{"PlaybackPositionTicks":1234}`)), &data)
	if data.PlaybackPositionTicks != 1234 || data.Played {
		t.Errorf("a position set %+v", data)
	}
	if got := jfList(t, server, "/UserItems/Resume?MediaTypes=Audio", token).names(); !slices.Equal(got, []string{"One"}) {
		t.Errorf("resumed %q", got)
	}
	if got := jfList(t, server, "/UserItems/Resume?MediaTypes=Video", token).names(); len(got) != 0 {
		t.Errorf("resumed videos %q", got)
	}

	// what is kept is the account's own
	anna := jfToken(t, server, "anna", "secret")
	decode(t, jf(t, server, http.MethodGet, "/Items/"+one.Id, anna, nil), &item)
	if item.UserData.PlayCount != 0 || item.UserData.PlaybackPositionTicks != 0 {
		t.Errorf("another account sees %+v", item.UserData)
	}
	solo := ""
	for _, track := range jfList(t, server, tracks, token).Items {
		if track.Name == "Solo song" {
			solo = track.Id
		}
	}
	if status := jfSend(t, server, http.MethodPost, "/Sessions/Playing", anna, `{"ItemId":"`+solo+`"}`); status != http.StatusNotFound {
		t.Errorf("a report of a track the account may not read got %d", status)
	}
	if status := jfSend(t, server, http.MethodPost, "/UserFavoriteItems/"+solo, anna, ""); status != http.StatusNotFound {
		t.Errorf("a favorite the account may not read got %d", status)
	}
	if status := jfSend(t, server, http.MethodPost, "/Sessions/Playing", token, "not json"); status != http.StatusBadRequest {
		t.Errorf("a report that is not JSON got %d", status)
	}
}

func TestJellyfinKeepsDisplayPreferences(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) { cfg.Store = st })
	token := jfToken(t, server, "john", "doe")
	var prefs struct {
		SortBy      string
		CustomPrefs map[string]string
	}
	path := "/DisplayPreferences/usersettings?userId=x&client=emby"
	if status := jfSend(t, server, http.MethodPost, path, token,
		`{"Id":"usersettings","SortBy":"Album","CustomPrefs":{"homesection0":"latestmedia"}}`); status != http.StatusNoContent {
		t.Fatalf("keeping preferences got %d", status)
	}
	decode(t, jf(t, server, http.MethodGet, path, token, nil), &prefs)
	if prefs.SortBy != "Album" || prefs.CustomPrefs["homesection0"] != "latestmedia" {
		t.Errorf("kept preferences %+v", prefs)
	}
	decode(t, jf(t, server, http.MethodGet, "/DisplayPreferences/usersettings?client=other", token, nil), &prefs)
	if prefs.SortBy != "SortName" {
		t.Errorf("another client's preferences %+v", prefs)
	}
	anna := jfToken(t, server, "anna", "secret")
	decode(t, jf(t, server, http.MethodGet, path, anna, nil), &prefs)
	if prefs.SortBy != "SortName" {
		t.Errorf("another account's preferences %+v", prefs)
	}
	for _, body := range []string{"[1]", "null", "{"} {
		if status := jfSend(t, server, http.MethodPost, path, token, body); status != http.StatusBadRequest {
			t.Errorf("preferences of %s got %d", body, status)
		}
	}
}

func TestJellyfinLogout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "go-fs.db"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, _ := newJellyfinServer(t, func(cfg *httpConfig) { cfg.Store = st })
	token := jfToken(t, server, "john", "doe")
	other := jfToken(t, server, "john", "doe")
	if status := jfSend(t, server, http.MethodPost, "/Sessions/Logout", token, ""); status != http.StatusNoContent {
		t.Fatalf("logout got %d", status)
	}
	if status := jfSend(t, server, http.MethodGet, "/System/Info", token, ""); status != http.StatusUnauthorized {
		t.Errorf("a token logged out with got %d", status)
	}
	if status := jfSend(t, server, http.MethodGet, "/System/Info", other, ""); status != http.StatusOK {
		t.Errorf("another token of the account got %d", status)
	}

	// without a persistent data file, a token cannot be taken back
	plain, _ := newJellyfinServer(t, nil)
	token = jfToken(t, plain, "john", "doe")
	if status := jfSend(t, plain, http.MethodPost, "/Sessions/Logout", token, ""); status != http.StatusNoContent {
		t.Fatalf("logout without a store got %d", status)
	}
	if status := jfSend(t, plain, http.MethodGet, "/System/Info", token, ""); status != http.StatusOK {
		t.Errorf("a token logged out with, without a store, got %d", status)
	}
}
