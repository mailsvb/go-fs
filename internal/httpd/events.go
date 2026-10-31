package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// A listing asks after the transfers of its account only while one it knows
// of runs, so a fetch started after it loaded — from the page of a stored
// server in another tab, say — would store into the folder it shows without
// it ever knowing. So the listing keeps a stream open on which the server
// tells it, as they happen, that a transfer of its account started or ended:
// a start wakes its asking up, and an end that stored into the folder it
// shows reloads it.
//
//	GET /?go-fs=events   text/event-stream of "transfer" events
//
// Each event is the job as fetch-job answers it. The stream is a session's of
// an account and nobody else's, as the transfers are, and it tells of that
// account's transfers only. It ends after eventStreamLifetime, and the browser
// opens it again by itself, which authenticates it again: a session that
// ended stops hearing.

const actionEvents = "events"

const (
	// eventStreamLifetime is how long a stream is kept open before the
	// browser is made to open it anew.
	eventStreamLifetime = 10 * time.Minute
	// eventHeartbeat is how often a stream that has nothing to say says so,
	// which keeps a proxy from closing it and finds a client that is gone.
	eventHeartbeat = 30 * time.Second
	// eventRetry is how many milliseconds the browser waits before it opens
	// a stream that ended again.
	eventRetry = 2000
	// eventBacklog is how many events a stream may be behind before it is
	// dropped: a client that reads that slowly opens it anew, and asks after
	// the transfers as it does.
	eventBacklog = 16
)

// eventsAction reports whether a request is for the stream of events.
func eventsAction(r *http.Request) bool {
	return r.URL.Query().Get(sessionParam) == actionEvents
}

// transferEvents are the streams that are open, each of one account.
type transferEvents struct {
	mu   sync.Mutex
	subs map[*eventSubscriber]struct{}
}

// eventSubscriber is one open stream.
type eventSubscriber struct {
	owner string
	ch    chan registryJobJSON
	// lost is closed when the stream fell too far behind and was dropped.
	lost chan struct{}
}

func (e *transferEvents) subscribe(owner string) *eventSubscriber {
	sub := &eventSubscriber{owner: owner, ch: make(chan registryJobJSON, eventBacklog), lost: make(chan struct{})}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.subs == nil {
		e.subs = map[*eventSubscriber]struct{}{}
	}
	e.subs[sub] = struct{}{}
	return sub
}

func (e *transferEvents) unsubscribe(sub *eventSubscriber) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.subs, sub)
}

// publish tells the streams of an account about one of its transfers. It
// never waits: a stream that cannot take the event is dropped instead.
func (e *transferEvents) publish(owner string, view registryJobJSON) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for sub := range e.subs {
		if sub.owner != owner {
			continue
		}
		select {
		case sub.ch <- view:
		default:
			delete(e.subs, sub)
			close(sub.lost)
		}
	}
}

// handleEvents answers the stream of events, until the client goes, the
// server shuts down or the stream has lived its time.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, cred credential) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !mayFollowFetches(cred) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	// the write timeout would end the stream once it has been written for
	// that long; the read one does not reach it, since net/http lifts it
	// itself once a request without a body has been read
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})

	sub := s.transferEvents.subscribe(cred.user.name)
	defer s.transferEvents.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	// a proxy that buffers would hold every event back
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", eventRetry); err != nil || controller.Flush() != nil {
		return
	}

	heartbeat := time.NewTicker(eventHeartbeat)
	defer heartbeat.Stop()
	lifetime := time.NewTimer(eventStreamLifetime)
	defer lifetime.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-sub.lost:
			return
		case <-lifetime.C:
			return
		case <-heartbeat.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		case view := <-sub.ch:
			data, _ := json.Marshal(view)
			_, err = fmt.Fprintf(w, "event: transfer\ndata: %s\n\n", data)
		}
		if err != nil || controller.Flush() != nil {
			return
		}
	}
}
