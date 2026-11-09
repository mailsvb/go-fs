package sonos

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// call is one SOAP call a fake speaker took.
type call struct {
	Action string
	Args   map[string]string
}

// fakeSpeaker answers as a speaker does: its description and the actions a
// test sets up, recording every call.
type fakeSpeaker struct {
	t      *testing.T
	server *httptest.Server
	uuid   string
	room   string

	mu      sync.Mutex
	calls   []call
	answers map[string]func(args map[string]string) (map[string]string, int)
}

func newFakeSpeaker(t *testing.T, uuid, room string) *fakeSpeaker {
	t.Helper()
	f := &fakeSpeaker{t: t, uuid: uuid, room: room,
		answers: map[string]func(map[string]string) (map[string]string, int){}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSpeaker) host() string { return f.server.Listener.Addr().String() }

func (f *fakeSpeaker) port() string {
	_, port, _ := net.SplitHostPort(f.host())
	return port
}

func (f *fakeSpeaker) answer(action string, fn func(map[string]string) (map[string]string, int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[action] = fn
}

func (f *fakeSpeaker) taken() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeSpeaker) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/xml/device_description.xml" {
		fmt.Fprintf(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0">`+
			`<device><deviceType>%s</deviceType><modelName>Sonos One</modelName>`+
			`<UDN>uuid:%s</UDN><roomName>%s</roomName></device></root>`, zonePlayer, f.uuid, f.room)
		return
	}
	soapAction := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
	urn, action, ok := strings.Cut(soapAction, "#")
	if r.Method != http.MethodPost || !ok {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var env struct {
		Body struct {
			Action struct {
				XMLName xml.Name
				Args    []struct {
					XMLName xml.Name
					Value   string `xml:",chardata"`
				} `xml:",any"`
			} `xml:",any"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &env); err != nil {
		f.t.Errorf("the %s call is no envelope: %v\n%s", action, err, body)
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	if env.Body.Action.XMLName.Local != action || env.Body.Action.XMLName.Space != urn {
		f.t.Errorf("SOAPACTION %s names another action than the body, %v", soapAction, env.Body.Action.XMLName)
	}
	args := map[string]string{}
	for _, a := range env.Body.Action.Args {
		args[a.XMLName.Local] = a.Value
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{Action: action, Args: args})
	fn := f.answers[action]
	f.mu.Unlock()

	out, fault := map[string]string{}, 0
	if fn != nil {
		out, fault = fn(args)
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	if fault != 0 {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>`+
			`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`+
			`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode></UPnPError>`+
			`</detail></s:Fault></s:Body></s:Envelope>`, fault)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="%s">`, action, urn)
	for name, value := range out {
		fmt.Fprintf(&b, "<%s>%s</%s>", name, html.EscapeString(value), name)
	}
	fmt.Fprintf(&b, "</u:%sResponse></s:Body></s:Envelope>", action)
	_, _ = io.WriteString(w, b.String())
}

// testClient is a client whose searches go to ssdp and that believes answers
// naming port.
func testClient(ssdp, port string) *Client {
	c := New()
	c.Search = ssdp
	c.port = port
	return c
}

// fakeSSDP answers a search for speakers on the loopback interface with
// each of the locations given, in turn.
func fakeSSDP(t *testing.T, locations ...string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			search := string(buf[:n])
			if !strings.HasPrefix(search, "M-SEARCH * HTTP/1.1\r\n") || !strings.Contains(search, "ST: "+zonePlayer+"\r\n") {
				continue
			}
			for _, location := range locations {
				answer := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age = 1800\r\nEXT:\r\n" +
					"LOCATION: " + location + "\r\nSERVER: Linux UPnP/1.0 Sonos/80.1\r\n" +
					"ST: " + zonePlayer + "\r\nUSN: uuid:x::" + zonePlayer + "\r\n\r\n"
				_, _ = conn.WriteTo([]byte(answer), from)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func TestDiscoverDescribesTheSpeakersThatAnswer(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_000E58A0B1C201400", "Kitchen")
	ssdp := fakeSSDP(t,
		"http://"+speaker.host()+"/xml/device_description.xml",
		// the same speaker again, which counts once
		"http://"+speaker.host()+"/xml/device_description.xml",
		// a description elsewhere than where the answer came from, which
		// is not fetched
		"http://10.255.255.1:"+speaker.port()+"/xml/device_description.xml")
	c := testClient(ssdp, speaker.port())
	players, err := c.Discover(context.Background(), 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 1 {
		t.Fatalf("found %+v, want the kitchen alone", players)
	}
	want := Player{UUID: "RINCON_000E58A0B1C201400", Room: "Kitchen", Model: "Sonos One", Host: speaker.host()}
	if players[0] != want {
		t.Errorf("found %+v, want %+v", players[0], want)
	}
}

func TestAnswerLocationBelievesOnlyTheSpeakerItself(t *testing.T) {
	from := &net.UDPAddr{IP: net.ParseIP("192.168.1.20"), Port: 1900}
	answer := func(st, location string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nST: " + st + "\r\nLOCATION: " + location + "\r\n\r\n")
	}
	tests := []struct {
		name  string
		reply []byte
		ok    bool
	}{
		{"speaker", answer(zonePlayer, "http://192.168.1.20:1400/xml/device_description.xml"), true},
		{"other host", answer(zonePlayer, "http://192.168.1.21:1400/xml/device_description.xml"), false},
		{"other port", answer(zonePlayer, "http://192.168.1.20:80/xml/device_description.xml"), false},
		{"https", answer(zonePlayer, "https://192.168.1.20:1400/xml/device_description.xml"), false},
		{"name", answer(zonePlayer, "http://sonos.local:1400/xml/device_description.xml"), false},
		{"other device", answer("upnp:rootdevice", "http://192.168.1.20:1400/xml/device_description.xml"), false},
		{"garbage", []byte("NOTIFY * HTTP/1.1\r\n\r\n"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := answerLocation(tt.reply, from, Port); ok != tt.ok {
				t.Errorf("believed %v, want %v", ok, tt.ok)
			}
		})
	}
}

func TestProbeRefusesWhatIsNoSpeaker(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<root><device><deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType></device></root>`)
	}))
	defer other.Close()
	if _, err := New().Probe(context.Background(), other.Listener.Addr().String()); err == nil {
		t.Error("a media server was taken for a speaker")
	}
	speaker := newFakeSpeaker(t, "RINCON_A01400", "Office")
	p, err := New().Probe(context.Background(), speaker.host())
	if err != nil {
		t.Fatal(err)
	}
	if p.Room != "Office" || p.UUID != "RINCON_A01400" || p.Host != speaker.host() {
		t.Errorf("probed %+v", p)
	}
}

