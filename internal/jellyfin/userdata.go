package jellyfin

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/store"
)

// What a listener did is kept in the persistent data file, by account and by
// item, so that a client finds its favorites and what was played after a
// restart. The account is the first part of every key, so that what one
// account has is read in one go: user NUL item for the records, and user NUL
// time NUL item for the plays, which so come in the order they were played.
//
// Records of an account or a file that is gone are kept: a library is built
// after the start, from its folder, so that an item not found yet is not one
// that is gone, and an account added again finds what it had.
const (
	userDataBucket = "jellyfin.userdata"
	historyBucket  = "jellyfin.history"
)

// A play counts once the listener heard half of the track, or four minutes
// of it, whichever comes first, as Last.fm counts a scrobble. A play nothing
// was heard of for an hour is given up.
const (
	countAfter = 4 * time.Minute
	playIdle   = time.Hour
)

// Where a track that was stopped is resumed from is kept only for tracks of
// ten minutes or more, a mix or a chapter of a book, and only when it was
// stopped after the first 5% and before the last 10% of it, which are the
// bounds Jellyfin has.
const (
	resumeMinimum = 10 * time.Minute
	resumeFrom    = 5
	resumeUntil   = 90
)

// historyTime is the time of a play in its key: fixed in width, so that the
// keys sort as the times do.
const historyTime = "2006-01-02T15:04:05.000000000Z"

// Record is what is kept of a listener and an item. A record of nothing is
// not kept.
type Record struct {
	Played    bool `json:"played,omitempty"`
	PlayCount int  `json:"playCount,omitempty"`
	// LastPlayed is when the item was played last.
	LastPlayed time.Time `json:"lastPlayed,omitzero"`
	// Position is where a track is resumed from, in ticks.
	Position int64 `json:"position,omitempty"`
	Favorite bool  `json:"favorite,omitempty"`
}

// Play is a play of a track that counted, as the history keeps it: with what
// the track was then, so that it says what was played after the file was
// moved or the library is gone.
type Play struct {
	At      time.Time     `json:"at"`
	Item    string        `json:"item"`
	Library string        `json:"library"`
	Href    string        `json:"href"`
	Track   string        `json:"track"`
	Album   string        `json:"album"`
	Artist  string        `json:"artist"`
	Runtime time.Duration `json:"runtime"`
	// Heard is how much of the track was heard, by when the play counted;
	// zero for one a client said was played without playing it here.
	Heard  time.Duration `json:"heard"`
	Client string        `json:"client,omitempty"`
	Device string        `json:"device,omitempty"`
}

// Report is what a client reports of the track it plays.
type Report struct {
	User   string
	Device string
	Client string
	Track  *Track
	// Position is where the client is in the track, when HasPosition says
	// it said.
	Position    time.Duration
	HasPosition bool
}

// Listeners keeps what the accounts did with the libraries: a record of each
// item an account played or made a favorite, and the history of its plays.
// The records of an account are read the first time it is asked about, and
// written through. With no store, they are kept for this run only.
type Listeners struct {
	data    *store.Bucket
	history *store.Bucket
	log     *slog.Logger
	// now is the clock, which the tests set.
	now func() time.Time

	mu sync.RWMutex
	// records are those of the accounts read so far, by account and item.
	records map[string]map[string]Record
	// plays are the plays going on, by account and device.
	plays map[string]*play
}

// play is a track a client plays, until it stops or plays another.
type play struct {
	track   *Track
	client  string
	device  string
	started time.Time
	// seen is when the client last reported the play.
	seen time.Time
	// heard is the furthest the client said it got, and positioned that it
	// said where it got at all; a client that says nothing is taken to have
	// played on since the start.
	heard      time.Duration
	positioned bool
	counted    bool
}

// NewListeners keeps what the listeners do in st, which may be nil.
func NewListeners(st *store.Store, logger *slog.Logger) *Listeners {
	return &Listeners{
		data:    st.Bucket(userDataBucket),
		history: st.Bucket(historyBucket),
		log:     logger,
		now:     time.Now,
		records: map[string]map[string]Record{},
		plays:   map[string]*play{},
	}
}

