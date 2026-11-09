//go:build !light && !no_sonos

package httpd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/sonos"
)

// What a group of speakers is told to play goes on in the server, as a
// session: a queue of files of one folder, in the order the listing showed
// them, played one after the other. The page that started it may be closed;
// the listing shows the session again in its bar when it comes back. A
// speaker says nothing when a file ends, and a file that is not a queue of
// its own is not followed by another, so the session asks the group what it
// is doing every few seconds, more often near the end of a file, and tells it
// the next file once the last one ended.
//
// A session ends after its last file; when it is stopped, here or in the
// Sonos app; when the group is told to play something else; when its
// speakers no longer answer; when http.sonos is switched off or the account
// may no longer read the folder; and with the server.

const (
	// sonosPollEvery is how often a session asks its group what it does,
	// and sonosPollNear how often near the end of a file, which is when
	// the next one is to be told.
	sonosPollEvery = 2 * time.Second
	sonosPollNear  = 500 * time.Millisecond
	// sonosNearEnd is how close to its end, in seconds, a file is when it
	// stops at its end rather than by someone stopping it.
	sonosNearEnd = 3
	// sonosStartLimit is how long a file may take to start playing before
	// it is taken as one the speakers cannot play.
	sonosStartLimit = 20 * time.Second
	// sonosPauseLimit is how long a session waits for a group paused to
	// play on. The link of the file runs out after sonosLinkLifetime.
	sonosPauseLimit = 6 * time.Hour
	// sonosUnanswered is how many calls in a row the group may leave
	// unanswered before the session gives up.
	sonosUnanswered = 5
	// sonosKeepEnded is how long a session that ended is still told of,
	// so that a bar asking after it learns how it ended.
	sonosKeepEnded = time.Minute
	// maxSonosFiles bounds the queue of a session.
	maxSonosFiles = 1000
)

// The states of a session.
const (
	sessionPlaying = "playing"
	sessionPaused  = "paused"
	sessionDone    = "done"
	sessionStopped = "stopped"
	sessionFailed  = "failed"
)

// sonosSession is a queue that a group plays.
type sonosSession struct {
	id      string
	owner   string
	folder  string // virtual, with its trailing slash
	files   []string
	started time.Time
	cancel  context.CancelFunc
	// wake has the session look at what it was told at once, rather than
	// after its wait.
	wake chan struct{}
	// exited is closed when the session has stopped telling the speakers
	// anything.
	exited chan struct{}

	mu     sync.Mutex
	index  int
	group  sonos.Group
	title  string
	state  string
	reason string
	last   sonos.State
	// lastAt is when last was asked
	lastAt time.Time
	ended  time.Time
	played int
	// skipTo is the file the session was told to play next, -1 for none.
	skipTo int
}

// sonosSessionView is a session as the bar shows it.
type sonosSessionView struct {
	ID        string   `json:"id"`
	Folder    string   `json:"folder"`
	Files     int      `json:"files"`
	Index     int      `json:"index"`
	Name      string   `json:"name"`
	Title     string   `json:"title,omitempty"`
	Group     string   `json:"group"`
	Room      string   `json:"room"`
	Rooms     []string `json:"rooms"`
	State     string   `json:"state"`
	Transport string   `json:"transport,omitempty"`
	Position  int      `json:"position"`
	Duration  int      `json:"duration"`
	Volume    int      `json:"volume"`
	Message   string   `json:"message,omitempty"`
}

func (ss *sonosSession) view() sonosSessionView {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	// the group was asked a moment ago; a file that plays has gone on since
	position := ss.last.Position
	if ss.last.Transport == "PLAYING" && !ss.lastAt.IsZero() {
		position += int(time.Since(ss.lastAt).Seconds())
		if ss.last.Duration > 0 {
			position = min(position, ss.last.Duration)
		}
	}
	return sonosSessionView{
		ID: ss.id, Folder: ss.folder, Files: len(ss.files), Index: ss.index,
		Name: ss.files[ss.index], Title: ss.title,
		Group: ss.group.ID, Room: ss.group.Name(), Rooms: roomsOf(ss.group),
		State: ss.state, Transport: ss.last.Transport,
		Position: position, Duration: ss.last.Duration, Volume: ss.last.Volume,
		Message: ss.reason,
	}
}

func (ss *sonosSession) running() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.state == sessionPlaying || ss.state == sessionPaused
}