func TestPlaySetsTheFileAndPlaysIt(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_A01400", "Office")
	uri := "http://192.168.1.5:8080/music/A&B%20%3C1%3E.mp3?go-fs-play=1.ab"
	track := Track{Title: `Rock & "Roll" <Live>`, Artist: "Ünïcödé", Album: "A&B", MIME: "audio/mpeg"}
	if err := New().Play(context.Background(), speaker.host(), uri, track); err != nil {
		t.Fatal(err)
	}
	calls := speaker.taken()
	if len(calls) != 2 || calls[0].Action != "SetAVTransportURI" || calls[1].Action != "Play" {
		t.Fatalf("calls %+v, want SetAVTransportURI and Play", calls)
	}
	set := calls[0].Args
	if set["InstanceID"] != "0" || set["CurrentURI"] != uri {
		t.Errorf("SetAVTransportURI got %v", set)
	}
	var meta struct {
		Item struct {
			Res struct {
				ProtocolInfo string `xml:"protocolInfo,attr"`
				URI          string `xml:",chardata"`
			} `xml:"res"`
			Title   string `xml:"http://purl.org/dc/elements/1.1/ title"`
			Creator string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			Album   string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ album"`
			Class   string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ class"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(set["CurrentURIMetaData"]), &meta); err != nil {
		t.Fatalf("the metadata does not read: %v\n%s", err, set["CurrentURIMetaData"])
	}
	if meta.Item.Res.URI != uri || meta.Item.Res.ProtocolInfo != "http-get:*:audio/mpeg:*" ||
		meta.Item.Title != track.Title || meta.Item.Creator != track.Artist || meta.Item.Album != track.Album ||
		meta.Item.Class != "object.item.audioItem.musicTrack" {
		t.Errorf("metadata %+v", meta.Item)
	}
	if calls[1].Args["Speed"] != "1" {
		t.Errorf("Play got %v", calls[1].Args)
	}
}

func TestFaultsAreErrors(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_B01400", "Bath")
	speaker.answer("SetAVTransportURI", func(map[string]string) (map[string]string, int) { return nil, 800 })
	err := New().Play(context.Background(), speaker.host(), "http://x/a.mp3", Track{MIME: "audio/mpeg"})
	if !NotCoordinator(err) {
		t.Fatalf("got %v, want a fault 800", err)
	}
	if !strings.Contains(err.Error(), "not the coordinator") {
		t.Errorf("the error says %q", err)
	}
	if calls := speaker.taken(); len(calls) != 1 {
		t.Errorf("played on after the fault: %+v", calls)
	}
	speaker.answer("Pause", func(map[string]string) (map[string]string, int) { return nil, 701 })
	if err := New().Pause(context.Background(), speaker.host()); err == nil || NotCoordinator(err) {
		t.Errorf("pause got %v, want a fault 701", err)
	}
}

const zoneState = `<ZoneGroupState><ZoneGroups>` +
	`<ZoneGroup Coordinator="RINCON_B01400" ID="RINCON_B01400:12">` +
	`<ZoneGroupMember UUID="RINCON_A01400" Location="http://192.168.1.20:1400/xml/device_description.xml" ZoneName="Kitchen"/>` +
	`<ZoneGroupMember UUID="RINCON_B01400" Location="http://192.168.1.21:1400/xml/device_description.xml" ZoneName="Living Room"/>` +
	`<ZoneGroupMember UUID="RINCON_C01400" Location="http://192.168.1.22:1400/xml/device_description.xml" ZoneName="Living Room" Invisible="1"/>` +
	`</ZoneGroup>` +
	`<ZoneGroup Coordinator="RINCON_D01400" ID="RINCON_D01400:3">` +
	`<ZoneGroupMember UUID="RINCON_D01400" Location="http://192.168.1.23:1400/xml/device_description.xml" ZoneName="Bedroom"><Satellite UUID="RINCON_E01400" Location="http://192.168.1.24:1400/xml/device_description.xml" ZoneName="Bedroom" Invisible="1"/></ZoneGroupMember>` +
	`</ZoneGroup>` +
	`</ZoneGroups><VanishedDevices/></ZoneGroupState>`

func TestParseZoneGroups(t *testing.T) {
	old := strings.TrimSuffix(strings.TrimPrefix(zoneState, "<ZoneGroupState>"), "<VanishedDevices/></ZoneGroupState>")
	for name, state := range map[string]string{"current": zoneState, "old": old} {
		t.Run(name, func(t *testing.T) {
			groups, placed, err := parseZoneGroups(state)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(placed, ","); got != "RINCON_A01400,RINCON_B01400,RINCON_C01400,RINCON_D01400,RINCON_E01400" {
				t.Errorf("placed %s", got)
			}
			if len(groups) != 2 {
				t.Fatalf("groups %+v", groups)
			}
			living := groups[0]
			if living.ID != "RINCON_B01400" || living.Coordinator.Room != "Living Room" ||
				living.Coordinator.Host != "192.168.1.21:1400" || len(living.Members) != 2 ||
				living.Members[0].UUID != "RINCON_B01400" || living.Members[1].Room != "Kitchen" {
				t.Errorf("living room %+v", living)
			}
			if living.Name() != "Living Room + 1" || groups[1].Name() != "Bedroom" {
				t.Errorf("names %q, %q", living.Name(), groups[1].Name())
			}
		})
	}
}

func TestGroupsAsksOncePerHousehold(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_A01400", "Kitchen")
	port := speaker.port()
	state := strings.ReplaceAll(zoneState, "192.168.1.20:1400", speaker.host())
	state = strings.ReplaceAll(state, ":1400/", ":"+port+"/")
	speaker.answer("GetZoneGroupState", func(map[string]string) (map[string]string, int) {
		return map[string]string{"ZoneGroupState": state}, 0
	})
	c := testClient(ssdpGroup, port)
	players := []Player{
		{UUID: "RINCON_A01400", Room: "Kitchen", Model: "Sonos One", Host: speaker.host()},
		// in the household of the kitchen, so not asked
		{UUID: "RINCON_D01400", Room: "Bedroom", Host: "192.0.2.1:" + port},
	}
	groups, err := c.Groups(context.Background(), players)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Coordinator.Room != "Bedroom" || groups[1].Name() != "Living Room + 1" {
		t.Fatalf("groups %+v", groups)
	}
	if groups[1].Members[1].Model != "Sonos One" {
		t.Errorf("the kitchen lost its model: %+v", groups[1].Members[1])
	}
	if calls := speaker.taken(); len(calls) != 1 {
		t.Errorf("asked %d times", len(calls))
	}
}

// A home theatre's surrounds and subwoofer answer a search as any speaker
// does; they are part of its room, not rooms of their own.
func TestGroupsLeavesOutTheSatellitesOfARoom(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_E01400", "Sub Mini")
	port := speaker.port()
	state := strings.ReplaceAll(zoneState, "192.168.1.24:1400", speaker.host())
	state = strings.ReplaceAll(state, ":1400/", ":"+port+"/")
	speaker.answer("GetZoneGroupState", func(map[string]string) (map[string]string, int) {
		return map[string]string{"ZoneGroupState": state}, 0
	})
	players := []Player{
		// what a search finds: the subwoofer, and the stereo pair's
		// second speaker, never the rooms themselves
		{UUID: "RINCON_E01400", Room: "Sub Mini", Host: speaker.host()},
		{UUID: "RINCON_C01400", Room: "Living Room", Host: "192.0.2.3:" + port},
	}
	groups, err := testClient(ssdpGroup, port).Groups(context.Background(), players)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name())
	}
	if got := strings.Join(names, "|"); got != "Bedroom|Living Room + 1" {
		t.Errorf("groups %s, want the bedroom and the living room alone", got)
	}
}

