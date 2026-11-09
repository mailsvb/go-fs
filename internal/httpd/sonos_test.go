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
	id     string
	room   string
	// journal, where set, has the calls of several speakers in the order
	// they came, as room:action.
	journal *journal

	mu     sync.Mutex
	calls  []string
	uri    string
	refuse map[string]bool
	// transport, at and length are what it says it does: PLAYING once told
	// to play, and how far into how long a file, H:MM:SS
	transport, at, length string
}

// playing has the speaker say it plays at of length.
func (sp *speaker) playing(transport, at, length string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.transport, sp.at, sp.length = transport, at, length
}

// told has the speaker say it plays uri, as when the Sonos app told it to.
func (sp *speaker) told(uri string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.uri = uri
}

// journal is the calls of several speakers, in order.
type journal struct {
	mu    sync.Mutex
	calls []string
}

func (j *journal) add(call string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, call)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.calls...)
}

func newSpeaker(t *testing.T) *speaker {
	return newRoom(t, speakerID, "Kitchen", nil)
}

// newRoom is a speaker of the id and the room, whose calls also go to j.
func newRoom(t *testing.T, id, room string, j *journal) *speaker {
	t.Helper()
	sp := &speaker{id: id, room: room, journal: j, refuse: map[string]bool{}}
	sp.server = httptest.NewServer(http.HandlerFunc(sp.serve))
	t.Cleanup(sp.server.Close)
	return sp
}

