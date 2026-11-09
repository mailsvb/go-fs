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
// Several groups chosen together play as one: the speakers of all but the
// first are told to join it, which groups them as the Sonos app does, for as
// long as nobody groups them otherwise. Ungroup has every room of a group
// play on its own again.
//
// The server reaches the speakers a search finds, and those
// http.sonosSpeakers names, and no other host: the dialog names a group by
// the id of its coordinator, which the server looks up among what it found.
// Playing a file is offered as sending one is, to a session of an account
// that may read it.

const (
	actionSonos = "sonos"
	// sonosParam says what the dialog asks: groups, state, play, resume,
	// pause, stop, volume or ungroup; and what the bar asks of a session:
	// session, skip or end.
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
	// sonosJoinTime bounds the call that has one speaker join a group, so
	// that one that does not answer leaves time for the others to play.
	sonosJoinTime = 5 * time.Second
	// maxSonosRequest bounds the body of a request of the dialog, which
	// may name every file of a folder.
	maxSonosRequest = 256 << 10
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

	// pollEvery, pollNear and startLimit stand in for sonosPollEvery,
	// sonosPollNear and sonosStartLimit in a test.
	pollEvery, pollNear, startLimit time.Duration

	// sessions are the queues groups play; see sonos_session.go.
	sessions sonosSessions

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

// merge has the cache know that the speakers of joined play in group now, so
// that the dialog lists the groups as they are before the speakers' own state
// says so, which takes them a moment.
func (st *sonosState) merge(group sonos.Group) {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := make([]sonos.Group, 0, len(st.groups))
	for _, g := range st.groups {
		if g.ID == group.ID {
			kept = append(kept, group)
			continue
		}
		// what is left of a group some of whose speakers joined
		members := slices.DeleteFunc(slices.Clone(g.Members), func(m sonos.Player) bool { return groupHas(group, m.UUID) })
		switch {
		case len(members) == len(g.Members):
			kept = append(kept, g)
		case len(members) == 0:
		case !groupHas(group, g.Coordinator.UUID):
			g.Members = members
			kept = append(kept, g)
		default:
			// the coordinator left; another speaker leads what is left, and
			// which one only the speakers know: the next search says
			st.found = time.Time{}
		}
	}
	st.groups = kept
}

// split has the cache know that every speaker of the group plays on its own,
// apart from those in left, which could not be told to.
func (st *sonosState) split(group sonos.Group, left []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := make([]sonos.Group, 0, len(st.groups)+len(group.Members))
	for _, g := range st.groups {
		if g.ID != group.ID {
			kept = append(kept, g)
			continue
		}
		stay := sonos.Group{ID: g.ID, Coordinator: g.Coordinator}
		for _, m := range group.Members {
			if m.UUID == group.Coordinator.UUID || slices.Contains(left, m.UUID) {
				stay.Members = append(stay.Members, m)
			} else {
				kept = append(kept, sonos.Group{ID: m.UUID, Coordinator: m, Members: []sonos.Player{m}})
			}
		}
		kept = append(kept, stay)
	}
	slices.SortStableFunc(kept, func(a, b sonos.Group) int {
		return strings.Compare(strings.ToLower(a.Coordinator.Room), strings.ToLower(b.Coordinator.Room))
	})
	st.groups = kept
}

// groupHas reports whether a speaker plays in the group.
func groupHas(g sonos.Group, uuid string) bool {
	return slices.ContainsFunc(g.Members, func(m sonos.Player) bool { return m.UUID == uuid })
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
		if groupHas(g, speaker) {
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
	Group string `json:"group"`
	// Join is more groups to play the file along with Group, which their
	// speakers join.
	Join   []string `json:"join,omitempty"`
	Volume *int     `json:"volume,omitempty"`
	// Files is the queue to play, names of files of the folder of the file
	// played first, in order; none is that file alone.
	Files []string `json:"files,omitempty"`
	// Session and Index are what skip and end name.
	Session string `json:"session,omitempty"`
	Index   *int   `json:"index,omitempty"`
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
	case "groups", "state", "session":
		method = http.MethodGet
	case "play", "resume", "pause", "stop", "volume", "ungroup", "skip", "end":
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
			views = append(views, sonosGroupView{ID: g.ID, Name: g.Name(), Rooms: roomsOf(g)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": views})
		return
	}

	var req sonosRequest
	switch op {
	case "state":
		req.Group = r.URL.Query().Get("group")
	case "session":
		req.Session = r.URL.Query().Get("id")
	default:
		if err := json.NewDecoder(io.LimitReader(r.Body, maxSonosRequest)).Decode(&req); err != nil {
			http.Error(w, "The request could not be read.", http.StatusBadRequest)
			return
		}
	}
	if op == "session" || op == "skip" || op == "end" {
		s.handleSonosSession(ctx, set, w, r, cred, op, req)
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
	switch op {
	case "play":
		s.sonosPlay(ctx, set, w, r, target, cred, group, req)
		return
	case "ungroup":
		s.sonosUngroup(ctx, set, w, r, cred, group)
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
		// a session on the group would take the stop for the end of a file
		// and play on
		s.sonosEnd("Stopped.", group)
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

// sonosPlay has a group play the file, along with the groups of the ids in
// join, whose speakers join it first, and starts the session that plays the
// rest of the queue after it.
func (s *Server) sonosPlay(ctx context.Context, set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, cred credential, group sonos.Group, req sonosRequest) {
	// every group is looked up before any speaker is told anything, so a
	// request naming one that is gone changes nothing
	var joining []sonos.Group
	for _, id := range req.Join {
		if id == group.ID || slices.ContainsFunc(joining, func(g sonos.Group) bool { return g.ID == id }) {
			continue
		}
		g, ok, err := s.sonosGroup(ctx, set, id)
		if err != nil {
			http.Error(w, "The speakers cannot be searched for: "+err.Error(), http.StatusBadGateway)
			return
		}
		if !ok {
			http.Error(w, "That speaker is not on the network any more.", http.StatusNotFound)
			return
		}
		joining = append(joining, g)
	}
	info, err := os.Stat(target.Path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if !sonosPlayable(path.Base(target.Virtual)) {
		http.Error(w, "Sonos speakers do not play this kind of file.", http.StatusUnsupportedMediaType)
		return
	}
	queue, index, err := sonosQueue(cred, target.Virtual, req.Files)
	if err != nil {
		http.Error(w, "The queue cannot be played: "+err.Error()+".", http.StatusBadRequest)
		return
	}
	// a session that plays on any of the speakers ends: they play this now
	s.sonosEnd("Something else plays on its speakers now.", append([]sonos.Group{group}, joining...)...)
	group, failed := s.sonosJoin(ctx, group, joining)
	group, track, err := s.sonosStartTrack(ctx, set, group, target, info)
	if err != nil {
		if linkErr := (*sonosLinkError)(nil); errors.As(err, &linkErr) {
			s.log.Warn("http sonos play refused", "file", target.Virtual, "room", group.Coordinator.Room, "error", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.sonosFailed(w, group, "play", err)
		return
	}
	session, err := s.sonosStartSession(cred.user.name, path.Dir(target.Virtual)+"/", queue, index, group, track.Title)
	if err != nil {
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	s.log.Info("http sonos play", "user", nameOf(cred.user), "file", target.Virtual, "queue", len(queue),
		"room", group.Coordinator.Room, "rooms", len(group.Members), "address", clientAddress(set, r))
	answer := map[string]any{"group": group.ID, "name": group.Name(), "rooms": roomsOf(group),
		"title": track.Title, "session": session.view()}
	if len(failed) > 0 {
		answer["failed"] = failed
	}
	writeJSON(w, http.StatusOK, answer)
}

// sonosLinkError is a file a speaker cannot be handed a link to.
type sonosLinkError struct{ err error }

func (e *sonosLinkError) Error() string { return e.err.Error() }
func (e *sonosLinkError) Unwrap() error { return e.err }

// sonosStartTrack has a group play a file. It returns the group that plays
// it, which is another where the speakers were grouped otherwise since they
// were found, and what the speakers were told of the file.
func (s *Server) sonosStartTrack(ctx context.Context, set *settings, group sonos.Group, target vfs.Target, info os.FileInfo) (sonos.Group, sonos.Track, error) {
	name := path.Base(target.Virtual)
	link, err := s.sonosLink(set, target, info, group.Coordinator)
	if err != nil {
		return group, sonos.Track{}, &sonosLinkError{err}
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
		// the speaker chosen plays in another group now, whose coordinator
		// is told instead
		if regrouped, ok := s.sonosRegrouped(ctx, set, group.Coordinator.UUID); ok {
			group = regrouped
			err = client.Play(ctx, group.Coordinator.Host, link, track)
		}
	}
	return group, track, err
}

// handleSonosSession answers the bar about a session: what it does, and to
// play another file of it or to end it.
func (s *Server) handleSonosSession(ctx context.Context, set *settings, w http.ResponseWriter, r *http.Request, cred credential, op string, req sonosRequest) {
	ss := s.sonos.sessions.get(req.Session, cred.user.name)
	if ss == nil {
		http.Error(w, "That playing has ended.", http.StatusNotFound)
		return
	}
	switch op {
	case "session":
		writeJSON(w, http.StatusOK, ss.view())
		return
	case "skip":
		if req.Index == nil || !ss.skip(*req.Index) {
			http.Error(w, "There is no such file to play, or the playing has ended.", http.StatusConflict)
			return
		}
	case "end":
		if !ss.running() {
			// it ended by itself; what the group plays now is not its
			break
		}
		view := ss.view()
		// the session is done telling the group anything before the group
		// is stopped, so that it does not start the next file after
		ss.finish(sessionStopped, "Stopped.")
		select {
		case <-ss.exited:
		case <-ctx.Done():
		}
		ss.mu.Lock()
		host := ss.group.Coordinator.Host
		ss.mu.Unlock()
		if err := s.sonos.clientOf().Stop(ctx, host); err != nil {
			s.log.Info("http sonos session stopped, but its group was not", "error", err)
		}
		s.log.Info("http sonos session stopped", "user", nameOf(cred.user), "room", view.Room,
			"address", clientAddress(set, r))
	}
	writeJSON(w, http.StatusOK, ss.view())
}

// sonosJoin has every speaker of the groups joining that does not play in
// group yet join it, all at once. It returns group with the speakers that
// did, and what kept the others from it, one line a room.
func (s *Server) sonosJoin(ctx context.Context, group sonos.Group, joining []sonos.Group) (sonos.Group, []string) {
	var members []sonos.Player
	for _, g := range joining {
		for _, m := range g.Members {
			if !groupHas(group, m.UUID) && !slices.ContainsFunc(members, func(p sonos.Player) bool { return p.UUID == m.UUID }) {
				members = append(members, m)
			}
		}
	}
	if len(members) == 0 {
		return group, nil
	}
	client := s.sonos.clientOf()
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, sonosJoinTime)
			defer cancel()
			errs[i] = client.Join(ctx, m.Host, group.Coordinator.UUID)
		})
	}
	wg.Wait()
	var failed []string
	group.Members = slices.Clone(group.Members)
	for i, m := range members {
		if err := errs[i]; err != nil {
			s.log.Warn("http sonos speaker did not join", "room", m.Room, "group", group.Coordinator.Room, "error", err)
			if errors.Is(err, context.DeadlineExceeded) {
				failed = append(failed, m.Room+" does not answer.")
			} else {
				failed = append(failed, m.Room+": "+err.Error())
			}
			continue
		}
		group.Members = append(group.Members, m)
	}
	s.sonos.merge(group)
	return group, failed
}

// sonosUngroup has every room of the group but its coordinator play on its
// own.
func (s *Server) sonosUngroup(ctx context.Context, set *settings, w http.ResponseWriter, r *http.Request, cred credential, group sonos.Group) {
	client := s.sonos.clientOf()
	var left []string
	var failed []string
	for _, m := range group.Members {
		if m.UUID == group.Coordinator.UUID {
			continue
		}
		if err := client.Leave(ctx, m.Host); err != nil {
			s.log.Warn("http sonos speaker did not leave its group", "room", m.Room, "error", err)
			left = append(left, m.UUID)
			failed = append(failed, m.Room)
		}
	}
	s.sonos.split(group, left)
	if len(failed) > 0 {
		http.Error(w, strings.Join(failed, ", ")+" could not be ungrouped.", http.StatusBadGateway)
		return
	}
	s.log.Info("http sonos ungroup", "user", nameOf(cred.user), "room", group.Coordinator.Room,
		"rooms", len(group.Members), "address", clientAddress(set, r))
	w.WriteHeader(http.StatusNoContent)
}

// roomsOf is the rooms of a group, its coordinator's first.
func roomsOf(g sonos.Group) []string {
	rooms := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		rooms = append(rooms, m.Room)
	}
	return rooms
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
