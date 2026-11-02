package jellyfin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
)

var day = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func source(href string, track audio.Track, added int) Source {
	if track.Duration == 0 {
		track.Duration = 3 * time.Minute
	}
	track.Container, track.Codec = "flac", "flac"
	return Source{Href: href, Size: 1000, ModTime: day.Add(time.Duration(added) * time.Hour), Track: track}
}

// library is a library of two albums of one band, a compilation without
// album artists in a folder of its own, and an untagged file named after its
// folders.
func library(t *testing.T) *Library {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Band", "First"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Band", "First", "Folder.JPG"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Band", "First", "back.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []Source{
		source("Band/First/02.flac", audio.Track{Title: "Two", Artist: "The Band", AlbumArtist: "The Band",
			Album: "First", Number: 2, Year: 2001, Genre: "Rock"}, 1),
		source("Band/First/01.flac", audio.Track{Title: "One", Artist: "The Band", AlbumArtist: "The Band",
			Album: "First", Number: 1, Year: 2001, Genre: "Rock"}, 2),
		source("Band/Second/01.flac", audio.Track{Title: "Again", Artist: "The Band", Album: "Second",
			Year: 2005, Genre: "rock", Picture: true}, 3),
		source("Mix/a.flac", audio.Track{Title: "A", Artist: "Alpha", Album: "Mix", Genre: "Pop"}, 4),
		source("Mix/b.flac", audio.Track{Title: "B", Artist: "Beta", Album: "Mix"}, 5),
		source("Someone/Untagged/03 - Found.flac", audio.Track{}, 6),
	}
	hrefs := make([]string, len(sources))
	for i, s := range sources {
		hrefs[i] = s.Href
	}
	return Build("music", "", "/music", dir, sources, Covers(dir, Folders(hrefs)), day)
}

func names[T Item](items []T) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ItemName()
	}
	return out
}

func TestBuildsAlbumsArtistsAndGenres(t *testing.T) {
	l := library(t)
	if l.Title != "music" || l.ID != newID("music") {
		t.Errorf("library %q %q", l.Title, l.ID)
	}
	if got := names(l.Albums); !slices.Equal(got, []string{"First", "Mix", "Second", "Untagged"}) {
		t.Errorf("albums %q", got)
	}
	if got := names(l.Artists); !slices.Equal(got, []string{"Alpha", "The Band", "Beta", "Someone", "Various Artists"}) {
		t.Errorf("artists %q", got)
	}
	// genres are one whatever their case, named as they were first found
	if got := names(l.Genres); !slices.Equal(got, []string{"Pop", "Rock"}) {
		t.Errorf("genres %q", got)
	}
	first := l.Albums[0]
	if got := names(first.Tracks); !slices.Equal(got, []string{"One", "Two"}) || first.Artist.Name != "The Band" ||
		first.Year != 2001 || first.Duration != 6*time.Minute || !first.Added.Equal(day.Add(2*time.Hour)) {
		t.Errorf("first album %q by %q, %d, %v, %v", got, first.Artist.Name, first.Year, first.Duration, first.Added)
	}
	mix := l.Albums[1]
	if mix.Artist.Name != "Various Artists" || len(mix.Tracks) != 2 {
		t.Errorf("the compilation is by %q with %d tracks", mix.Artist.Name, len(mix.Tracks))
	}
	untagged := l.Albums[3]
	if tr := untagged.Tracks[0]; tr.Name != "Found" || tr.Number != 3 || tr.Artist.Name != "Someone" {
		t.Errorf("the untagged track is %q, %d, by %q", tr.Name, tr.Number, tr.Artist.Name)
	}
	band := l.Item(newID("music", "artist", "the band")).(*Artist)
	if got := names(band.Albums); !slices.Equal(got, []string{"First", "Second"}) || len(band.Tracks) != 3 {
		t.Errorf("the band has albums %q and %d tracks", got, len(band.Tracks))
	}
	if got := l.Tracks[0].Virtual(); got != "/music/Band/First/01.flac" {
		t.Errorf("virtual path %q", got)
	}
}