// fail has the speaker answer action with a fault.
func (sp *speaker) fail(action string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.refuse[action] = true
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
			`<modelName>Sonos One</modelName><UDN>uuid:%s</UDN><roomName>%s</roomName></device></root>`, sp.id, sp.room)
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
	refused := sp.refuse[action]
	if !refused {
		switch action {
		case "SetAVTransportURI":
			sp.uri, sp.transport, sp.at, sp.length = env.URI, "STOPPED", "0:00:00", "0:03:00"
		case "Play":
			sp.transport = "PLAYING"
		case "Pause":
			sp.transport = "PAUSED_PLAYBACK"
		case "Stop":
			sp.transport = "STOPPED"
		}
	}
	uri, transport, at, length := sp.uri, sp.transport, sp.at, sp.length
	sp.mu.Unlock()
	if sp.journal != nil {
		sp.journal.add(sp.room + ":" + action)
	}

	out := map[string]string{}
	if refused {
		action = "refused"
	}
	switch action {
	case "refused", "GetZoneGroupState":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail>`+
			`<UPnPError><errorCode>401</errorCode></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
		return
	case "GetTransportInfo":
		if transport == "" {
			transport = "STOPPED"
		}
		out["CurrentTransportState"] = transport
	case "GetPositionInfo":
		out["TrackURI"] = uri
		out["RelTime"] = at
		out["TrackDuration"] = length
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
	// a sweep would reach the real speakers of the network the test runs on
	client.Subnets = nil
	server.sonos.client = client
	server.sonos.wait = 20 * time.Millisecond
	server.sonos.pollEvery = 10 * time.Millisecond
	server.sonos.pollNear = 5 * time.Millisecond
	server.sonos.startLimit = 300 * time.Millisecond
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
	if strings.Join(commands(calls), ",") != "SetAVTransportURI,Play" || calls[0] != "GetZoneGroupState" {
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
	calls = commands(calls)
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

// threeRooms is a server that finds Kitchen, Office and Porch, each a group
// of its own, whose calls all go to one journal.
func threeRooms(t *testing.T) (*testServer, *journal, []*speaker) {
	t.Helper()
	j := &journal{}
	rooms := []*speaker{
		newRoom(t, "RINCON_A01400", "Kitchen", j),
		newRoom(t, "RINCON_B01400", "Office", j),
		newRoom(t, "RINCON_C01400", "Porch", j),
	}
	server := sonosServer(t, nil, func(cfg *httpConfig) {
		for _, sp := range rooms {
			cfg.SonosSpeakers = append(cfg.SonosSpeakers, sp.host())
		}
	})
	return server, j, rooms
}

// sonosGroupsOf is the groups the dialog is told of.
func sonosGroupsOf(t *testing.T, server *testServer, session *http.Cookie, file string) []sonosGroupView {
	t.Helper()
	res, body := sonosAsk(t, server, session, http.MethodGet, file+"?go-fs=sonos&sonos=groups", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("groups answered %d: %s", res.StatusCode, body)
	}
	var view struct {
		Groups []sonosGroupView `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	return view.Groups
}

func namesOf(groups []sonosGroupView) string {
	var names []string
	for _, g := range groups {
		names = append(names, g.Name+"="+strings.Join(g.Rooms, "/"))
	}
	return strings.Join(names, ", ")
}

// commands are the calls that tell the speaker something, without those
// that ask what it does, which the session that follows a play makes at any
// time.
func commands(calls []string) []string {
	var told []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "Get") {
			told = append(told, c)
		}
	}
	return told
}

// count is how many of the calls are call.
func count(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestSonosPlaysOnSeveralRoomsTogether(t *testing.T) {
	server, j, rooms := threeRooms(t)
	server.write(t, "music/song.mp3", "song")
	session := login(t, server, "/music/", "john", "doe")
	file := "/music/song.mp3"

	if got := namesOf(sonosGroupsOf(t, server, session, file)); got != "Kitchen=Kitchen, Office=Office, Porch=Porch" {
		t.Fatalf("groups %s", got)
	}
	searched := count(j.all(), "Kitchen:GetZoneGroupState")

	res, body := sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos=play",
		sonosRequest{Group: "RINCON_A01400", Join: []string{"RINCON_B01400", "RINCON_C01400", "RINCON_A01400"}}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play answered %d: %s", res.StatusCode, body)
	}
	var answer struct {
		Group  string   `json:"group"`
		Name   string   `json:"name"`
		Rooms  []string `json:"rooms"`
		Failed []string `json:"failed"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Group != "RINCON_A01400" || answer.Name != "Kitchen + 2" ||
		strings.Join(answer.Rooms, ",") != "Kitchen,Office,Porch" || answer.Failed != nil {
		t.Errorf("play answered %s", body)
	}

	// the others join Kitchen before it plays, so they start along with it
	var played []string
	for _, call := range j.all() {
		if strings.HasSuffix(call, ":SetAVTransportURI") || strings.HasSuffix(call, ":Play") {
			played = append(played, call)
		}
	}
	if len(played) != 4 || played[2] != "Kitchen:SetAVTransportURI" || played[3] != "Kitchen:Play" {
		t.Errorf("the speakers were called %v", played)
	}
	for _, sp := range rooms[1:] {
		if _, uri := sp.taken(); uri != "x-rincon:RINCON_A01400" {
			t.Errorf("%s was told %s", sp.room, uri)
		}
	}
	if _, uri := rooms[0].taken(); !strings.Contains(uri, sonosPlayParam+"=") {
		t.Errorf("Kitchen was told %s", uri)
	}

	// the dialog is told the new grouping at once, with no search
	if got := namesOf(sonosGroupsOf(t, server, session, file)); got != "Kitchen + 2=Kitchen/Office/Porch" {
		t.Errorf("groups after joining %s", got)
	}

	res, body = sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos=ungroup", sonosRequest{Group: "RINCON_A01400"}, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("ungroup answered %d: %s", res.StatusCode, body)
	}
	calls := j.all()
	if count(calls, "Office:BecomeCoordinatorOfStandaloneGroup") != 1 || count(calls, "Porch:BecomeCoordinatorOfStandaloneGroup") != 1 ||
		count(calls, "Kitchen:BecomeCoordinatorOfStandaloneGroup") != 0 {
		t.Errorf("ungroup called %v", calls)
	}
	if got := namesOf(sonosGroupsOf(t, server, session, file)); got != "Kitchen=Kitchen, Office=Office, Porch=Porch" {
		t.Errorf("groups after ungrouping %s", got)
	}
	if got := count(j.all(), "Kitchen:GetZoneGroupState"); got != searched {
		t.Errorf("the speakers were searched again %d times", got-searched)
	}
	if server.logs.find("http sonos ungroup") == nil {
		t.Error("the ungroup was not logged")
	}
}

func TestSonosPlaysWhenARoomDoesNotJoin(t *testing.T) {
	server, j, rooms := threeRooms(t)
	server.write(t, "music/song.mp3", "song")
	session := login(t, server, "/music/", "john", "doe")
	file := "/music/song.mp3"
	rooms[1].fail("SetAVTransportURI")

	res, body := sonosAsk(t, server, session, http.MethodPost, file+"?go-fs=sonos&sonos=play",
		sonosRequest{Group: "RINCON_A01400", Join: []string{"RINCON_B01400", "RINCON_C01400"}}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play answered %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"name":"Kitchen + 1"`) || !strings.Contains(body, `"failed":["Office: `) {
		t.Errorf("play answered %s", body)
	}
	if count(j.all(), "Kitchen:Play") != 1 {
		t.Errorf("Kitchen did not play: %v", j.all())
	}
	if got := namesOf(sonosGroupsOf(t, server, session, file)); got != "Kitchen + 1=Kitchen/Porch, Office=Office" {
		t.Errorf("groups %s", got)
	}
	if server.logs.find("http sonos speaker did not join") == nil {
		t.Error("the room that did not join was not logged")
	}
}

// sessionAsk asks about a session, or tells it something, and reads what
// it does.
func sessionAsk(t *testing.T, server *testServer, session *http.Cookie, op string, body any) (int, sonosSessionView) {
	t.Helper()
	method, path := http.MethodPost, "/music/?go-fs=sonos&sonos="+op
	if op == "session" {
		method, path, body = http.MethodGet, path+"&id="+body.(string), nil
	}
	res, text := sonosAsk(t, server, session, method, path, body, nil)
	var view sonosSessionView
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(text), &view); err != nil {
			t.Fatalf("%s answered %s", op, text)
		}
	}
	return res.StatusCode, view
}

// eventually waits for what is wanted to come true.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited in vain for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// playQueue has the speaker play the files of /music/ from first, and
// returns the session.
func playQueue(t *testing.T, server *testServer, session *http.Cookie, first string, files ...string) sonosSessionView {
	t.Helper()
	res, body := sonosAsk(t, server, session, http.MethodPost, "/music/"+first+"?go-fs=sonos&sonos=play",
		sonosRequest{Group: speakerID, Files: files}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("play answered %d: %s", res.StatusCode, body)
	}
	var answer struct {
		Session sonosSessionView `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	return answer.Session
}

// plays reports whether the speaker was last told to play the file.
func (sp *speaker) plays(name string) bool {
	_, uri := sp.taken()
	u, err := url.Parse(uri)
	return err == nil && u.Path == "/music/"+name && u.Query().Has(sonosPlayParam)
}

// ends has the file the speaker plays come to its end.
func (sp *speaker) ends(t *testing.T, server *testServer, session *http.Cookie, id string) {
	t.Helper()
	sp.playing("PLAYING", "0:02:59", "0:03:00")
	eventually(t, "the session to see the end near", func() bool {
		_, view := sessionAsk(t, server, session, "session", id)
		return view.Position == 179
	})
	sp.playing("STOPPED", "0:00:00", "0:03:00")
}

func sonosQueueServer(t *testing.T) (*testServer, *speaker, *http.Cookie) {
	t.Helper()
	sp := newSpeaker(t)
	server := sonosServer(t, sp, func(cfg *httpConfig) {
		cfg.Users = append(cfg.Users, fullUser("kim", "pw"))
	})
	for _, name := range []string{"a.mp3", "b.mp3", "c.mp3", "notes.txt"} {
		server.write(t, "music/"+name, name)
	}
	return server, sp, login(t, server, "/music/", "john", "doe")
}

func TestSonosSessionPlaysTheQueueToItsEnd(t *testing.T) {
	server, sp, session := sonosQueueServer(t)
	// the text is no audio and is left out; the file that is gone is passed
	// over when its turn comes
	view := playQueue(t, server, session, "a.mp3", "a.mp3", "notes.txt", "gone.mp3", "b.mp3")
	if view.Files != 3 || view.Index != 0 || view.Name != "a.mp3" || view.State != sessionPlaying || view.Folder != "/music/" {
		t.Fatalf("session %+v", view)
	}
	if !sp.plays("a.mp3") {
		t.Fatal("the speaker does not play the first file")
	}
	sp.ends(t, server, session, view.ID)
	eventually(t, "the next file", func() bool { return sp.plays("b.mp3") })
	if _, view = sessionAsk(t, server, session, "session", view.ID); view.Index != 2 || view.Name != "b.mp3" {
		t.Errorf("session after the first file %+v", view)
	}
	sp.ends(t, server, session, view.ID)
	eventually(t, "the session to end", func() bool {
		_, view = sessionAsk(t, server, session, "session", view.ID)
		return view.State == sessionDone
	})
	if view.Message != "Played 2 of 3 files on Kitchen." {
		t.Errorf("the session ended with %q", view.Message)
	}
	calls, _ := sp.taken()
	time.Sleep(50 * time.Millisecond)
	if after, _ := sp.taken(); len(after) != len(calls) {
		t.Errorf("the speaker was called after the session ended: %v", after[len(calls):])
	}
	if server.logs.find("http sonos session ended") == nil {
		t.Error("the end of the session was not logged")
	}
}

// polled waits until the session asked the speaker what it does twice more.
func (sp *speaker) polled(t *testing.T) {
	t.Helper()
	count := func() int {
		calls, _ := sp.taken()
		n := 0
		for _, c := range calls {
			if c == "GetPositionInfo" {
				n++
			}
		}
		return n
	}
	before := count()
	eventually(t, "the session to ask the speaker", func() bool { return count() >= before+2 })
}

func TestSonosSessionTakesAFileBackAtItsStartForItsEnd(t *testing.T) {
	server, sp, session := sonosQueueServer(t)
	view := playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3")
	sp.playing("PLAYING", "0:02:58", "0:03:00")
	sp.polled(t)
	// the file ended between asking what the group does and how far it is
	sp.playing("PLAYING", "0:00:00", "0:03:00")
	sp.polled(t)
	sp.playing("STOPPED", "0:00:00", "0:03:00")
	eventually(t, "the next file", func() bool { return sp.plays("b.mp3") })
	if _, view = sessionAsk(t, server, session, "session", view.ID); view.State != sessionPlaying {
		t.Errorf("session %+v", view)
	}
}

func TestSonosSessionEndsWhenTheGroupIsStoppedOrTakenOver(t *testing.T) {
	server, sp, session := sonosQueueServer(t)

	view := playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3")
	sp.playing("PLAYING", "0:00:30", "0:03:00")
	eventually(t, "the session to see the file play", func() bool {
		_, view = sessionAsk(t, server, session, "session", view.ID)
		return view.Position == 30
	})
	// stopped in the Sonos app, well before the end
	sp.playing("STOPPED", "0:00:30", "0:03:00")
	eventually(t, "the session to end", func() bool {
		_, view = sessionAsk(t, server, session, "session", view.ID)
		return view.State == sessionStopped
	})
	if view.Message != "Stopped on Kitchen." || sp.plays("b.mp3") {
		t.Errorf("session %+v, the next file played: %v", view, sp.plays("b.mp3"))
	}

	view = playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3")
	sp.told("x-sonos-spotify:spotify%3atrack%3a1")
	eventually(t, "the session to end", func() bool {
		_, view = sessionAsk(t, server, session, "session", view.ID)
		return view.State == sessionStopped
	})
	if view.Message != "Something else plays on Kitchen now." {
		t.Errorf("session %+v", view)
	}
	if calls, _ := sp.taken(); strings.Contains(strings.Join(calls[len(calls)-1:], ","), "Stop") {
		t.Error("the session stopped what plays instead of it")
	}
}

func TestSonosSessionSkipsAndEnds(t *testing.T) {
	server, sp, session := sonosQueueServer(t)
	view := playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3", "c.mp3")

	// someone else's session is not there to them
	other := login(t, server, "/music/", "kim", "pw")
	for _, op := range []string{"session", "skip", "end"} {
		var body any = sonosRequest{Session: view.ID, Index: new(1)}
		if op == "session" {
			body = view.ID
		}
		if code, _ := sessionAsk(t, server, other, op, body); code != http.StatusNotFound {
			t.Errorf("%s of another account's session answered %d", op, code)
		}
	}
	if strings.Contains(bodyOf(t, withSession(t, server, http.MethodGet, "/music/", other)), "data-session=") {
		t.Error("the listing of another account shows the session")
	}
	if page := bodyOf(t, withSession(t, server, http.MethodGet, "/music/", session)); !strings.Contains(page, view.ID) {
		t.Error("the listing does not hand the session to its bar")
	}

	if code, _ := sessionAsk(t, server, session, "skip", sonosRequest{Session: view.ID, Index: new(7)}); code != http.StatusConflict {
		t.Errorf("a skip past the queue answered %d", code)
	}
	if code, _ := sessionAsk(t, server, session, "skip", sonosRequest{Session: view.ID, Index: new(2)}); code != http.StatusOK {
		t.Fatalf("skip answered %d", code)
	}
	eventually(t, "the file skipped to", func() bool { return sp.plays("c.mp3") })
	if _, view = sessionAsk(t, server, session, "session", view.ID); view.Index != 2 {
		t.Errorf("session after skipping %+v", view)
	}

	code, view := sessionAsk(t, server, session, "end", sonosRequest{Session: view.ID})
	if code != http.StatusOK || view.State != sessionStopped {
		t.Fatalf("end answered %d: %+v", code, view)
	}
	calls, _ := sp.taken()
	if calls[len(calls)-1] != "Stop" {
		t.Errorf("the group was not stopped: %v", calls)
	}
	// a session that ended is not ended again, nor is what plays then
	if code, _ := sessionAsk(t, server, session, "end", sonosRequest{Session: view.ID}); code != http.StatusOK {
		t.Errorf("a second end answered %d", code)
	}
	if after, _ := sp.taken(); len(after) != len(calls) {
		t.Errorf("a second end called the speaker: %v", after[len(calls):])
	}
	if strings.Contains(bodyOf(t, withSession(t, server, http.MethodGet, "/music/", session)), "data-session=") {
		t.Error("the listing hands a session that ended to its bar")
	}
}

func TestSonosPlayEndsTheSessionOnItsSpeakers(t *testing.T) {
	server, sp, session := sonosQueueServer(t)
	first := playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3")
	// the row menu plays the one file
	second := playQueue(t, server, session, "c.mp3")
	if second.Files != 1 {
		t.Errorf("the second session %+v", second)
	}
	if _, view := sessionAsk(t, server, session, "session", first.ID); view.State != sessionStopped {
		t.Errorf("the first session goes on: %+v", view)
	}
	sp.ends(t, server, session, second.ID)
	eventually(t, "the second session to end", func() bool {
		_, view := sessionAsk(t, server, session, "session", second.ID)
		return view.State == sessionDone
	})
	if !sp.plays("c.mp3") {
		t.Error("the first session played on")
	}
	// the dialog's Stop ends the session too, rather than it taking the stop
	// for the end of the file
	third := playQueue(t, server, session, "a.mp3", "a.mp3", "b.mp3")
	if res, body := sonosAsk(t, server, session, http.MethodPost, "/music/a.mp3?go-fs=sonos&sonos=stop", sonosRequest{Group: speakerID}, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop answered %d: %s", res.StatusCode, body)
	}
	if _, view := sessionAsk(t, server, session, "session", third.ID); view.State != sessionStopped {
		t.Errorf("the session goes on after Stop: %+v", view)
	}
	time.Sleep(50 * time.Millisecond)
	if sp.plays("b.mp3") {
		t.Error("the session played the next file after Stop")
	}
}

func TestSonosQueueRefusals(t *testing.T) {
	server, _, session := sonosQueueServer(t)
	for name, files := range map[string][]string{
		"without the file": {"b.mp3"},
		"only elsewhere":   {"../a.mp3"},
	} {
		res, body := sonosAsk(t, server, session, http.MethodPost, "/music/a.mp3?go-fs=sonos&sonos=play",
			sonosRequest{Group: speakerID, Files: files}, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("a queue %s answered %d: %s", name, res.StatusCode, body)
		}
	}
	many := make([]string, maxSonosFiles+1)
	for i := range many {
		many[i] = "a.mp3"
	}
	if res, _ := sonosAsk(t, server, session, http.MethodPost, "/music/a.mp3?go-fs=sonos&sonos=play",
		sonosRequest{Group: speakerID, Files: many}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("a queue too long answered %d", res.StatusCode)
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
		{"join another speaker", session, http.MethodPost, "/music/song.mp3?go-fs=sonos&sonos=play",
			sonosRequest{Group: speakerID, Join: []string{"RINCON_ELSEWHERE"}}, nil, http.StatusNotFound},
		{"ungroup by GET", session, http.MethodGet, "/music/song.mp3?go-fs=sonos&sonos=ungroup", nil, nil, http.StatusMethodNotAllowed},
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
