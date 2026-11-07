package disks

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is what a lease lets its holder do with an image.
type Mode int

const (
	// Reader is an NBD session that attached the image read-only. Any number
	// of them may, alongside a writer.
	Reader Mode = iota
	// Writer is an NBD session that attached it read-write: one at a time,
	// since two VMs writing one disk corrupt it, and none while it is being
	// downloaded or cloned.
	Writer
	// Hold is a download or the source of a clone, which read all of the
	// image and need it to stay as it is: no writer meanwhile.
	Hold
	// Exclusive is a resize, a rename, a delete, a clone into the name or an
	// upload that replaces it: nothing else at all.
	Exclusive
)

// takeoverWait is how long a writer that takes over waits for the session it
// closed to let go of the image.
const takeoverWait = 15 * time.Second

// Session is one NBD connection that has an image attached. The NBD server
// fills it in and registers it with Attach; the Disks page reads it and may
// close it.
type Session struct {
	// ID tells the session apart, assigned by Attach.
	ID string
	// Disk is the image, set by Attach.
	Disk string
	// Account is the username the client certificate maps to, and Client
	// the address the connection came from.
	Account string
	Client  string
	// Writable is whether it attached the image read-write.
	Writable bool
	// Since is when it attached.
	Since time.Time
	// Fingerprint is the SHA-256 of the client certificate, hex. A writer
	// that comes back with the same certificate takes over from itself.
	Fingerprint string
	// Serial is the serial number of the client certificate, and Expires
	// when it runs out.
	Serial  string
	Expires time.Time

	read, written atomic.Int64
	closer        func(reason string)
	closeOnce     sync.Once
	released      chan struct{}
	// seq is the order the sessions were made in. Since cannot tell that
	// apart where the clock ticks coarsely, as on Windows, where two sessions
	// made within a millisecond or so are given the same time.
	seq uint64
}

// sessionSeq numbers the sessions in the order they are made.
var sessionSeq atomic.Uint64

// NewSession is a session that close ends, told why: it is called at most
// once, by the Disks page, by a writer taking over, or by whatever else ends
// it.
func NewSession(close func(reason string)) *Session {
	return &Session{closer: close, released: make(chan struct{}), Since: time.Now(), seq: sessionSeq.Add(1)}
}

// bySeq sorts sessions in the order they were made.
func bySeq(sessions []*Session) {
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].seq < sessions[j].seq })
}

// Close ends the session for the reason given.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		if s.closer != nil {
			s.closer(reason)
		}
	})
}

// AddRead and AddWritten count the bytes the session moved.
func (s *Session) AddRead(n int64)    { s.read.Add(n) }
func (s *Session) AddWritten(n int64) { s.written.Add(n) }

// Read and Written are the bytes the session moved so far.
func (s *Session) Read() int64    { return s.read.Load() }
func (s *Session) Written() int64 { return s.written.Load() }

// BusyError is a lease that was refused, with what is in the way.
type BusyError struct {
	Name   string
	Reason string
}

func (e *BusyError) Error() string { return fmt.Sprintf("%q %s", e.Name, e.Reason) }

// Is makes errors.Is(err, ErrBusy) true for a BusyError.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// Lease is a lock on an image, held until it is released.
type Lease struct {
	reg     *registry
	key     string
	mode    Mode
	session *Session
	once    sync.Once
}

// Release lets go of the image. Releasing twice is harmless.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.reg.release(l) })
}

// Leases release together.
type Leases []*Lease

// Release lets go of every one.
func (ls Leases) Release() {
	for _, l := range ls {
		l.Release()
	}
}

// entry is the state of one image.
type entry struct {
	writer    *Session
	readers   map[*Session]bool
	holds     int
	exclusive string
}

func (e *entry) empty() bool {
	return e.writer == nil && len(e.readers) == 0 && e.holds == 0 && e.exclusive == ""
}

// registry is the state of every image of one folder.
type registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	byID    map[string]*Session
	// work names the uploads and clones in progress, kind and key, which
	// Sweep leaves alone
	work map[string]int
	// uploads serialize the chunks of one upload
	uploads map[string]*sync.Mutex
	jobs    []*Job
}

func newRegistry() *registry {
	return &registry{entries: map[string]*entry{}, byID: map[string]*Session{},
		work: map[string]int{}, uploads: map[string]*sync.Mutex{}}
}