func TestIDsStayTheSame(t *testing.T) {
	a, b := library(t), library(t)
	for i := range a.Tracks {
		if a.Tracks[i].ID != b.Tracks[i].ID {
			t.Fatal("a track has another id in another build")
		}
	}
	for i := range a.Albums {
		if a.Albums[i].ID != b.Albums[i].ID || len(a.Albums[i].ID) != 32 {
			t.Fatal("an album has another id in another build")
		}
	}
}

func TestCovers(t *testing.T) {
	l := library(t)
	file, track, ok := CoverOf(l.Albums[0])
	if !ok || filepath.Base(file) != "Folder.JPG" || track != nil {
		t.Errorf("the cover beside the tracks: %q %v %v", file, track, ok)
	}
	second := l.Albums[2]
	if file, track, ok := CoverOf(second.Tracks[0]); !ok || file != "" || track != second.Tracks[0] {
		t.Errorf("the picture of a track: %q %v %v", file, track, ok)
	}
	if _, _, ok := CoverOf(l.Albums[1]); ok {
		t.Error("an album without a picture has a cover")
	}
	band := l.Item(newID("music", "artist", "the band"))
	if imageTag(band) != l.Albums[0].coverTag || imageTag(band) == "" {
		t.Error("an artist has not the cover of their first album")
	}
	if !strings.Contains(Listing(l.covers), "Band/First/Folder.JPG") {
		t.Errorf("listing %q", Listing(l.covers))
	}
}

func find(t *testing.T, l *Library, q Query) []string {
	t.Helper()
	items, total := Find([]*Library{l}, q)
	if q.Limit == 0 && q.Start == 0 && total != len(items) {
		t.Errorf("total %d for %d items", total, len(items))
	}
	return names(items)
}

func TestFind(t *testing.T) {
	l := library(t)
	band := newID("music", "artist", "the band")
	rock := newID("music", "genre", "rock")
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"the libraries", Query{}, []string{"music"}},
		{"a library", Query{ParentID: l.ID}, []string{"First", "Mix", "Second", "Untagged"}},
		{"every track", Query{Types: []string{"Audio"}, Recursive: true},
			[]string{"One", "Two", "A", "B", "Again", "Found"}},
		{"an album", Query{ParentID: l.Albums[0].ID}, []string{"One", "Two"}},
		{"an artist", Query{ParentID: band}, []string{"First", "Second"}},
		{"the albums of an artist", Query{Types: []string{"MusicAlbum"}, Recursive: true, AlbumArtistIDs: []string{band}},
			[]string{"First", "Second"}},
		{"the tracks of an artist", Query{Types: []string{"Audio"}, Recursive: true, ArtistIDs: []string{band}},
			[]string{"One", "Two", "Again"}},
		{"a genre", Query{Types: []string{"MusicAlbum"}, Recursive: true, GenreIDs: []string{rock}},
			[]string{"First", "Second"}},
		{"a genre by name", Query{Types: []string{"Audio"}, Recursive: true, Genres: []string{"pop"}}, []string{"A"}},
		{"a year", Query{Types: []string{"MusicAlbum"}, Recursive: true, Years: []int{2005}}, []string{"Second"}},
		{"a search", Query{Types: []string{"Audio", "MusicAlbum"}, Recursive: true, Search: "ON"},
			[]string{"Second", "One"}},
		{"by a letter", Query{Types: []string{"MusicArtist"}, Recursive: true, NameStartsWith: "b"},
			[]string{"The Band", "Beta"}},
		{"newest first", Query{Types: []string{"MusicAlbum"}, Recursive: true, SortBy: []string{"DateCreated"},
			Descending: []bool{true}}, []string{"Untagged", "Mix", "Second", "First"}},
		{"by year then name", Query{Types: []string{"MusicAlbum"}, Recursive: true,
			SortBy: []string{"ProductionYear", "SortName"}}, []string{"Mix", "Untagged", "First", "Second"}},
		{"a page", Query{ParentID: l.ID, Start: 1, Limit: 2}, []string{"Mix", "Second"}},
		{"past the end", Query{ParentID: l.ID, Start: 10}, []string{}},
		{"some ids", Query{IDs: []string{l.Albums[2].ID, l.Tracks[0].ID}}, []string{"Second", "One"}},
		{"favorites", Query{Types: []string{"Audio"}, Recursive: true, Favorite: &yes}, []string{}},
		{"an unknown parent", Query{ParentID: "nothing"}, []string{}},
	}
	for _, c := range cases {
		if got := find(t, l, c.q); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if _, total := Find([]*Library{l}, Query{ParentID: l.ID, Limit: 1}); total != 4 {
		t.Errorf("the total of a page is %d", total)
	}
}

