//go:build !light && !no_sonos

package httpd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/sonos"
	"github.com/mailsvb/go-fs/internal/vfs"
)

// With http.sonos on, the listing offers Play on Sonos on an audio file. The
// dialog lists the groups of speakers of the local network and has the one
// chosen play the file, pause, play on, stop and change its volume.
//
// A speaker is not sent the file: it is told a URL, which it fetches itself,
// with no account. So the URL carries a key, ?go-fs-play=, which stands in
// for one as a share link's does, for the download of the one file until the
// key runs out or the file changes. Unlike a share link it runs out by
// itself, and only while http.sonos is on does it open anything.
//
// The server reaches the speakers a search finds, and those
// http.sonosSpeakers names, and no other host: the dialog names a group by
// the id of its coordinator, which the server looks up among what it found.
// Playing a file is offered as sending one is, to a session of an account
// that may read it.

const (
	actionSonos = "sonos"
	// sonosParam says what the dialog asks: groups, state, play, resume,
	// pause, stop or volume.
	sonosParam = "sonos"
	// sonosPlayParam carries the key of the link a speaker fetches a file
	// by.
	sonosPlayParam = "go-fs-play"
	// sonosLinkLifetime is how long a speaker may fetch a file it was told
	// to play: long enough for a long recording, paused for a while.
	sonosLinkLifetime = 12 * time.Hour
	// sonosSearchTime is how long a search waits for the speakers to answer.
	sonosSearchTime = 2 * time.Second
	// sonosFresh is how long the groups found are believed before the next
	// dialog searches again.
	sonosFresh = 30 * time.Second
	// sonosCallTime bounds the calls a request makes to the speakers.
	sonosCallTime = 15 * time.Second
	// maxSonosRequest bounds the body of a request of the dialog.
	maxSonosRequest = 4 << 10
)

// sonosTypes are the files a speaker plays, by extension, which the listing
// also knows; see sonosPlayable in listing.js.
var sonosTypes = []string{".mp3", ".m4a", ".aac", ".flac", ".ogg", ".oga", ".wav"}

func sonosPlayable(name string) bool {
	return slices.Contains(sonosTypes, strings.ToLower(path.Ext(name)))
}

func sonosAction(r *http.Request) bool {
	return r.URL.Query().Get(sessionParam) == actionSonos
}

// sonosOn reports whether the listing offers Play on Sonos at all.
func sonosOn(set *settings) bool { return set.cfg.Sonos }

// sonosState is the speakers the last search found, grouped.
type sonosState struct {
	// search lets one search run at a time; a request that comes while one
	// runs waits for it and takes what it found.
	search sync.Mutex

	// wait is how long a search waits for answers, sonosSearchTime but in
	// a test.
	wait time.Duration

	mu       sync.Mutex
	client   *sonos.Client
	groups   []sonos.Group
	found    time.Time
	speakers []string // the http.sonosSpeakers the groups were found with
}

func (st *sonosState) clientOf() *sonos.Client {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.client == nil {
		st.client = sonos.New()
	}
	return st.client
}

// cached is the groups the last search found, while they are fresh and the
// speakers the configuration names are those they were found with.
func (st *sonosState) cached(speakers []string) ([]sonos.Group, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.found.IsZero() || time.Since(st.found) > sonosFresh || !slices.Equal(st.speakers, speakers) {
		return nil, false
	}
	return st.groups, true
}

// sonosGroups is the groups of speakers: those found a moment ago, or
// those a search finds now. fresh searches whatever was found before.
func (s *Server) sonosGroups(ctx context.Context, set *settings, fresh bool) ([]sonos.Group, error) {
	st := &s.sonos
	if !fresh {
		if groups, ok := st.cached(set.cfg.SonosSpeakers); ok {
			return groups, nil
		}
	}
	st.search.Lock()
	defer st.search.Unlock()
	// a search that ran while this one waited is as fresh as it gets
	if groups, ok := st.cached(set.cfg.SonosSpeakers); ok && (!fresh || time.Since(st.found) < time.Second) {
		return groups, nil
	}
	client := st.clientOf()
	wait := st.wait
	if wait == 0 {
		wait = sonosSearchTime
	}
	players, searchErr := client.Discover(ctx, wait)
	if searchErr != nil {
		s.log.Warn("http sonos search failed", "error", searchErr)
	}
	for _, raw := range set.cfg.SonosSpeakers {
		host, err := config.SonosSpeaker(raw)
		if err != nil {
			continue
		}
		p, err := client.Probe(ctx, host)
		if err != nil {
			s.log.Info("http sonos speaker of http.sonosSpeakers does not answer", "speaker", raw, "error", err)
			continue
		}
		if !slices.ContainsFunc(players, func(q sonos.Player) bool { return q.UUID == p.UUID }) {
			players = append(players, p)
		}
	}
	if len(players) == 0 && searchErr != nil {
		return nil, searchErr
	}
	groups, err := client.Groups(ctx, players)
	if err != nil {
		return nil, err
	}
	s.log.Debug("http sonos speakers found", "speakers", len(players), "groups", len(groups))
	st.mu.Lock()
	st.groups, st.found, st.speakers = groups, time.Now(), slices.Clone(set.cfg.SonosSpeakers)
	st.mu.Unlock()
	return groups, nil
}