// Get is the record of an account and an item, a record of nothing when
// there is none or it cannot be read.
func (l *Listeners) Get(user, id string) Record {
	l.mu.RLock()
	records, ok := l.records[user]
	l.mu.RUnlock()
	if ok {
		return records[id]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	records, err := l.load(user)
	if err != nil {
		l.log.Warn("jellyfin cannot read what an account played from the persistent data file",
			"user", user, "error", err)
	}
	return records[id]
}

// Data is what is kept of an account and an item, for Describer and Query:
// the record of the item, or for an album what its tracks have.
func (l *Listeners) Data(user string) func(Item) Record {
	return func(item Item) Record {
		album, ok := item.(*Album)
		if !ok {
			return l.Get(user, item.ItemID())
		}
		r := Record{Favorite: l.Get(user, album.ID).Favorite, Played: len(album.Tracks) > 0}
		for _, t := range album.Tracks {
			tr := l.Get(user, t.ID)
			r.Played = r.Played && tr.Played
			r.PlayCount += tr.PlayCount
			if tr.LastPlayed.After(r.LastPlayed) {
				r.LastPlayed = tr.LastPlayed
			}
		}
		return r
	}
}

// Update changes the record of an account and an item, and keeps it.
func (l *Listeners) Update(user, id string, change func(*Record)) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.update(user, []string{id}, change); err != nil {
		return Record{}, err
	}
	return l.records[user][id], nil
}

// MarkPlayed marks tracks played, as a client does that played them without
// reporting it. A client that says when counts a play of each, and its date
// is when they were played last; otherwise they are played but no play is
// counted, as Jellyfin marks them.
func (l *Listeners) MarkPlayed(user string, tracks []*Track, date time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, len(tracks))
	for i, t := range tracks {
		ids[i] = t.ID
	}
	now := l.now()
	err := l.update(user, ids, func(r *Record) {
		r.Played = true
		if !date.IsZero() {
			r.PlayCount++
			r.LastPlayed = date
		} else if r.LastPlayed.IsZero() {
			r.LastPlayed = now
		}
	})
	if err != nil || date.IsZero() {
		return err
	}
	for _, t := range tracks {
		if err := l.remember(user, Play{At: date}, t); err != nil {
			return err
		}
	}
	return nil
}

// MarkUnplayed takes back that tracks were played, how often and where they
// are resumed from, as Jellyfin does. Their history stays.
func (l *Listeners) MarkUnplayed(user string, tracks []*Track) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, len(tracks))
	for i, t := range tracks {
		ids[i] = t.ID
	}
	return l.update(user, ids, func(r *Record) {
		r.Played, r.PlayCount, r.LastPlayed, r.Position = false, 0, time.Time{}, 0
	})
}

// Started reports that a client began to play a track. A play the device
// had going is over.
func (l *Listeners) Started(r Report) error {
	return l.report(r, true, false)
}

// Progress reports where a client is in the track it plays. One that was
// not reported as started is taken to be, so that a play missed at its start
// or across a restart still counts.
func (l *Listeners) Progress(r Report) error {
	return l.report(r, false, false)
}

// Stopped reports that a client stopped playing a track, where it is resumed
// from next time.
func (l *Listeners) Stopped(r Report) error {
	return l.report(r, false, true)
}

func (l *Listeners) report(r Report, start, stop bool) error {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var errs []error
	for key, p := range l.plays {
		if now.Sub(p.seen) > playIdle {
			delete(l.plays, key)
			errs = append(errs, l.count(key, p, p.seen))
		}
	}
	key := r.User + "\x00" + cmp.Or(r.Device, r.Client)
	p := l.plays[key]
	if p != nil && (start || p.track.ID != r.Track.ID) {
		delete(l.plays, key)
		errs = append(errs, l.count(key, p, now))
		p = nil
	}
	if p == nil {
		p = &play{track: r.Track, client: r.Client, device: r.Device, started: now}
		l.plays[key] = p
	}
	p.seen = now
	// a client that starts at 0 says nothing yet of how far it gets
	if r.HasPosition && (!start || r.Position > 0) {
		p.positioned = true
		p.heard = max(p.heard, r.Position)
	}
	if stop {
		delete(l.plays, key)
		errs = append(errs, l.count(key, p, now), l.resume(r, p))
	} else {
		errs = append(errs, l.count(key, p, now))
	}
	return errors.Join(errs...)
}