// finish ends the session, unless it already ended.
func (ss *sonosSession) finish(state, reason string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.state != sessionPlaying && ss.state != sessionPaused {
		return
	}
	ss.state, ss.reason, ss.ended = state, reason, time.Now()
	ss.cancel()
}

func (ss *sonosSession) poke() {
	select {
	case ss.wake <- struct{}{}:
	default:
	}
}

// shares reports whether the session plays on a speaker of the groups.
func (ss *sonosSession) shares(groups ...sonos.Group) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, m := range ss.group.Members {
		for _, g := range groups {
			if groupHas(g, m.UUID) {
				return true
			}
		}
	}
	return false
}

// sonosSessions are the sessions of the server.
type sonosSessions struct {
	mu   sync.Mutex
	byID map[string]*sonosSession
}

func (all *sonosSessions) add(ss *sonosSession) {
	all.mu.Lock()
	defer all.mu.Unlock()
	if all.byID == nil {
		all.byID = map[string]*sonosSession{}
	}
	for id, other := range all.byID {
		other.mu.Lock()
		old := !other.ended.IsZero() && time.Since(other.ended) > sonosKeepEnded
		other.mu.Unlock()
		if old {
			delete(all.byID, id)
		}
	}
	all.byID[ss.id] = ss
}

// get is a session of an account; someone else's is not there to it.
func (all *sonosSessions) get(id, owner string) *sonosSession {
	all.mu.Lock()
	defer all.mu.Unlock()
	if ss := all.byID[id]; ss != nil && ss.owner == owner {
		return ss
	}
	return nil
}

// newest is the session of an account that runs and started last.
func (all *sonosSessions) newest(owner string) *sonosSession {
	all.mu.Lock()
	defer all.mu.Unlock()
	var found *sonosSession
	for _, ss := range all.byID {
		if ss.owner == owner && ss.running() && (found == nil || ss.started.After(found.started)) {
			found = ss
		}
	}
	return found
}

// matching is the sessions that run and match.
func (all *sonosSessions) matching(match func(*sonosSession) bool) []*sonosSession {
	all.mu.Lock()
	defer all.mu.Unlock()
	var found []*sonosSession
	for _, ss := range all.byID {
		if ss.running() && match(ss) {
			found = append(found, ss)
		}
	}
	return found
}

// sonosEnd ends the sessions that play on a speaker of the groups, and waits
// until they no longer tell the speakers anything, so that what is told next
// is not undone by them.
func (s *Server) sonosEnd(reason string, groups ...sonos.Group) {
	for _, ss := range s.sonos.sessions.matching(func(ss *sonosSession) bool { return ss.shares(groups...) }) {
		ss.finish(sessionStopped, reason)
		select {
		case <-ss.exited:
		case <-time.After(sonosCallTime):
		}
	}
}

// sonosSessionOf is the session the account runs as the listing hands it to
// its bar, JSON, or "" for none.
func (s *Server) sonosSessionOf(cred credential) string {
	if cred.user == nil {
		return ""
	}
	ss := s.sonos.sessions.newest(cred.user.name)
	if ss == nil {
		return ""
	}
	encoded, err := json.Marshal(ss.view())
	if err != nil {
		return ""
	}
	return string(encoded)
}

// sonosQueue is the queue a request names: files of the folder of target,
// in order, which hold target; just target when it names none. A name that
// is not of a file the account may have played is left out.
func sonosQueue(cred credential, target string, files []string) ([]string, int, error) {
	folder, name := path.Split(target)
	if len(files) == 0 {
		return []string{name}, 0, nil
	}
	if len(files) > maxSonosFiles {
		return nil, 0, fmt.Errorf("a queue holds at most %d files", maxSonosFiles)
	}
	queue := make([]string, 0, len(files))
	at := -1
	for _, f := range files {
		if f == "" || f == "." || f == ".." || strings.ContainsAny(f, "/\\") || !sonosPlayable(f) ||
			!fetchGranted(cred.user, folder+f, actRead) {
			continue
		}
		if f == name && at < 0 {
			at = len(queue)
		}
		queue = append(queue, f)
	}
	if at < 0 {
		return nil, 0, errors.New("the queue does not hold the file")
	}
	return queue, at, nil
}

