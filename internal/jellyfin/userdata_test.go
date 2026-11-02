package jellyfin

import (
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/store"
)

var yes, no = true, false

// playlist is a library of a song of three minutes, one of ten and a mix of
// twenty, the first two on one album.
func playlist() *Library {
	sources := []Source{
		source("Band/Album/01.flac", audio.Track{Title: "Song", Artist: "Band", AlbumArtist: "Band",
			Album: "Album", Number: 1}, 1),
		source("Band/Album/02.flac", audio.Track{Title: "Long", Artist: "Band", AlbumArtist: "Band",
			Album: "Album", Number: 2, Format: audio.Format{Duration: 10 * time.Minute}}, 2),
		source("Mixes/mix.flac", audio.Track{Title: "Mix", Artist: "DJ", Album: "Mixes",
			Format: audio.Format{Duration: 20 * time.Minute}}, 3),
	}
	return Build("music", "", "/music", "/nowhere", sources, nil, day)
}

func track(l *Library, name string) *Track {
	for _, t := range l.Tracks {
		if t.Name == name {
			return t
		}
	}
	panic(name)
}

// clock is a time the tests move on by hand.
type clock struct{ now time.Time }

func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }

// newListeners keeps what the listeners do in st, or in a store of its own
// for nil.
func newListeners(t *testing.T, st *store.Store) (*Listeners, *clock) {
	t.Helper()
	if st == nil {
		st = openStore(t, filepath.Join(t.TempDir(), "go-fs.db"))
		t.Cleanup(func() { _ = st.Close() })
	}
	l := NewListeners(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &clock{now: day}
	l.now = func() time.Time { return c.now }
	return l, c
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func at(user string, t *Track, position time.Duration) Report {
	return Report{User: user, Device: "phone", Client: "Test", Track: t, Position: position, HasPosition: true}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func plays(t *testing.T, l *Listeners, user string) []string {
	t.Helper()
	var out []string
	must(t, l.History(user, time.Time{}, func(p Play) error {
		out = append(out, p.Track)
		return nil
	}))
	return out
}

func TestAPlayCountsOnceHalfIsHeard(t *testing.T) {
	lib := playlist()
	song := track(lib, "Song")
	l, c := newListeners(t, nil)
	must(t, l.Started(at("john", song, 0)))
	c.add(80 * time.Second)
	must(t, l.Progress(at("john", song, 80*time.Second)))
	if r := l.Get("john", song.ID); r.PlayCount != 0 || r.Played {
		t.Fatalf("counted before half was heard: %+v", r)
	}
	c.add(10 * time.Second)
	must(t, l.Progress(at("john", song, 90*time.Second)))
	if r := l.Get("john", song.ID); r.PlayCount != 1 || !r.Played || !r.LastPlayed.Equal(c.now) {
		t.Fatalf("half of the song heard: %+v", r)
	}
	c.add(time.Minute)
	must(t, l.Stopped(at("john", song, 150*time.Second)))
	if r := l.Get("john", song.ID); r.PlayCount != 1 || r.Position != 0 {
		t.Errorf("stopped after it counted: %+v", r)
	}
	if got := plays(t, l, "john"); !slices.Equal(got, []string{"Song"}) {
		t.Errorf("history %q", got)
	}
	if r := l.Get("anna", song.ID); r != (Record{}) {
		t.Errorf("another account has %+v", r)
	}
}

func TestALongTrackCountsAfterFourMinutes(t *testing.T) {
	long := track(playlist(), "Long")
	l, _ := newListeners(t, nil)
	must(t, l.Progress(at("john", long, 4*time.Minute-time.Second)))
	if r := l.Get("john", long.ID); r.PlayCount != 0 {
		t.Fatalf("counted at 3:59: %+v", r)
	}
	// seeking back is not heard twice
	must(t, l.Progress(at("john", long, time.Minute)))
	must(t, l.Progress(at("john", long, 4*time.Minute)))
	if r := l.Get("john", long.ID); r.PlayCount != 1 {
		t.Errorf("not counted at 4:00: %+v", r)
	}
}

func TestAPlayWithoutPositionsGoesByTheClock(t *testing.T) {
	lib := playlist()
	song, long := track(lib, "Song"), track(lib, "Long")
	l, c := newListeners(t, nil)
	silent := func(t *Track) Report { return Report{User: "john", Device: "phone", Track: t} }

	// skipped after ten seconds by starting another
	must(t, l.Started(silent(song)))
	c.add(10 * time.Second)
	must(t, l.Started(silent(long)))
	if r := l.Get("john", song.ID); r.PlayCount != 0 {
		t.Errorf("a skipped song counted: %+v", r)
	}
	// played on for five minutes, then the next one started without a stop
	c.add(5 * time.Minute)
	must(t, l.Started(silent(song)))
	if r := l.Get("john", long.ID); r.PlayCount != 1 {
		t.Errorf("a play over without a stop did not count: %+v", r)
	}
	// a play left without a word is given up by when it was last reported
	c.add(2 * playIdle)
	must(t, l.Started(Report{User: "anna", Device: "desk", Track: long}))
	if r := l.Get("john", song.ID); r.PlayCount != 0 {
		t.Errorf("an abandoned play counted: %+v", r)
	}
}

func TestResumePositions(t *testing.T) {
	lib := playlist()
	song, mix := track(lib, "Song"), track(lib, "Mix")
	l, _ := newListeners(t, nil)
	cases := []struct {
		track    *Track
		stopped  time.Duration
		position int64
	}{
		{mix, 8 * time.Minute, ticks(8 * time.Minute)},
		{mix, 30 * time.Second, 0},
		{mix, 19 * time.Minute, 0},
		{song, time.Minute, 0},
	}
	for _, c := range cases {
		must(t, l.Started(at("john", c.track, 0)))
		must(t, l.Stopped(at("john", c.track, c.stopped)))
		if r := l.Get("john", c.track.ID); r.Position != c.position {
			t.Errorf("%s stopped at %s is resumed from %d, want %d", c.track.Name, c.stopped, r.Position, c.position)
		}
	}
}

func TestMarkPlayed(t *testing.T) {
	lib := playlist()
	album := lib.Albums[0]
	l, c := newListeners(t, nil)
	must(t, l.MarkPlayed("john", TracksOf(album), time.Time{}))
	data := l.Data("john")
	if r := data(album); !r.Played || r.PlayCount != 0 || !r.LastPlayed.Equal(c.now) {
		t.Errorf("an album marked played: %+v", r)
	}
	when := day.Add(-time.Hour)
	must(t, l.MarkPlayed("john", TracksOf(album), when))
	if r := data(album); r.PlayCount != 2 || !r.LastPlayed.Equal(when) {
		t.Errorf("an album marked played on a date: %+v", r)
	}
	if got := plays(t, l, "john"); len(got) != 2 {
		t.Errorf("history %q", got)
	}
	must(t, l.MarkUnplayed("john", TracksOf(album)[:1]))
	if r := data(album); r.Played || r.PlayCount != 1 {
		t.Errorf("an album with a track marked unplayed: %+v", r)
	}
}

func TestRecordsAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go-fs.db")
	song := track(playlist(), "Song")
	st := openStore(t, path)
	l, _ := newListeners(t, st)
	must(t, l.Progress(at("john", song, 2*time.Minute)))
	if _, err := l.Update("john", song.ID, func(r *Record) { r.Favorite = true }); err != nil {
		t.Fatal(err)
	}
	must(t, st.Close())

	st = openStore(t, path)
	defer func() { _ = st.Close() }()
	l, _ = newListeners(t, st)
	if r := l.Get("john", song.ID); r.PlayCount != 1 || !r.Favorite || !r.LastPlayed.Equal(day) {
		t.Errorf("after a restart: %+v", r)
	}
	if got := plays(t, l, "john"); !slices.Equal(got, []string{"Song"}) {
		t.Errorf("history after a restart %q", got)
	}
	// a record of nothing is not kept
	if _, err := l.Update("john", song.ID, func(r *Record) { *r = Record{} }); err != nil {
		t.Fatal(err)
	}
	found, err := st.Bucket(userDataBucket).Get("john\x00"+song.ID, &Record{})
	if err != nil || found {
		t.Errorf("an empty record is kept: %v %v", found, err)
	}
}

func TestQueriesReadTheRecords(t *testing.T) {
	lib := playlist()
	song, long, mix := track(lib, "Song"), track(lib, "Long"), track(lib, "Mix")
	l, c := newListeners(t, nil)
	must(t, l.Progress(at("john", mix, 5*time.Minute)))
	c.add(time.Hour)
	for range 2 {
		must(t, l.Started(at("john", song, 0)))
		must(t, l.Progress(at("john", song, 2*time.Minute)))
	}
	c.add(time.Hour)
	// too little to count, enough to resume from
	must(t, l.Started(at("john", mix, 0)))
	must(t, l.Stopped(at("john", mix, 3*time.Minute)))
	if _, err := l.Update("john", long.ID, func(r *Record) { r.Favorite = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Update("john", lib.Albums[0].ID, func(r *Record) { r.Favorite = true }); err != nil {
		t.Fatal(err)
	}

	data := l.Data("john")
	tracks := Query{Types: []string{"Audio"}, Recursive: true, Data: data}
	with := func(change func(*Query)) Query {
		q := tracks
		change(&q)
		return q
	}
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"favorites", with(func(q *Query) { q.Favorite = &yes }), []string{"Long"}},
		{"favorite albums", Query{Types: []string{"MusicAlbum"}, Recursive: true, Data: data, Favorite: &yes},
			[]string{"Album"}},
		{"played", with(func(q *Query) { q.Played = &yes }), []string{"Song", "Mix"}},
		{"unplayed", with(func(q *Query) { q.Played = &no }), []string{"Long"}},
		{"resumable", with(func(q *Query) { q.Resumable = true }), []string{"Mix"}},
		{"played last first", with(func(q *Query) {
			q.Played, q.SortBy, q.Descending = &yes, []string{"DatePlayed"}, []bool{true}
		}), []string{"Song", "Mix"}},
		{"played most first", with(func(q *Query) {
			q.SortBy, q.Descending = []string{"PlayCount", "SortName"}, []bool{true, false}
		}), []string{"Song", "Mix", "Long"}},
		{"nothing kept", Query{Types: []string{"Audio"}, Recursive: true, Played: &yes}, []string{}},
	}
	for _, c := range cases {
		if got := find(t, lib, c.q); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	d := Describer{ServerID: "server", Data: data}
	album := d.Item(lib.Albums[0]).UserData
	if album.Played || album.PlayCount != 2 || !album.IsFavorite || album.UnplayedItemCount == nil ||
		*album.UnplayedItemCount != 1 || album.LastPlayedDate == "" {
		t.Errorf("album %+v", album)
	}
	if u := d.Item(mix).UserData; u.PlaybackPositionTicks != ticks(3*time.Minute) || u.PlayedPercentage != 15 {
		t.Errorf("mix %+v", u)
	}
}

func TestWithoutAStoreRecordsLastTheRun(t *testing.T) {
	song := track(playlist(), "Song")
	l := NewListeners(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, l.Progress(at("john", song, 2*time.Minute)))
	if r := l.Get("john", song.ID); r.PlayCount != 1 {
		t.Errorf("without a store: %+v", r)
	}
	if got := plays(t, l, "john"); len(got) != 0 {
		t.Errorf("history without a store %q", got)
	}
}