// sonosGroup is the group whose coordinator has the id, searching again
// when the groups found do not have it.
func (s *Server) sonosGroup(ctx context.Context, set *settings, id string) (sonos.Group, bool, error) {
	for _, fresh := range []bool{false, true} {
		groups, err := s.sonosGroups(ctx, set, fresh)
		if err != nil {
			return sonos.Group{}, false, err
		}
		for _, g := range groups {
			if g.ID == id {
				return g, true, nil
			}
		}
	}
	return sonos.Group{}, false, nil
}

// sonosRegrouped is the group a speaker plays in now, after a call to it
// was refused because it no longer coordinates the group it did.
func (s *Server) sonosRegrouped(ctx context.Context, set *settings, speaker string) (sonos.Group, bool) {
	groups, err := s.sonosGroups(ctx, set, true)
	if err != nil {
		return sonos.Group{}, false
	}
	for _, g := range groups {
		if slices.ContainsFunc(g.Members, func(m sonos.Player) bool { return m.UUID == speaker }) {
			return g, true
		}
	}
	return sonos.Group{}, false
}

// sonosKey is the key of the link a speaker fetches a file by, until expires,
// unix seconds. Like a share link's it is bound to the path and to the
// modification time and the size, so a file that changes is not served by
// it; the first line keeps it from ever being a key of anything else.
func sonosKey(secret []byte, virtual string, info os.FileInfo, expires int64) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "sonos\n%s\n%d\n%d\n%d", virtual, expires,
		info.ModTime().UnixNano(), info.Size())
	return strconv.FormatInt(expires, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// sonosFile reports whether a request is a speaker fetching a file it was
// told to play, by a key that is still good. As with a share link, anything
// else is left to be authenticated as any other request.
func (s *Server) sonosFile(set *settings, r *http.Request, target vfs.Target) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	key := r.URL.Query().Get(sonosPlayParam)
	if key == "" || !sonosOn(set) {
		return false
	}
	stamp, _, ok := strings.Cut(key, ".")
	expires, err := strconv.ParseInt(stamp, 10, 64)
	if !ok || err != nil || time.Now().Unix() > expires {
		s.log.Debug("http sonos link has run out", "file", target.Virtual,
			"address", clientAddress(set, r))
		return false
	}
	info, err := os.Stat(target.Path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	expected := sonosKey(s.shareSecret(set), target.Virtual, info, expires)
	if !hmac.Equal([]byte(key), []byte(expected)) {
		s.log.Debug("http sonos key does not match", "file", target.Virtual,
			"address", clientAddress(set, r))
		return false
	}
	return true
}

// sonosRequest is the body of what the dialog posts.
type sonosRequest struct {
	Group  string `json:"group"`
	Volume *int   `json:"volume,omitempty"`
}

// sonosGroupView is a group as the dialog lists it.
type sonosGroupView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Rooms []string `json:"rooms"`
}

// sonosStateView is what a group is doing, as the dialog shows it.
type sonosStateView struct {
	sonos.State
	// File says the group plays the file the dialog was opened on.
	File bool `json:"file"`
}