// sonosStartSession has the session of a queue follow a group, which was
// just told to play the file at index.
func (s *Server) sonosStartSession(owner, folder string, files []string, index int, group sonos.Group, title string) (*sonosSession, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	ss := &sonosSession{
		id: hex.EncodeToString(raw), owner: owner, folder: folder, files: files,
		started: time.Now(), cancel: cancel,
		wake: make(chan struct{}, 1), exited: make(chan struct{}),
		index: index, group: group, title: title, state: sessionPlaying, skipTo: -1,
	}
	s.sonos.sessions.add(ss)
	// the shutdown waits for the session, which it stops first
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(ss.exited)
		defer cancel()
		go func() {
			select {
			case <-s.done:
				ss.finish(sessionStopped, "The server stopped.")
			case <-ctx.Done():
			}
		}()
		s.sonosRun(ctx, ss)
		s.log.Info("http sonos session ended", "user", owner, "folder", folder,
			"room", ss.view().Room, "state", ss.view().State, "played", ss.played)
	}()
	return ss, nil
}

// the ways a file a session follows comes to an end
type sonosOutcome int

const (
	outcomeEnded   sonosOutcome = iota // it played to its end
	outcomeSkipped                     // another file was asked for
	outcomeFailed                      // it never started
	outcomeOver                        // the session ended
)

func (s *Server) sonosRun(ctx context.Context, ss *sonosSession) {
	failures := 0
	told := true // the request that started the session told the first file
	for {
		if !told && !s.sonosTell(ctx, ss) {
			return
		}
		told = false
		switch s.sonosFollow(ctx, ss) {
		case outcomeOver:
			return
		case outcomeSkipped:
			ss.mu.Lock()
			ss.index, ss.skipTo = ss.skipTo, -1
			ss.mu.Unlock()
			failures = 0
			continue
		case outcomeEnded:
			failures = 0
			ss.mu.Lock()
			ss.played++
			ss.mu.Unlock()
		case outcomeFailed:
			failures++
			s.log.Warn("http sonos file did not start", "file", ss.folder+ss.view().Name, "room", ss.view().Room)
		}
		ss.mu.Lock()
		last := ss.index >= len(ss.files)-1
		if !last {
			ss.index++
		}
		played, room := ss.played, ss.group.Name()
		ss.mu.Unlock()
		if last {
			if played == 0 && failures > 0 {
				ss.finish(sessionFailed, room+" played none of the files.")
			} else {
				ss.finish(sessionDone, fmt.Sprintf("Played %d of %d files on %s.", played, len(ss.files), room))
			}
			return
		}
	}
}

// sonosTell tells the group of the session to play the file at its index,
// or the first after it that can be played. It reports whether one was.
func (s *Server) sonosTell(ctx context.Context, ss *sonosSession) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		set := s.settings()
		ss.mu.Lock()
		name, group := ss.files[ss.index], ss.group
		ss.mu.Unlock()
		virtual := ss.folder + name
		if !sonosOn(set) {
			ss.finish(sessionStopped, "Playing on Sonos was switched off.")
			return false
		}
		if user := accountNamed(set.accounts, ss.owner); user == nil || !fetchGranted(user, virtual, actRead) {
			ss.finish(sessionStopped, "The account may no longer play "+name+".")
			return false
		}
		target := s.root.Resolve("/", virtual)
		info, err := os.Stat(target.Path)
		if !target.Valid || err != nil || !info.Mode().IsRegular() {
			s.log.Info("http sonos session passes over a file that is gone", "file", virtual)
			if !ss.advance() {
				ss.finish(sessionDone, "The rest of the files are gone.")
				return false
			}
			continue
		}
		// what the group did is of the file before, which is not shown
		// as this one's
		ss.mu.Lock()
		ss.last = sonos.State{Volume: ss.last.Volume}
		ss.mu.Unlock()
		callCtx, cancel := context.WithTimeout(ctx, sonosCallTime)
		group, track, err := s.sonosStartTrack(callCtx, set, group, target, info)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			s.log.Warn("http sonos session cannot play", "file", virtual, "room", group.Coordinator.Room, "error", err)
			ss.finish(sessionFailed, group.Coordinator.Room+": "+err.Error())
			return false
		}
		ss.mu.Lock()
		ss.group, ss.title = group, track.Title
		ss.mu.Unlock()
		s.log.Debug("http sonos session plays", "file", virtual, "room", group.Coordinator.Room)
		return true
	}
}

// advance moves to the next file, reporting whether there is one.
func (ss *sonosSession) advance() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.index >= len(ss.files)-1 {
		return false
	}
	ss.index++
	return true
}

