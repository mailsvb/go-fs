package httpd

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

// song is an MP3 file with a tag, and frames whose volume can be raised.
func song(title string) string {
	return string(audiotest.MP3(audiotest.Tags{Title: title, Artist: "Band", Composer: "Writer"}, 40, true))
}

// mp3Do sends a request of the editor as an account, with the headers given.
func mp3Do(t *testing.T, server *testServer, method, link, user string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, server.url(link), reader)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, "doe")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return do(t, req)
}

// mp3Info reads what the editor shows of a response.
func mp3Info(t *testing.T, res *http.Response) mp3InfoJSON {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reading the tag answered %d: %s", res.StatusCode, bodyOf(t, res))
	}
	var info mp3InfoJSON
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info
}

func mp3Server(t *testing.T) *testServer {
	t.Helper()
	return newServer(t, func(cfg *httpConfig) {
		publicServer(cfg)
		reader := fullUser("kim", "doe")
		reader.AllowUserFileOverwrite = new(false)
		cfg.Users = append(cfg.Users, reader)
	})
}

// The editor is offered to an account that may replace a file, in the menu
// of an MP3 file, and for the whole folder where it has one.
func TestListingOffersMP3TagsToWhoMayReplace(t *testing.T) {
	skipWithout(t, features.MP3Tags)
	server := mp3Server(t)
	server.write(t, "music/song.mp3", song("One"))
	server.write(t, "docs/notes.txt", "hello")

	page := func(user, folder string) string {
		t.Helper()
		var res *http.Response
		if user == "" {
			res, _ = get(t, server, folder)
		} else {
			res = basic(t, server, http.MethodGet, folder, user, "doe", nil)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		return bodyOf(t, res)
	}
	body := page("john", "/music/")
	for _, want := range []string{`id="mp3-tags"`, `data-do="mp3tag"`, `id="mp3-dialog"`, `id="mp3-folder-dialog"`,
		`<datalist id="mp3-genres">`, `<option value="Rock">`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if body := page("john", "/docs/"); strings.Contains(body, `id="mp3-tags"`) {
		t.Error("Edit MP3 Tags is offered in a folder without an MP3 file")
	}
	for _, user := range []string{"", "kim"} {
		if body := page(user, "/music/"); strings.Contains(body, `data-do="mp3tag"`) || strings.Contains(body, `id="mp3-dialog"`) {
			t.Errorf("the editor is offered to %q, who may not replace a file", user)
		}
	}
}

func TestMP3TagsAreReadAndSaved(t *testing.T) {
	skipWithout(t, features.MP3Tags)
	server := mp3Server(t)
	server.write(t, "music/song.mp3", song("One"))
	link := "/music/song.mp3?go-fs=mp3tags&mp3tags="

	info := mp3Info(t, mp3Do(t, server, http.MethodGet, link+"tags", "kim", nil, nil))
	if info.Fields.Title != "One" || info.Fields.Artist != "Band" || info.Cover != nil {
		t.Errorf("read %+v", info.Info)
	}

	edit := map[string]any{"fields": map[string]string{"title": "Two", "artist": "Band"}}
	unchanged := map[string]string{"If-Unmodified-Since": info.Modified}
	if res := mp3Do(t, server, http.MethodPost, link+"tags", "kim", edit, unchanged); res.StatusCode != http.StatusForbidden {
		t.Errorf("a save by an account that may not replace the file answered %d", res.StatusCode)
	}
	if res := mp3Do(t, server, http.MethodPost, link+"tags", "john", edit,
		map[string]string{"Sec-Fetch-Site": "cross-site"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a save from another site answered %d", res.StatusCode)
	}
	stale := map[string]string{"If-Unmodified-Since": time.Unix(1000, 0).UTC().Format(http.TimeFormat)}
	if res := mp3Do(t, server, http.MethodPost, link+"tags", "john", edit, stale); res.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("a save of a file changed since it was read answered %d", res.StatusCode)
	}
	if res := mp3Do(t, server, http.MethodPost, link+"tags", "john", edit, unchanged); res.StatusCode != http.StatusNoContent {
		t.Fatalf("the save answered %d: %s", res.StatusCode, bodyOf(t, res))
	}
	saved := mp3Info(t, mp3Do(t, server, http.MethodGet, link+"tags", "john", nil, nil))
	if saved.Fields.Title != "Two" {
		t.Errorf("title after the save = %q", saved.Fields.Title)
	}
	if !strings.Contains(server.read(t, "music/song.mp3"), "Writer") {
		t.Error("the save lost the composer, which the editor does not manage")
	}
	if server.logs.find("http mp3 tags") == nil {
		t.Error("the save was not logged")
	}

	if res := mp3Do(t, server, http.MethodGet, link+"cover", "john", nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("the cover of a file without one answered %d", res.StatusCode)
	}
	if res := mp3Do(t, server, http.MethodPost, link+"cover", "john", nil, nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST of the cover answered %d", res.StatusCode)
	}
	server.write(t, "music/notes.txt", "hello")
	if res := mp3Do(t, server, http.MethodGet, "/music/notes.txt?go-fs=mp3tags&mp3tags=tags", "john", nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a file that is no MP3 answered %d", res.StatusCode)
	}
}

func TestMP3VolumeIsMeasuredAndChanged(t *testing.T) {
	skipWithout(t, features.MP3Tags)
	server := mp3Server(t)
	server.write(t, "music/a.mp3", song("A"))
	server.write(t, "music/b.mp3", song("B"))
	link := "/music/a.mp3?go-fs=mp3tags&mp3tags="

	if res := mp3Do(t, server, http.MethodPost, link+"analyze", "kim", struct{}{}, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("measuring by an account that may not replace the file answered %d", res.StatusCode)
	}
	res := mp3Do(t, server, http.MethodPost, link+"analyze", "john", struct{}{}, nil)
	var analysis struct {
		MaxUp int `json:"maxUp"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&analysis) != nil || analysis.MaxUp != 255 {
		t.Fatalf("measuring answered %d, %+v", res.StatusCode, analysis)
	}
	res = mp3Do(t, server, http.MethodPost, "/music/?go-fs=mp3tags&mp3tags=album", "john",
		map[string]any{"names": []string{"a.mp3", "b.mp3"}}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("planning the album answered %d: %s", res.StatusCode, bodyOf(t, res))
	}
	if res := mp3Do(t, server, http.MethodPost, "/music/?go-fs=mp3tags&mp3tags=album", "john",
		map[string]any{"names": []string{"../a.mp3"}}, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an album with a name outside the folder answered %d", res.StatusCode)
	}

	if res := mp3Do(t, server, http.MethodPost, link+"tags", "john",
		map[string]any{"gain": map[string]any{"steps": -1}}, nil); res.StatusCode != http.StatusConflict {
		t.Errorf("a change the file does not allow answered %d", res.StatusCode)
	}
	if res := mp3Do(t, server, http.MethodPost, link+"tags", "john",
		map[string]any{"gain": map[string]any{"steps": 2, "replayGain": true}}, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("changing the volume answered %d: %s", res.StatusCode, bodyOf(t, res))
	}
	if info := mp3Info(t, mp3Do(t, server, http.MethodGet, link+"tags", "john", nil, nil)); info.Gain.Steps != 2 {
		t.Errorf("gain after the change = %+v", info.Gain)
	}
}

// On a stored server the editor reads the file there and uploads it again,
// where the server allows uploads.
func TestRemoteMP3TagsAreSaved(t *testing.T) {
	skipWithout(t, features.SFTPRemote, features.MP3Tags)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	host.Write(t, "music/song.mp3", song("One"))
	server, session := remoteServer(t, host, func(cfg *httpConfig) {
		closed := storedServer(host, "Closed")
		closed.AllowUpload = ptr(false)
		cfg.Servers = append(cfg.Servers, closed)
		cfg.Users = append(cfg.Users, readOnlyUser("reader", "pw"))
	})
	link := remoteAt(host, "music/song.mp3") + "&" + mp3PartParam + "="

	if body := bodyOf(t, remoteDo(t, server, http.MethodGet, remoteAt(host, "music"), session, nil, nil)); !strings.Contains(body, `id="mp3-tags"`) {
		t.Error("the page of the server does not offer the editor")
	}
	res := remoteDo(t, server, http.MethodGet, link+"tags", session, nil, nil)
	info := mp3Info(t, res)
	if info.Fields.Title != "One" {
		t.Errorf("read %+v", info.Info)
	}
	edit := strings.NewReader(`{"fields":{"title":"Two","artist":"Band"}}`)
	res = remoteDo(t, server, http.MethodPost, link+"tags", session, edit,
		map[string]string{"If-Unmodified-Since": info.Modified, "Content-Type": "application/json"})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("the save answered %d: %s", res.StatusCode, bodyOf(t, res))
	}
	if info := mp3Info(t, remoteDo(t, server, http.MethodGet, link+"tags", session, nil, nil)); info.Fields.Title != "Two" {
		t.Errorf("title after the save = %q", info.Fields.Title)
	}
	if !strings.Contains(remoteFile(host, "music/song.mp3"), "Writer") {
		t.Error("the save lost the composer")
	}
	if left := host.Leftovers(t, "music"); len(left) != 0 {
		t.Errorf("the save left %v", left)
	}

	reader := login(t, server, "/", "reader", "pw")
	if res := remoteDo(t, server, http.MethodPost, link+"tags", reader, strings.NewReader(`{}`), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("a save by a reader answered %d", res.StatusCode)
	}
	if body := bodyOf(t, remoteDo(t, server, http.MethodGet, remoteAt(host, "music"), reader, nil, nil)); strings.Contains(body, `data-do="mp3tag"`) {
		t.Error("the page of a reader offers the editor")
	}
	closed := remoteURL("Closed", remotetest.Path(host.Dir+"/music/song.mp3")) + "&" + mp3PartParam + "=tags"
	if res := remoteDo(t, server, http.MethodPost, closed, session, strings.NewReader(`{}`), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("a save on a server that allows no uploads answered %d", res.StatusCode)
	}
	if body := bodyOf(t, remoteDo(t, server, http.MethodGet, remoteURL("Closed", remotetest.Path(host.Dir+"/music")), session, nil, nil)); strings.Contains(body, `data-do="mp3tag"`) {
		t.Error("the page of a server that allows no uploads offers the editor")
	}
}