// acquire takes a lease on the image key, called name in messages. session
// is the NBD session of a Reader or a Writer, and why what an Exclusive
// lease is for, as the Disks page shows it.
func (r *registry) acquire(key, name string, mode Mode, session *Session, why string) (*Lease, error) {
	deadline := time.Now().Add(takeoverWait)
	for {
		r.mu.Lock()
		e := r.entries[key]
		if e == nil {
			e = &entry{readers: map[*Session]bool{}}
			r.entries[key] = e
		}
		var victim *Session
		var refusal string
		switch {
		case e.exclusive != "":
			refusal = "is " + e.exclusive
		case mode == Exclusive && e.writer != nil:
			refusal = "is attached read-write " + by(e.writer)
		case mode == Exclusive && len(e.readers) > 0:
			refusal = "is attached read-only " + byAll(e.readers)
		case mode == Exclusive && e.holds > 0:
			refusal = "is being downloaded or cloned"
		case (mode == Writer || mode == Hold) && e.writer != nil:
			if mode == Writer && session.Fingerprint != "" && e.writer.Fingerprint == session.Fingerprint {
				victim = e.writer
			} else {
				refusal = "is attached read-write " + by(e.writer)
			}
		case mode == Writer && e.holds > 0:
			refusal = "is being downloaded or cloned"
		}
		if refusal != "" {
			if e.empty() {
				delete(r.entries, key)
			}
			r.mu.Unlock()
			return nil, &BusyError{Name: name, Reason: refusal}
		}
		if victim != nil {
			r.mu.Unlock()
			// the same client is back, its old connection most likely dead
			// without TCP having noticed yet: end it, and wait for it to let
			// go of the image before the new one may write
			victim.Close("a new connection with the same certificate took over")
			select {
			case <-victim.released:
			case <-time.After(time.Until(deadline)):
				return nil, &BusyError{Name: name, Reason: "is still attached read-write " + by(victim) +
					", which did not let go when it was closed"}
			}
			continue
		}
		switch mode {
		case Reader:
			e.readers[session] = true
		case Writer:
			e.writer = session
		case Hold:
			e.holds++
		case Exclusive:
			e.exclusive = why
		}
		if session != nil {
			session.ID = newID()
			r.byID[session.ID] = session
		}
		r.mu.Unlock()
		return &Lease{reg: r, key: key, mode: mode, session: session}, nil
	}
}

func (r *registry) release(l *Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[l.key]
	if e != nil {
		switch l.mode {
		case Reader:
			delete(e.readers, l.session)
		case Writer:
			if e.writer == l.session {
				e.writer = nil
			}
		case Hold:
			e.holds--
		case Exclusive:
			e.exclusive = ""
		}
		if e.empty() {
			delete(r.entries, l.key)
		}
	}
	if l.session != nil {
		delete(r.byID, l.session.ID)
		close(l.session.released)
	}
}

// state is what List reports of the image key.
func (r *registry) state(key string) (sessions []*Session, busy string, holds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[key]
	if e == nil {
		return nil, "", 0
	}
	if e.writer != nil {
		sessions = append(sessions, e.writer)
	}
	for reader := range e.readers {
		sessions = append(sessions, reader)
	}
	bySeq(sessions)
	return sessions, e.exclusive, e.holds
}

func (r *registry) sessions() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := make([]*Session, 0, len(r.byID))
	for _, session := range r.byID {
		all = append(all, session)
	}
	bySeq(all)
	return all
}

// begin and end mark work on a staging file, which Sweep leaves alone.
func (r *registry) begin(kind, key string) {
	r.mu.Lock()
	r.work[kind+"/"+key]++
	r.mu.Unlock()
}

func (r *registry) end(kind, key string) {
	r.mu.Lock()
	if r.work[kind+"/"+key]--; r.work[kind+"/"+key] <= 0 {
		delete(r.work, kind+"/"+key)
	}
	r.mu.Unlock()
}

func (r *registry) working(kind, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.work[kind+"/"+key] > 0
}

// uploadLock is the mutex of the upload of the image key.
func (r *registry) uploadLock(key string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock := r.uploads[key]
	if lock == nil {
		lock = &sync.Mutex{}
		r.uploads[key] = lock
	}
	return lock
}

func by(s *Session) string {
	return fmt.Sprintf("by %s from %s", s.Account, s.Client)
}

func byAll(readers map[*Session]bool) string {
	var names []string
	for s := range readers {
		names = append(names, fmt.Sprintf("%s from %s", s.Account, s.Client))
	}
	sort.Strings(names)
	return "by " + strings.Join(names, ", ")
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