func TestAnAccountSeesWhatItMayRead(t *testing.T) {
	l := library(t)
	band := func(virtual string) bool { return strings.HasPrefix(virtual, "/music/Band/") }
	view := l.For("band", band)
	if got := names(view.Albums); !slices.Equal(got, []string{"First", "Second"}) {
		t.Errorf("albums %q", got)
	}
	if view.Albums[0].ID != l.Albums[0].ID {
		t.Error("an album has another id in a view")
	}
	if l.For("band", func(string) bool { return false }) != view {
		t.Error("a view is worked out again for the same key")
	}
	if l.For("all", func(string) bool { return true }) != l {
		t.Error("an account that reads everything does not see the library itself")
	}
}

func TestDescribes(t *testing.T) {
	l := library(t)
	d := Describer{ServerID: "server"}
	track := d.Item(l.Tracks[0])
	if track.Type != "Audio" || track.IsFolder || track.AlbumID != l.Albums[0].ID || track.IndexNumber != 1 ||
		track.RunTimeTicks != int64(3*time.Minute/100) || track.ImageTags["Primary"] == "" ||
		track.AlbumImageTag == "" || len(track.MediaSources) != 1 || !track.MediaSources[0].SupportsDirectPlay {
		t.Errorf("track %+v", track)
	}
	album := d.Item(l.Albums[1])
	if album.AlbumArtist != "Various Artists" || !slices.Equal(album.Artists, []string{"Alpha", "Beta"}) ||
		album.ChildCount != 2 || len(album.ImageTags) != 0 {
		t.Errorf("album %+v", album)
	}
	lib := d.Item(l)
	if lib.Type != "CollectionFolder" || lib.CollectionType != "music" || lib.ChildCount != 4 {
		t.Errorf("library %+v", lib)
	}
	encoded, err := json.Marshal(d.Item(l.Artists[0]))
	if err != nil {
		t.Fatal(err)
	}
	// clients written against the schema expect the lists even when empty
	if !strings.Contains(string(encoded), `"Genres":[]`) || !strings.Contains(string(encoded), `"ServerId":"server"`) {
		t.Errorf("artist %s", encoded)
	}
}

func TestPublish(t *testing.T) {
	l := library(t)
	Publish(l)
	defer Retire("music")
	if got := Libraries(); len(got) != 1 || got[0] != l {
		t.Fatalf("published %v", got)
	}
	if Lookup(Libraries(), l.Tracks[0].ID) != l.Tracks[0] {
		t.Error("a track is not found by its id")
	}
	Retire("music")
	if len(Libraries()) != 0 {
		t.Error("a retired library is still published")
	}
}

func TestNamesFromFiles(t *testing.T) {
	for name, want := range map[string]struct {
		title  string
		number int
	}{
		"01 One.mp3":    {"One", 1},
		"02 - Two.flac": {"Two", 2},
		"3.Three.ogg":   {"Three", 3},
		"2Pac Song.mp3": {"2Pac Song", 0},
		"1999.mp3":      {"1999", 0},
		"Plain.mp3":     {"Plain", 0},
	} {
		title, number := fromFileName(name, 0)
		if title != want.title || number != want.number {
			t.Errorf("%s: %q %d", name, title, number)
		}
	}
}