// skip has the session play the file at index next.
func (ss *sonosSession) skip(index int) bool {
	ss.mu.Lock()
	if index < 0 || index >= len(ss.files) || (ss.state != sessionPlaying && ss.state != sessionPaused) {
		ss.mu.Unlock()
		return false
	}
	ss.skipTo = index
	ss.mu.Unlock()
	ss.poke()
	return true
}

// sonosFollow asks the group what it does until the file it was told ends,
// or the session is told otherwise.
func (s *Server) sonosFollow(ctx context.Context, ss *sonosSession) sonosOutcome {
	every, near, limit := s.sonos.timing()
	client := s.sonos.clientOf()
	ss.mu.Lock()
	virtual := ss.folder + ss.files[ss.index]
	ss.mu.Unlock()
	began := time.Now()
	seen := false
	var pausedSince time.Time
	position, duration := 0, 0
	unanswered := 0
	for first := true; ; first = false {
		// the first answer soon, which says how long the file is
		wait := every
		if first || (seen && duration > 0 && duration-position <= sonosNearEnd) {
			wait = near
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return outcomeOver
		case <-ss.wake:
			timer.Stop()
		case <-timer.C:
		}
		ss.mu.Lock()
		skipping := ss.skipTo >= 0
		group := ss.group
		ss.mu.Unlock()
		if skipping {
			return outcomeSkipped
		}

		callCtx, cancel := context.WithTimeout(ctx, sonosCallTime)
		st, err := client.State(callCtx, group.Coordinator.Host)
		cancel()
		if ctx.Err() != nil {
			return outcomeOver
		}
		if err != nil {
			if sonos.NotCoordinator(err) {
				if regrouped, ok := s.sonosRegrouped(ctx, s.settings(), group.Coordinator.UUID); ok {
					ss.mu.Lock()
					ss.group = regrouped
					ss.mu.Unlock()
					continue
				}
			}
			unanswered++
			s.log.Debug("http sonos session cannot ask the group", "room", group.Coordinator.Room, "error", err)
			if unanswered >= sonosUnanswered {
				ss.finish(sessionFailed, group.Coordinator.Room+" does not answer.")
				return outcomeOver
			}
			continue
		}
		unanswered = 0
		ss.mu.Lock()
		ss.last, ss.lastAt = st, time.Now()
		ss.mu.Unlock()

		// the group was told to play something else, here or in the app
		if !playsFile(st.URI, virtual) {
			ss.finish(sessionStopped, "Something else plays on "+group.Name()+" now.")
			return outcomeOver
		}
		switch st.Transport {
		case "PLAYING":
			seen = true
			pausedSince = time.Time{}
			// the state and the position are asked one after the other: a
			// file that ends between the two is still playing, but back at
			// the start, and then stopped. A file near its end does not go
			// back to its start.
			if !(st.Position < position && duration > 0 && duration-position <= sonosNearEnd) {
				position = st.Position
			}
			if st.Duration > 0 {
				duration = st.Duration
			}
			ss.setState(sessionPlaying)
		case "PAUSED_PLAYBACK":
			seen = true
			if pausedSince.IsZero() {
				pausedSince = time.Now()
			}
			if time.Since(pausedSince) > sonosPauseLimit {
				ss.finish(sessionStopped, "Paused for too long.")
				return outcomeOver
			}
			ss.setState(sessionPaused)
		case "STOPPED":
			if !seen {
				if time.Since(began) > limit {
					return outcomeFailed
				}
				continue
			}
			// a file whose length is not known can only be taken to have
			// ended
			if duration == 0 || position >= duration-sonosNearEnd {
				return outcomeEnded
			}
			ss.finish(sessionStopped, "Stopped on "+group.Name()+".")
			return outcomeOver
		default:
			if !seen && time.Since(began) > limit {
				return outcomeFailed
			}
		}
	}
}

func (ss *sonosSession) setState(state string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.state == sessionPlaying || ss.state == sessionPaused {
		ss.state = state
	}
}

// timing is how often sessions ask their groups and how long a file may
// take to start, shorter in a test.
func (st *sonosState) timing() (every, near, limit time.Duration) {
	every, near, limit = sonosPollEvery, sonosPollNear, sonosStartLimit
	if st.pollEvery > 0 {
		every = st.pollEvery
	}
	if st.pollNear > 0 {
		near = st.pollNear
	}
	if st.startLimit > 0 {
		limit = st.startLimit
	}
	return every, near, limit
}
