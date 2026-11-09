//go:build !light && !no_sonos

package httpd

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/sonos"
)

const speakerID = "RINCON_000E58A0B1C201400"

// speaker stands in for a Sonos speaker: it describes itself, does not say
// how it is grouped, and plays whatever it is told to, recording the calls.
type speaker struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []string
	uri   string
}

func newSpeaker(t *testing.T) *speaker {
	t.Helper()
	sp := &speaker{}
	sp.server = httptest.NewServer(http.HandlerFunc(sp.serve))
	t.Cleanup(sp.server.Close)
	return sp
}

func (sp *speaker) host() string { return sp.server.Listener.Addr().String() }

func (sp *speaker) taken() ([]string, string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return append([]string(nil), sp.calls...), sp.uri
}

func (sp *speaker) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/xml/device_description.xml" {
		_, _ = fmt.Fprintf(w, `<root><device><deviceType>urn:schemas-upnp-org:device:ZonePlayer:1</deviceType>`+
			`<modelName>Sonos One</modelName><UDN>uuid:%s</UDN><roomName>Kitchen</roomName></device></root>`, speakerID)
		return
	}
	urn, action, _ := strings.Cut(strings.Trim(r.Header.Get("SOAPACTION"), `"`), "#")
	body, _ := io.ReadAll(r.Body)
	var env struct {
		URI string `xml:"Body>SetAVTransportURI>CurrentURI"`
	}
	_ = xml.Unmarshal(body, &env)
	sp.mu.Lock()
	sp.calls = append(sp.calls, action)
	if action == "SetAVTransportURI" {
		sp.uri = env.URI
	}
	uri := sp.uri
	sp.mu.Unlock()

	out := map[string]string{}
	switch action {
	case "GetZoneGroupState":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail>`+
			`<UPnPError><errorCode>401</errorCode></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
		return
	case "GetTransportInfo":
		out["CurrentTransportState"] = "PLAYING"
	case "GetPositionInfo":
		out["TrackURI"] = uri
	case "GetGroupVolume":
		out["CurrentVolume"] = "30"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="%s">`, action, urn)
	for name, value := range out {
		fmt.Fprintf(&b, "<%s>%s</%s>", name, html.EscapeString(value), name)
	}
	fmt.Fprintf(&b, "</u:%sResponse></s:Body></s:Envelope>", action)
	_, _ = io.WriteString(w, b.String())
}

// sonosServer is a server with Play on Sonos, which finds the one speaker
// http.sonosSpeakers names: its search goes to a socket that never answers.
func sonosServer(t *testing.T, sp *speaker, tune func(*httpConfig)) *testServer {
	t.Helper()
	skipWithout(t, features.Sonos)
	server := newServer(t, func(cfg *httpConfig) {
		cfg.Sonos = true
		if sp != nil {
			cfg.SonosSpeakers = []string{sp.host()}
		}
		if tune != nil {
			tune(cfg)
		}
	})
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	client := sonos.New()
	client.Search = silent.LocalAddr().String()
	server.sonos.client = client
	server.sonos.wait = 20 * time.Millisecond
	return server
}

// sonosAsk sends a request of the dialog in a session.
func sonosAsk(t *testing.T, server *testServer, session *http.Cookie, method, path string, body any, header http.Header) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, server.url(path), reader)
	if err != nil {
		t.Fatal(err)
	}
	if session != nil {
		req.AddCookie(session)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	res := do(t, req)
	text, _ := io.ReadAll(res.Body)
	return res, string(text)
}

func TestListingOffersSonosToASessionThatMayRead(t *testing.T) {
	server := sonosServer(t, nil, nil)
	server.write(t, "music/song.mp3", "x")
	session := login(t, server, "/music/", "john", "doe")
	if body := bodyOf(t, withSession(t, server, http.MethodGet, "/music/", session)); !strings.Contains(body, `data-do="sonos"`) {
		t.Error("a session is not offered Play on Sonos")
	}
	// Basic is no session, as with Send
	if body := bodyOf(t, basic(t, server, http.MethodGet, "/music/", "john", "doe", nil)); strings.Contains(body, `data-do="sonos"`) {
		t.Error("a request with Basic is offered Play on Sonos")
	}

	off := newServer(t, nil)
	off.write(t, "music/song.mp3", "x")
	session = login(t, off, "/music/", "john", "doe")
	if body := bodyOf(t, withSession(t, off, http.MethodGet, "/music/", session)); strings.Contains(body, `data-do="sonos"`) {
		t.Error("Play on Sonos is offered while http.sonos is off")
	}
}

func TestSonosPlaysAFileByALinkThatNeedsNoLogin(t *testing.T) {
	sp := newSpeaker(t)
	server := sonosServer(t, sp, nil)
	song := audiotest.MP3(audiotest.Tags{Title: "Morning", Artist: "Band"}, 20, false)
	server.write(t, "private/music/A & B #1.mp3", string(song))
	server.write(t, "private/music/other.mp3", "other")
	session := login(t, server, "/private/music/", "john", "doe")
	file := "/private/music/" + url.PathEscape("A & B #1.mp3")

	res, body := sonosAsk(t, server, session, http.MethodGet, file+"?go-fs=sonos&sonos=groups", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("groups answered %d: %s", res.StatusCode, body)
	}
	var groups struct {
		Groups []sonosGroupView `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].ID != speakerID || groups.Groups[0].Name != "Kitchen" {
		t.Fatalf("groups %+v", groups.Groups)
	}

	res, body = sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos=play", sonosRequest{Group: speakerID}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play answered %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"title":"Morning"`) {
		t.Errorf("play answered %s, want the title of the tag", body)
	}
	calls, link := sp.taken()
	if strings.Join(calls, ",") != "GetZoneGroupState,SetAVTransportURI,Play" {
		t.Errorf("the speaker was called %v", calls)
	}
	if !strings.HasPrefix(link, server.url(file+"?"+sonosPlayParam+"=")) {
		t.Fatalf("the speaker was told %s", link)
	}
	if server.logs.find("http sonos play") == nil {
		t.Error("the play was not logged")
	}

	// the speaker fetches the file with no account, and seeks in it
	req, _ := http.NewRequest(http.MethodGet, link, nil)
	res = do(t, req)
	if fetched := bodyOf(t, res); res.StatusCode != http.StatusOK || fetched != string(song) {
		t.Fatalf("the link answered %d with %d bytes", res.StatusCode, len(fetched))
	}
	if typ := res.Header.Get("Content-Type"); typ != "audio/mpeg" {
		t.Errorf("served as %s", typ)
	}
	req, _ = http.NewRequest(http.MethodGet, link, nil)
	req.Header.Set("Range", "bytes=10-19")
	if res = do(t, req); res.StatusCode != http.StatusPartialContent {
		t.Errorf("a range answered %d", res.StatusCode)
	}

	// the key is the file's: not another file's, nor a folder's
	key := strings.SplitN(link, "?", 2)[1]
	if res, _ := get(t, server, "/private/music/other.mp3?"+key); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("another file with the key answered %d", res.StatusCode)
	}
	if res, _ := get(t, server, "/private/music/?"+key); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the folder with the key answered %d", res.StatusCode)
	}

	// the dialog is told the speaker plays this file
	res, body = sonosAsk(t, server, session, http.MethodGet, file+"?go-fs=sonos&sonos=state&group="+speakerID, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"file":true`) ||
		!strings.Contains(body, `"transport":"PLAYING"`) || !strings.Contains(body, `"volume":30`) {
		t.Errorf("state answered %d: %s", res.StatusCode, body)
	}
	res, body = sonosAsk(t, server, session, http.MethodGet, "/private/music/other.mp3?go-fs=sonos&sonos=state&group="+speakerID, nil, nil)
	if !strings.Contains(body, `"file":false`) {
		t.Errorf("state of another file answered %d: %s", res.StatusCode, body)
	}

	for _, op := range []string{"pause", "resume", "stop"} {
		if res, body := sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos="+op, sonosRequest{Group: speakerID}, nil); res.StatusCode != http.StatusNoContent {
			t.Errorf("%s answered %d: %s", op, res.StatusCode, body)
		}
	}
	volume := 140
	if res, body := sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos=volume", sonosRequest{Group: speakerID, Volume: &volume}, nil); res.StatusCode != http.StatusNoContent {
		t.Errorf("volume answered %d: %s", res.StatusCode, body)
	}
	calls, _ = sp.taken()
	if got := strings.Join(calls[len(calls)-5:], ","); got != "Pause,Play,Stop,SnapshotGroupVolume,SetGroupVolume" {
		t.Errorf("the speaker was called %s", got)
	}

	// a file that changes is no longer served by the link
	server.write(t, "private/music/A & B #1.mp3", "changed")
	req, _ = http.NewRequest(http.MethodGet, link, nil)
	if res = do(t, req); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the link to a changed file answered %d", res.StatusCode)
	}
}

func TestSonosLinkRunsOut(t *testing.T) {
	server := sonosServer(t, nil, nil)
	path := server.write(t, "private/music/song.mp3", "song")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	set := server.settings()
	for name, expires := range map[string]time.Time{"past": time.Now().Add(-time.Minute), "future": time.Now().Add(time.Hour)} {
		key := sonosKey(server.shareSecret(set), "/private/music/song.mp3", info, expires.Unix())
		res, _ := get(t, server, "/private/music/song.mp3?"+sonosPlayParam+"="+url.QueryEscape(key))
		want := http.StatusOK
		if name == "past" {
			want = http.StatusUnauthorized
		}
		if res.StatusCode != want {
			t.Errorf("a key of the %s answered %d, want %d", name, res.StatusCode, want)
		}
	}
	// the time is part of what is signed
	key := sonosKey(server.shareSecret(set), "/private/music/song.mp3", info, time.Now().Add(-time.Minute).Unix())
	_, mac, _ := strings.Cut(key, ".")
	forged := fmt.Sprint(time.Now().Add(time.Hour).Unix()) + "." + mac
	if res, _ := get(t, server, "/private/music/song.mp3?"+sonosPlayParam+"="+forged); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a key given a later time answered %d", res.StatusCode)
	}
}

func TestSonosLinkStopsWhenSonosIsOff(t *testing.T) {
	skipWithout(t, features.Sonos)
	server := newServer(t, nil)
	path := server.write(t, "private/music/song.mp3", "song")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	key := sonosKey(server.shareSecret(server.settings()), "/private/music/song.mp3", info, time.Now().Add(time.Hour).Unix())
	if res, _ := get(t, server, "/private/music/song.mp3?"+sonosPlayParam+"="+key); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a link answered %d while http.sonos is off", res.StatusCode)
	}
}

func TestSonosRefusals(t *testing.T) {
	sp := newSpeaker(t)
	server := sonosServer(t, sp, func(cfg *httpConfig) {
		reader := fullUser("kim", "pw")
		reader.AllowUserFileRetrieve = new(false)
		cfg.Users = append(cfg.Users, reader)
	})
	server.write(t, "music/song.mp3", "song")
	server.write(t, "music/notes.txt", "text")
	session := login(t, server, "/music/", "john", "doe")
	play := sonosRequest{Group: speakerID}

	tests := []struct {
		name    string
		session *http.Cookie
		method  string
		path    string
		body    any
		header  http.Header
		want    int
	}{
		{"no session", nil, http.MethodGet, "/music/song.mp3?go-fs=sonos&sonos=groups", nil, nil, http.StatusForbidden},
		{"may not read", login(t, server, "/music/", "kim", "pw"), http.MethodPost, "/music/song.mp3?go-fs=sonos&sonos=play", play, nil, http.StatusForbidden},
		{"another site", session, http.MethodPost, "/music/song.mp3?go-fs=sonos&sonos=play", play,
			http.Header{"Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden},
		{"no audio", session, http.MethodPost, "/music/notes.txt?go-fs=sonos&sonos=play", play, nil, http.StatusUnsupportedMediaType},
		{"no file", session, http.MethodPost, "/music/gone.mp3?go-fs=sonos&sonos=play", play, nil, http.StatusNotFound},
		{"play by GET", session, http.MethodGet, "/music/song.mp3?go-fs=sonos&sonos=play", nil, nil, http.StatusMethodNotAllowed},
		{"unknown", session, http.MethodGet, "/music/song.mp3?go-fs=sonos&sonos=reboot", nil, nil, http.StatusBadRequest},
		{"another speaker", session, http.MethodPost, "/music/song.mp3?go-fs=sonos&sonos=play", sonosRequest{Group: "RINCON_ELSEWHERE"}, nil, http.StatusNotFound},
		{"no volume", session, http.MethodPost, "/music/song.mp3?go-fs=sonos&sonos=volume", play, nil, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, body := sonosAsk(t, server, tt.session, tt.method, tt.path, tt.body, tt.header)
			if res.StatusCode != tt.want {
				t.Errorf("answered %d, want %d: %s", res.StatusCode, tt.want, body)
			}
		})
	}
	if calls, _ := sp.taken(); strings.Contains(strings.Join(calls, ","), "SetAVTransportURI") {
		t.Errorf("a refused request reached the speaker: %v", calls)
	}

	off := newServer(t, nil)
	off.write(t, "music/song.mp3", "song")
	session = login(t, off, "/music/", "john", "doe")
	if res, _ := sonosAsk(t, off, session, http.MethodGet, "/music/song.mp3?go-fs=sonos&sonos=groups", nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("groups answered %d while http.sonos is off", res.StatusCode)
	}
}

func TestSonosLinkNeedsAnAddressTheSpeakerReaches(t *testing.T) {
	server := sonosServer(t, nil, nil)
	path := server.write(t, "music/song.mp3", "song")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target := server.root.Resolve("/", "/music/song.mp3")
	away := sonos.Player{Room: "Kitchen", Host: "192.0.2.10:1400"}

	// the server listens on 127.0.0.1, which a speaker elsewhere does not reach
	if _, err := server.sonosLink(server.settings(), target, info, away); err == nil || !strings.Contains(err.Error(), "http.address") {
		t.Errorf("a link on the loopback address was handed out: %v", err)
	}

	set := *server.settings()
	set.cfg.ShareInternalAddress = "https://files.example.com/base"
	link, err := server.sonosLink(&set, target, info, away)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://files.example.com/base/music/song.mp3?"+sonosPlayParam+"=") {
		t.Errorf("the link is %s", link)
	}
}

func TestSonosPlayableNames(t *testing.T) {
	for name, want := range map[string]bool{
		"a.mp3": true, "B.FLAC": true, "c.m4a": true, "d.wav": true, "e.ogg": true,
		"f.opus": false, "g.txt": false, "mp3": false, filepath.Join("x.mp3", "y"): false,
	} {
		if got := sonosPlayable(name); got != want {
			t.Errorf("sonosPlayable(%q) = %v", name, got)
		}
	}
}
