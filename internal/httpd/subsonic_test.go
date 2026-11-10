//go:build !light && !no_subsonic

package httpd

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/subsonic"
)

// newSubsonicServer serves a library in /music of two albums, as
// newJellyfinServer does, to OpenSubsonic clients: one of a band, with a cover
// beside it, and one of someone else, whose track carries its picture, its
// ReplayGain and its composer. john reads everything; anna reads the band
// only.
func newSubsonicServer(t *testing.T, tune func(*httpConfig)) (*testServer, *jellyfin.Library) {
	t.Helper()
	anna := readOnlyUser("anna", "secret")
	anna.Paths = []string{"^/music/Band/.*"}
	server := newServer(t, func(cfg *httpConfig) {
		publicServer(cfg)
		cfg.SubsonicFolders = []string{"/music"}
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
			Album: "Solo", Genre: "Jazz", Composer: "Writer", Picture: audiotest.PNG, Year: 1999,
			Extra: map[string]string{"REPLAYGAIN_TRACK_GAIN": "-6.50 dB", "REPLAYGAIN_ALBUM_GAIN": "-5.00 dB"}}, 30),
	}
	var sources []jellyfin.Source
	var hrefs []string
	for _, href := range slices.Sorted(func(yield func(string) bool) {
		for href := range files {
			if !yield(href) {
				return
			}
		}
	}) {
		path := server.write(t, "music/"+href, string(files[href]))
		if strings.HasSuffix(href, ".png") {
			continue
		}
		track, err := audio.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, jellyfin.Source{Href: href, Size: int64(len(files[href])),
			ModTime: time.Now(), Track: track})
		hrefs = append(hrefs, href)
	}
	onDisk := filepath.Join(server.base, "music")
	library := jellyfin.Build("music", "Our music", "/music", onDisk, sources,
		jellyfin.Covers(onDisk, jellyfin.Folders(hrefs)), time.Now())
	jellyfin.Publish(config.RepositoryTypeSubsonic, library)
	t.Cleanup(func() { jellyfin.Retire(config.RepositoryTypeSubsonic, "music") })
	return server, library
}

// subParams are the parameters a client sends with every call: john's
// password, and JSON answers.
func subParams(extra ...string) url.Values {
	params := url.Values{"u": {"john"}, "p": {"doe"}, "v": {subsonic.Version}, "c": {"test"}, "f": {"json"}}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "" {
			params.Del(extra[i])
			continue
		}
		params.Add(extra[i], extra[i+1])
	}
	return params
}