func TestGroupsTakesASpeakerThatDoesNotAnswerAsItsOwnGroup(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_A01400", "Kitchen")
	speaker.answer("GetZoneGroupState", func(map[string]string) (map[string]string, int) { return nil, 501 })
	p := Player{UUID: "RINCON_A01400", Room: "Kitchen", Host: speaker.host()}
	groups, err := testClient(ssdpGroup, speaker.port()).Groups(context.Background(), []Player{p})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != p.UUID || groups[0].Coordinator != p {
		t.Errorf("groups %+v", groups)
	}
}

func TestStateAndVolume(t *testing.T) {
	speaker := newFakeSpeaker(t, "RINCON_A01400", "Kitchen")
	speaker.answer("GetTransportInfo", func(map[string]string) (map[string]string, int) {
		return map[string]string{"CurrentTransportState": "PLAYING"}, 0
	})
	speaker.answer("GetPositionInfo", func(map[string]string) (map[string]string, int) {
		return map[string]string{"TrackURI": "http://x/a.mp3",
			"TrackMetaData": didl("http://x/a.mp3", Track{Title: "Song & Dance", MIME: "audio/mpeg"})}, 0
	})
	speaker.answer("GetGroupVolume", func(map[string]string) (map[string]string, int) {
		return map[string]string{"CurrentVolume": "23"}, 0
	})
	c := New()
	st, err := c.State(context.Background(), speaker.host())
	if err != nil {
		t.Fatal(err)
	}
	if st != (State{Transport: "PLAYING", Title: "Song & Dance", URI: "http://x/a.mp3", Volume: 23}) {
		t.Errorf("state %+v", st)
	}
	if err := c.SetGroupVolume(context.Background(), speaker.host(), 140); err != nil {
		t.Fatal(err)
	}
	calls := speaker.taken()
	last := calls[len(calls)-2:]
	if last[0].Action != "SnapshotGroupVolume" || last[1].Action != "SetGroupVolume" || last[1].Args["DesiredVolume"] != "100" {
		t.Errorf("volume calls %+v", last)
	}
}

func TestTitleOfWhatHasNone(t *testing.T) {
	for _, meta := range []string{"", "NOT_IMPLEMENTED", "<not xml"} {
		if got := titleOf(meta); got != "" {
			t.Errorf("titleOf(%q) = %q", meta, got)
		}
	}
}

func TestProbeAddsThePort(t *testing.T) {
	var asked string
	c := New()
	c.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = r.URL.String()
		return nil, fmt.Errorf("no network in this test")
	})
	_, _ = c.Probe(context.Background(), "192.168.1.20")
	if want := "http://192.168.1.20:1400/xml/device_description.xml"; asked != want {
		t.Errorf("asked %s, want %s", asked, want)
	}
	_, _ = c.Probe(context.Background(), "fe80::1")
	if u, err := url.Parse(asked); err != nil || u.Host != "[fe80::1]:1400" {
		t.Errorf("asked %s for an IPv6 address", asked)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