// count counts a play, once, when it was heard enough by now: as it goes on,
// or when it is over, by when it was last reported.
func (l *Listeners) count(key string, p *play, now time.Time) error {
	if p.counted {
		return nil
	}
	heard := p.heard
	if !p.positioned {
		heard = now.Sub(p.started)
	}
	runtime := p.track.Duration
	if runtime > 0 {
		heard = min(heard, runtime)
	}
	needed := countAfter
	if runtime > 0 {
		// to the tick, the unit a client says where it is in
		needed = min(needed, (runtime / 2).Truncate(100))
	}
	if heard < needed {
		return nil
	}
	p.counted = true
	user, _, _ := strings.Cut(key, "\x00")
	err := l.update(user, []string{p.track.ID}, func(r *Record) {
		r.Played = true
		r.PlayCount++
		r.LastPlayed = now
	})
	if err != nil {
		return err
	}
	return l.remember(user, Play{At: now, Heard: heard, Client: p.client, Device: p.device}, p.track)
}

// resume keeps where a stopped track is resumed from, or forgets it.
func (l *Listeners) resume(r Report, p *play) error {
	position := p.heard
	if r.HasPosition {
		position = r.Position
	}
	runtime := r.Track.Duration
	ticks := ticks(position)
	if runtime < resumeMinimum || position*100 < runtime*resumeFrom || position*100 > runtime*resumeUntil {
		ticks = 0
	}
	if l.cached(r.User)[r.Track.ID].Position == ticks {
		return nil
	}
	return l.update(r.User, []string{r.Track.ID}, func(rec *Record) { rec.Position = ticks })
}

// History calls fn for the plays of an account since a time, the first
// first.
func (l *Listeners) History(user string, since time.Time, fn func(Play) error) error {
	prefix := user + "\x00"
	from := prefix + since.UTC().Format(historyTime)
	return l.history.Scan(prefix, func(key string, decode func(any) error) error {
		if key < from {
			return nil
		}
		var p Play
		if err := decode(&p); err != nil {
			return err
		}
		return fn(p)
	})
}

// remember adds a play of a track to the history of an account.
func (l *Listeners) remember(user string, p Play, t *Track) error {
	p.Item, p.Library, p.Href, p.Track = t.ID, t.lib.Name, t.Href, t.Name
	p.Album, p.Artist, p.Runtime = t.Album.Name, t.Artist.Name, t.Duration
	key := user + "\x00" + p.At.UTC().Format(historyTime) + "\x00" + t.ID
	// written at once rather than batched, which would wait for other
	// writes that cannot come while l.mu is held
	return l.history.Write(func(w *store.Writer) error { return w.Put(key, p) })
}

// update changes the records of an account and some items, and keeps them
// in one transaction. l.mu is held.
func (l *Listeners) update(user string, ids []string, change func(*Record)) error {
	records, err := l.load(user)
	if err != nil {
		return err
	}
	changed := make(map[string]Record, len(ids))
	for _, id := range ids {
		r := records[id]
		change(&r)
		changed[id] = r
	}
	err = l.data.Write(func(w *store.Writer) error {
		for id, r := range changed {
			key := user + "\x00" + id
			var err error
			if r == (Record{}) {
				err = w.Delete(key)
			} else {
				err = w.Put(key, r)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id, r := range changed {
		if r == (Record{}) {
			delete(records, id)
		} else {
			records[id] = r
		}
	}
	return nil
}

// load reads the records of an account the first time. l.mu is held.
func (l *Listeners) load(user string) (map[string]Record, error) {
	if records, ok := l.records[user]; ok {
		return records, nil
	}
	records := map[string]Record{}
	prefix := user + "\x00"
	err := l.data.Scan(prefix, func(key string, decode func(any) error) error {
		var r Record
		if err := decode(&r); err != nil {
			return err
		}
		records[strings.TrimPrefix(key, prefix)] = r
		return nil
	})
	if err != nil {
		// not kept, so that it is read again next time
		return records, fmt.Errorf("%s: %w", userDataBucket, err)
	}
	l.records[user] = records
	return records, nil
}

// cached is the records of an account as they were read. l.mu is held.
func (l *Listeners) cached(user string) map[string]Record {
	records, _ := l.load(user)
	return records
}

// TracksOf are the tracks an item holds: itself, for a track.
func TracksOf(item Item) []*Track {
	switch v := item.(type) {
	case *Track:
		return []*Track{v}
	case *Album:
		return v.Tracks
	case *Artist:
		return v.Tracks
	case *Genre:
		return v.Tracks
	case *Library:
		return v.Tracks
	}
	return nil
}