// subGet calls a method with GET, as clients do, with params in the query.
func subGet(t *testing.T, server *testServer, method string, params url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.url("/rest/"+method+".view?"+params.Encode()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

// subCall calls a method and reads its JSON answer.
func subCall(t *testing.T, server *testServer, method string, params url.Values) subsonic.Response {
	t.Helper()
	res := subGet(t, server, method, params)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, res.StatusCode, body)
	}
	var answer struct {
		Response subsonic.Response `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("%s: %v: %s", method, err, body)
	}
	return answer.Response
}

// subOK calls a method that has to work.
func subOK(t *testing.T, server *testServer, method string, params url.Values) subsonic.Response {
	t.Helper()
	r := subCall(t, server, method, params)
	if r.Status != "ok" || r.Error != nil {
		t.Fatalf("%s failed: %+v", method, r.Error)
	}
	if r.Version != subsonic.Version || r.Type != "go-fs" || !r.OpenSubsonic {
		t.Errorf("%s: the envelope is %+v", method, r)
	}
	return r
}

// subFails calls a method that has to fail with code.
func subFails(t *testing.T, server *testServer, method string, params url.Values, code int) {
	t.Helper()
	r := subCall(t, server, method, params)
	if r.Status != "failed" || r.Error == nil || r.Error.Code != code {
		t.Errorf("%s: got %s %+v, want error %d", method, r.Status, r.Error, code)
	}
}

func TestSubsonicLogins(t *testing.T) {
	token, plain := fullToken(t, "phone")
	server, _ := newSubsonicServer(t, func(cfg *httpConfig) { cfg.Tokens = []config.Token{token} })

	subOK(t, server, "ping", subParams())
	subOK(t, server, "ping", subParams("p", "", "p", "enc:"+hex.EncodeToString([]byte("doe"))))
	salt := "c19b2d"
	sum := md5.Sum([]byte("doe" + salt))
	subOK(t, server, "ping", subParams("p", "", "t", hex.EncodeToString(sum[:]), "s", salt))
	subFails(t, server, "ping", subParams("p", "", "t", hex.EncodeToString(sum[:]), "s", "other1"), subsonic.ErrCredentials)
	subFails(t, server, "ping", subParams("p", "", "p", "wrong"), subsonic.ErrCredentials)
	subFails(t, server, "ping", subParams("u", ""), subsonic.ErrMissing)
	subFails(t, server, "ping", subParams("p", ""), subsonic.ErrMissing)
	if entry := server.logs.find("subsonic login"); entry == nil || entry["user"] != "john" {
		t.Errorf("the login is logged as %v", entry)
	}

	// a bearer token is an API key, which comes without a name
	keyed := url.Values{"apiKey": {plain}, "v": {subsonic.Version}, "c": {"test"}, "f": {"json"}}
	if r := subOK(t, server, "tokenInfo", keyed); r.TokenInfo == nil || r.TokenInfo.Username != "token:phone" {
		t.Errorf("the key is of %+v", r.TokenInfo)
	}
	keyed.Set("u", "john")
	subFails(t, server, "ping", keyed, subsonic.ErrConflicting)
	keyed.Del("u")
	keyed.Set("apiKey", "gofs_wrong")
	subFails(t, server, "ping", keyed, subsonic.ErrAPIKey)

	// the extensions are asked for before logging in
	r := subOK(t, server, "getOpenSubsonicExtensions", url.Values{"f": {"json"}})
	var names []string
	for _, e := range r.OpenSubsonicExtensions {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"apiKeyAuthentication", "formPost"}) {
		t.Errorf("extensions %q", names)
	}

	// and every argument may come in the body of a form instead
	res, err := http.Post(server.url("/rest/ping"), "application/x-www-form-urlencoded",
		strings.NewReader(subParams().Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if body := bodyOf(t, res); !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("a form post is answered %s", body)
	}
}

// Wrong passwords count towards the login lock, as everywhere.
func TestSubsonicWrongPasswordsLockTheAddress(t *testing.T) {
	server, _ := newSubsonicServer(t, func(cfg *httpConfig) {
		cfg.LoginAttempts = 2
		cfg.LoginLockout = 60
	})
	fixClock(server)
	for range 2 {
		subFails(t, server, "ping", subParams("p", "", "p", "wrong"), subsonic.ErrCredentials)
	}
	res := subGet(t, server, "ping", subParams())
	if body := bodyOf(t, res); !strings.Contains(body, `"code":40`) || res.Header.Get("Retry-After") == "" {
		t.Errorf("the locked address was answered %s, Retry-After %q", body, res.Header.Get("Retry-After"))
	}
}

func TestSubsonicIsToldApart(t *testing.T) {
	server, _ := newSubsonicServer(t, nil)
	server.write(t, "rest/notes.txt", "a folder of files")
	server.write(t, "rest/ping", "a file named after a method")

	// a file of a folder rest is still a file, but for the methods
	if res, body := get(t, server, "/rest/notes.txt"); res.StatusCode != http.StatusOK || body != "a folder of files" {
		t.Errorf("a file below /rest: %d %q", res.StatusCode, body)
	}
	if _, body := get(t, server, "/rest/ping"); !strings.Contains(body, "subsonic-response") {
		t.Errorf("/rest/ping is not the API: %q", body)
	}
	// an answer is XML unless JSON is asked for
	params := subParams("f", "")
	res := subGet(t, server, "ping", params)
	body := bodyOf(t, res)
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/xml") ||
		!strings.Contains(body, `<subsonic-response xmlns="http://subsonic.org/restapi" status="ok" version="1.16.1" type="go-fs"`) {
		t.Errorf("XML answer %q: %s", res.Header.Get("Content-Type"), body)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("the API sends no CORS header")
	}
	// a method go-fs does not know is answered as one
	subFails(t, server, "getVideoInfo", subParams(), subsonic.ErrGeneric)
	subFails(t, server, "createPlaylist", subParams("name", "x"), subsonic.ErrNotAuthorized)

	// JSONP calls the function it is asked to
	res = subGet(t, server, "ping", subParams("f", "", "f", "jsonp", "callback", "cb"))
	if body := bodyOf(t, res); !strings.HasPrefix(body, `cb({"subsonic-response":`) {
		t.Errorf("JSONP %s", body)
	}
	res = subGet(t, server, "ping", subParams("f", "", "f", "jsonp", "callback", "alert(1)//"))
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a callback that is no name: %d", res.StatusCode)
	}

	// the folder of the library needs an account, though everything is public
	if res, _ := get(t, server, "/music/Band/First/cover.png"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the library's folder without an account: %d", res.StatusCode)
	}
}

func TestSubsonicBrowses(t *testing.T) {
	server, library := newSubsonicServer(t, nil)

	folders := subOK(t, server, "getMusicFolders", subParams()).MusicFolders
	if folders == nil || len(folders.MusicFolder) != 1 || folders.MusicFolder[0].Name != "Our music" ||
		folders.MusicFolder[0].ID != subsonic.FolderID(library) {
		t.Fatalf("music folders %+v", folders)
	}
	folderID := strconv.Itoa(folders.MusicFolder[0].ID)

	artists := subOK(t, server, "getArtists", subParams("musicFolderId", folderID)).Artists
	var letters []string
	for _, index := range artists.Index {
		letters = append(letters, index.Name)
	}
	// The Band sorts by B
	if !slices.Equal(letters, []string{"B", "S"}) || artists.Index[0].Artist[0].Name != "The Band" {
		t.Fatalf("artists %+v", artists)
	}
	subFails(t, server, "getArtists", subParams("musicFolderId", "1"), subsonic.ErrNotFound)

	band := subOK(t, server, "getArtist", subParams("id", artists.Index[0].Artist[0].ID)).Artist
	if band == nil || len(band.Album) != 1 || band.Album[0].Name != "First" || band.Album[0].SongCount != 2 {
		t.Fatalf("artist %+v", band)
	}
	album := subOK(t, server, "getAlbum", subParams("id", band.Album[0].ID)).Album
	if album == nil || len(album.Song) != 2 || album.Song[0].Title != "One" || album.Song[1].Title != "Two" {
		t.Fatalf("album %+v", album)
	}
	one := album.Song[0]
	if one.Suffix != "mp3" || one.ContentType != "audio/mpeg" || one.CoverArt != album.ID || one.Track != 1 ||
		one.Year != 2001 || one.Genre != "Rock" || one.Path != "Band/First/01 One.mp3" || one.IsDir ||
		one.MediaType != "song" || one.ReplayGain == nil || len(one.Genres) != 1 {
		t.Errorf("song %+v", one)
	}
	subFails(t, server, "getAlbum", subParams("id", one.ID), subsonic.ErrNotFound)
	subFails(t, server, "getAlbum", subParams("id", "nothing"), subsonic.ErrNotFound)
	subFails(t, server, "getAlbum", subParams(), subsonic.ErrMissing)

	var solo *jellyfin.Track
	for _, tr := range library.Tracks {
		if tr.Name == "Solo song" {
			solo = tr
		}
	}
	song := subOK(t, server, "getSong", subParams("id", solo.ID)).Song
	if song == nil || song.DisplayComposer != "Writer" || song.ReplayGain == nil || song.ReplayGain.TrackGain == nil ||
		*song.ReplayGain.TrackGain != -6.5 || *song.ReplayGain.AlbumGain != -5 || song.Suffix != "flac" ||
		song.CoverArt == "" {
		t.Errorf("solo %+v", song)
	}

	// the directories are the library, its artists and their albums
	dir := subOK(t, server, "getMusicDirectory", subParams("id", library.ID)).Directory
	if dir == nil || len(dir.Child) != 2 || !dir.Child[0].IsDir || dir.Child[0].Title != "The Band" {
		t.Fatalf("library directory %+v", dir)
	}
	dir = subOK(t, server, "getMusicDirectory", subParams("id", dir.Child[0].ID)).Directory
	if len(dir.Child) != 1 || dir.Child[0].Title != "First" || !dir.Child[0].IsDir {
		t.Fatalf("artist directory %+v", dir)
	}
	dir = subOK(t, server, "getMusicDirectory", subParams("id", dir.Child[0].ID)).Directory
	if len(dir.Child) != 2 || dir.Child[0].IsDir || dir.Parent != band.ID {
		t.Fatalf("album directory %+v", dir)
	}

	genres := subOK(t, server, "getGenres", subParams()).Genres
	if genres == nil || len(genres.Genre) != 2 || genres.Genre[0].Value != "Jazz" || genres.Genre[1].SongCount != 2 ||
		genres.Genre[1].AlbumCount != 1 {
		t.Errorf("genres %+v", genres)
	}

	// an index not built again since is not sent again
	indexes := subOK(t, server, "getIndexes", subParams()).Indexes
	if indexes == nil || len(indexes.Index) != 2 || indexes.LastModified == 0 {
		t.Fatalf("indexes %+v", indexes)
	}
	since := strconv.FormatInt(indexes.LastModified, 10)
	if again := subOK(t, server, "getIndexes", subParams("ifModifiedSince", since)).Indexes; len(again.Index) != 0 {
		t.Errorf("indexes sent again %+v", again)
	}

	user := subOK(t, server, "getUser", subParams("username", "john")).User
	if user == nil || !user.StreamRole || !user.DownloadRole || user.AdminRole || !slices.Equal(user.Folder,
		[]int{subsonic.FolderID(library)}) {
		t.Errorf("user %+v", user)
	}
	subFails(t, server, "getUser", subParams("username", "anna"), subsonic.ErrNotAuthorized)
	if status := subOK(t, server, "getScanStatus", subParams()).ScanStatus; status == nil || status.Count != 3 {
		t.Errorf("scan status %+v", status)
	}
}

func TestSubsonicListsAndSearches(t *testing.T) {
	server, _ := newSubsonicServer(t, nil)

	list := subOK(t, server, "getAlbumList2", subParams("type", "alphabeticalByName")).AlbumList2
	if list == nil || len(list.Album) != 2 || list.Album[0].Name != "First" || list.Album[1].Name != "Solo" {
		t.Fatalf("albums %+v", list)
	}
	list = subOK(t, server, "getAlbumList2", subParams("type", "byYear", "fromYear", "2010", "toYear", "1990")).AlbumList2
	if len(list.Album) != 2 || list.Album[0].Year != 2001 {
		t.Errorf("albums from 2010 back to 1990 %+v", list)
	}
	list = subOK(t, server, "getAlbumList2", subParams("type", "byGenre", "genre", "jazz")).AlbumList2
	if len(list.Album) != 1 || list.Album[0].Name != "Solo" {
		t.Errorf("jazz albums %+v", list)
	}
	subFails(t, server, "getAlbumList2", subParams("type", "byYear"), subsonic.ErrMissing)
	subFails(t, server, "getAlbumList2", subParams(), subsonic.ErrMissing)
	old := subOK(t, server, "getAlbumList", subParams("type", "newest", "size", "1")).AlbumList
	if old == nil || len(old.Album) != 1 || !old.Album[0].IsDir {
		t.Errorf("album list %+v", old)
	}

	// an empty query is everything, page by page
	all := subOK(t, server, "search3", subParams("query", `""`, "songCount", "2")).SearchResult3
	if all == nil || len(all.Song) != 2 || len(all.Album) != 2 || len(all.Artist) != 2 {
		t.Fatalf("everything %+v", all)
	}
	rest := subOK(t, server, "search3", subParams("query", "", "songCount", "2", "songOffset", "2")).SearchResult3
	if len(rest.Song) != 1 || slices.ContainsFunc(all.Song, func(c subsonic.Child) bool { return c.ID == rest.Song[0].ID }) {
		t.Errorf("the second page %+v", rest.Song)
	}
	found := subOK(t, server, "search3", subParams("query", "solo", "artistCount", "0")).SearchResult3
	if len(found.Song) != 1 || len(found.Album) != 1 || len(found.Artist) != 0 {
		t.Errorf("solo %+v", found)
	}
	found2 := subOK(t, server, "search2", subParams("query", "band two")).SearchResult2
	if len(found2.Song) != 1 || found2.Song[0].Title != "Two" {
		t.Errorf("band two %+v", found2)
	}

	random := subOK(t, server, "getRandomSongs", subParams("size", "10", "genre", "Rock")).RandomSongs
	if random == nil || len(random.Song) != 2 {
		t.Errorf("random rock %+v", random)
	}
	byGenre := subOK(t, server, "getSongsByGenre", subParams("genre", "Rock", "count", "1", "offset", "1")).SongsByGenre
	if byGenre == nil || len(byGenre.Song) != 1 || byGenre.Song[0].Title != "Two" {
		t.Errorf("songs by genre %+v", byGenre)
	}
	if r := subOK(t, server, "getPlaylists", subParams()); r.Playlists == nil {
		t.Error("no empty playlists")
	}
}

func TestSubsonicSeesWhatTheAccountReads(t *testing.T) {
	server, library := newSubsonicServer(t, nil)
	anna := subParams("u", "", "p", "", "u", "anna", "p", "secret")
	all := subOK(t, server, "search3", withParams(anna, "query", "")).SearchResult3
	if len(all.Song) != 2 || len(all.Album) != 1 {
		t.Errorf("anna finds %+v", all)
	}
	for _, tr := range library.Tracks {
		if tr.Name == "Solo song" {
			subFails(t, server, "getSong", withParams(anna, "id", tr.ID), subsonic.ErrNotFound)
			subFails(t, server, "stream", withParams(anna, "id", tr.ID), subsonic.ErrNotFound)
		}
	}
	nobody := readOnlyUser("nobody", "x")
	nobody.Paths = []string{"^/elsewhere/.*"}
	server2, _ := newSubsonicServer(t, func(cfg *httpConfig) { cfg.Users = append(cfg.Users, nobody) })
	folders := subOK(t, server2, "getMusicFolders", subParams("u", "", "p", "", "u", "nobody", "p", "x")).MusicFolders
	if folders != nil && len(folders.MusicFolder) != 0 {
		t.Errorf("an account that reads nothing of the library sees %+v", folders)
	}
}

// withParams is params and more.
func withParams(params url.Values, more ...string) url.Values {
	out := url.Values{}
	for k, v := range params {
		out[k] = slices.Clone(v)
	}
	for i := 0; i+1 < len(more); i += 2 {
		out.Add(more[i], more[i+1])
	}
	return out
}

func TestSubsonicStreamsAndCovers(t *testing.T) {
	server, library := newSubsonicServer(t, nil)
	first := library.Albums[0]
	if first.Name != "First" {
		t.Fatalf("albums %v", library.Albums)
	}
	track := first.Tracks[0]

	req, err := http.NewRequest(http.MethodGet, server.url("/rest/stream?"+subParams("id", track.ID, "format", "mp3",
		"maxBitRate", "128").Encode()), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	res := do(t, req)
	if body := bodyOf(t, res); res.StatusCode != http.StatusPartialContent || len(body) != 10 ||
		res.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("a range of a stream: %d %q, %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(body))
	}
	if entry := server.logs.find("http download"); entry == nil || entry["via"] != "subsonic" {
		t.Errorf("the download is logged as %v", entry)
	}
	res = subGet(t, server, "download", subParams("id", track.ID))
	if bodyOf(t, res); res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), "01 One.mp3") {
		t.Errorf("a download: %d %q", res.StatusCode, res.Header.Get("Content-Disposition"))
	}

	// the cover beside the tracks, and the picture a track carries
	res = subGet(t, server, "getCoverArt", subParams("id", first.ID, "size", "300"))
	if body := bodyOf(t, res); res.StatusCode != http.StatusOK || body != string(audiotest.PNG) {
		t.Errorf("the cover of First: %d, %d bytes", res.StatusCode, len(body))
	}
	res = subGet(t, server, "getCoverArt", subParams("id", library.Albums[1].ID))
	if bodyOf(t, res); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("the picture of Solo: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res = subGet(t, server, "getCoverArt", subParams("id", "nothing"))
	if bodyOf(t, res); res.StatusCode != http.StatusNotFound {
		t.Errorf("a cover that is not there: %d", res.StatusCode)
	}
	// covers need an account, unlike Jellyfin's
	res = subGet(t, server, "getCoverArt", url.Values{"id": {first.ID}})
	if body := bodyOf(t, res); !strings.Contains(body, `code="10"`) {
		t.Errorf("a cover without an account: %s", body)
	}
}

func TestSubsonicStarsAndScrobbles(t *testing.T) {
	server, library := newSubsonicServer(t, nil)
	first := library.Albums[0]
	track := first.Tracks[0]

	subOK(t, server, "star", subParams("id", track.ID, "albumId", first.ID))
	starred := subOK(t, server, "getStarred2", subParams()).Starred2
	if starred == nil || len(starred.Song) != 1 || len(starred.Album) != 1 || starred.Song[0].Starred == nil {
		t.Fatalf("starred %+v", starred)
	}
	if song := subOK(t, server, "getSong", subParams("id", track.ID)).Song; song.Starred == nil {
		t.Error("a starred song is not described as one")
	}
	list := subOK(t, server, "getAlbumList2", subParams("type", "starred")).AlbumList2
	if len(list.Album) != 1 || list.Album[0].ID != first.ID {
		t.Errorf("starred albums %+v", list)
	}
	subOK(t, server, "unstar", subParams("id", track.ID))
	if starred := subOK(t, server, "getStarred", subParams()).Starred; len(starred.Song) != 0 || len(starred.Album) != 1 {
		t.Errorf("after unstarring the song %+v", starred)
	}
	subFails(t, server, "star", subParams("id", "nothing"), subsonic.ErrNotFound)
	subFails(t, server, "star", subParams(), subsonic.ErrMissing)

	// what is playing counts nothing, what was played counts once
	subOK(t, server, "scrobble", subParams("id", track.ID, "submission", "false"))
	if list := subOK(t, server, "getAlbumList2", subParams("type", "frequent")).AlbumList2; len(list.Album) != 0 {
		t.Errorf("now playing counted %+v", list)
	}
	subOK(t, server, "scrobble", subParams("id", track.ID))
	frequent := subOK(t, server, "getAlbumList2", subParams("type", "frequent")).AlbumList2
	if len(frequent.Album) != 1 || frequent.Album[0].PlayCount != 1 {
		t.Errorf("frequent %+v", frequent)
	}
	played := time.Now().Add(-48 * time.Hour).UnixMilli()
	other := library.Albums[1].Tracks[0]
	subOK(t, server, "scrobble", subParams("id", other.ID, "time", strconv.FormatInt(played, 10)))
	song := subOK(t, server, "getSong", subParams("id", other.ID)).Song
	if song.PlayCount != 1 || song.Played == nil || song.Played.UnixMilli() != played {
		t.Errorf("an offline scrobble %+v", song)
	}
	recent := subOK(t, server, "getAlbumList2", subParams("type", "recent")).AlbumList2
	if len(recent.Album) != 2 || recent.Album[0].ID != first.ID {
		t.Errorf("recent %+v", recent)
	}
	// anna sees none of what john did
	anna := subParams("u", "", "p", "", "u", "anna", "p", "secret")
	if list := subOK(t, server, "getAlbumList2", withParams(anna, "type", "frequent")).AlbumList2; len(list.Album) != 0 {
		t.Errorf("anna's frequent %+v", list)
	}
}