// handleSonos answers the dialog.
func (s *Server) handleSonos(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, cred credential) {
	w.Header().Set("Cache-Control", "no-store")
	if !sonosOn(set) {
		http.NotFound(w, r)
		return
	}
	op := r.URL.Query().Get(sonosParam)
	method := http.MethodPost
	switch op {
	case "groups", "state":
		method = http.MethodGet
	case "play", "resume", "pause", "stop", "volume":
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if method == http.MethodPost && !s.sameSite(set, w, r) {
		return
	}
	if !maySend(cred, target.Virtual) {
		s.log.Info("http sonos refused, no session of an account that may read here",
			"user", nameOf(cred.user), "path", target.Virtual, "address", clientAddress(set, r))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sonosCallTime)
	defer cancel()

	if op == "groups" {
		groups, err := s.sonosGroups(ctx, set, r.URL.Query().Get("refresh") != "")
		if err != nil {
			s.log.Warn("http sonos speakers cannot be found", "error", err)
			http.Error(w, "The speakers cannot be searched for: "+err.Error(), http.StatusBadGateway)
			return
		}
		views := make([]sonosGroupView, 0, len(groups))
		for _, g := range groups {
			view := sonosGroupView{ID: g.ID, Name: g.Name()}
			for _, m := range g.Members {
				view.Rooms = append(view.Rooms, m.Room)
			}
			views = append(views, view)
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": views})
		return
	}

	var req sonosRequest
	if op == "state" {
		req.Group = r.URL.Query().Get("group")
	} else if err := json.NewDecoder(io.LimitReader(r.Body, maxSonosRequest)).Decode(&req); err != nil {
		http.Error(w, "The request could not be read.", http.StatusBadRequest)
		return
	}
	group, ok, err := s.sonosGroup(ctx, set, req.Group)
	if err != nil {
		http.Error(w, "The speakers cannot be searched for: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !ok {
		http.Error(w, "That speaker is not on the network any more.", http.StatusNotFound)
		return
	}
	if op == "play" {
		s.sonosPlay(ctx, set, w, r, target, cred, group)
		return
	}

	client := s.sonos.clientOf()
	host := group.Coordinator.Host
	switch op {
	case "state":
		st, err := client.State(ctx, host)
		if err != nil {
			s.sonosFailed(w, group, op, err)
			return
		}
		writeJSON(w, http.StatusOK, sonosStateView{State: st, File: playsFile(st.URI, target.Virtual)})
		return
	case "resume":
		err = client.Resume(ctx, host)
	case "pause":
		err = client.Pause(ctx, host)
	case "stop":
		err = client.Stop(ctx, host)
	case "volume":
		if req.Volume == nil {
			http.Error(w, "The request names no volume.", http.StatusBadRequest)
			return
		}
		err = client.SetGroupVolume(ctx, host, *req.Volume)
	}
	if err != nil {
		s.sonosFailed(w, group, op, err)
		return
	}
	s.log.Debug("http sonos "+op, "user", nameOf(cred.user), "room", group.Coordinator.Room)
	w.WriteHeader(http.StatusNoContent)
}

// playsFile reports whether uri, what a group plays, is the link to the file
// at virtual.
func playsFile(uri, virtual string) bool {
	u, err := url.Parse(uri)
	return err == nil && u.Path == virtual && u.Query().Has(sonosPlayParam)
}

// sonosFailed answers a call a speaker refused or did not answer.
func (s *Server) sonosFailed(w http.ResponseWriter, group sonos.Group, op string, err error) {
	s.log.Warn("http sonos call failed", "op", op, "room", group.Coordinator.Room, "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, group.Coordinator.Room+" does not answer.", http.StatusGatewayTimeout)
		return
	}
	http.Error(w, group.Coordinator.Room+": "+err.Error(), http.StatusBadGateway)
}

// sonosPlay has a group play the file.
func (s *Server) sonosPlay(ctx context.Context, set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, cred credential, group sonos.Group) {
	info, err := os.Stat(target.Path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	name := path.Base(target.Virtual)
	if !sonosPlayable(name) {
		http.Error(w, "Sonos speakers do not play this kind of file.", http.StatusUnsupportedMediaType)
		return
	}
	link, err := s.sonosLink(set, target, info, group.Coordinator)
	if err != nil {
		s.log.Warn("http sonos play refused", "file", target.Virtual, "room", group.Coordinator.Room, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	track := sonos.Track{Title: strings.TrimSuffix(name, path.Ext(name)), MIME: typeOf(name).Media}
	if tags, err := audio.Read(target.Path); err == nil {
		if tags.Title != "" {
			track.Title = tags.Title
		}
		track.Artist = tags.Artist
		track.Album = tags.Album
	}
	client := s.sonos.clientOf()
	err = client.Play(ctx, group.Coordinator.Host, link, track)
	if sonos.NotCoordinator(err) {
		// the speakers were grouped otherwise since they were found; the
		// speaker chosen plays in another group now, whose coordinator is
		// told instead
		if regrouped, ok := s.sonosRegrouped(ctx, set, group.Coordinator.UUID); ok {
			group = regrouped
			err = client.Play(ctx, group.Coordinator.Host, link, track)
		}
	}
	if err != nil {
		s.sonosFailed(w, group, "play", err)
		return
	}
	s.log.Info("http sonos play", "user", nameOf(cred.user), "file", target.Virtual,
		"room", group.Coordinator.Room, "address", clientAddress(set, r))
	writeJSON(w, http.StatusOK, map[string]string{"group": group.ID, "name": group.Name(), "title": track.Title})
}

// sonosLink is the URL a speaker fetches a file by: at general.internalAddress
// where that is set, or else at the address of this server on the network
// the speaker is on.
func (s *Server) sonosLink(set *settings, target vfs.Target, info os.FileInfo, speaker sonos.Player) (string, error) {
	base := set.cfg.ShareInternalAddress
	if base == "" {
		addr, err := net.ResolveUDPAddr("udp", speaker.Host)
		if err != nil {
			return "", fmt.Errorf("%s has no address: %w", speaker.Room, err)
		}
		base = s.reachableAddress(set, addr)
		if base == "" {
			return "", errors.New("this server has no address " + speaker.Room + " reaches")
		}
		u, err := url.Parse(base)
		if err == nil {
			if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() && !addr.IP.IsLoopback() {
				return "", errors.New("http.address is " + u.Hostname() +
					", which no speaker reaches; set it to the address of the network or to 0.0.0.0")
			}
			if u.Scheme == "https" {
				s.log.Warn("http sonos speakers fetch from the https listener, which they only do with a certificate they trust; " +
					"switch http on or set general.internalAddress")
			}
		}
	}
	expires := time.Now().Add(sonosLinkLifetime).Unix()
	return base + (&url.URL{Path: target.Virtual}).EscapedPath() + "?" + sonosPlayParam + "=" +
		sonosKey(s.shareSecret(set), target.Virtual, info, expires), nil
}
