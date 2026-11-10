package subsonic

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/jellyfin"
)

var built = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// library is a library of three albums, of three years and two genres, with
// an artist whose name sorts by no letter.
func library() *jellyfin.Library {
	source := func(href, title, artist, album, genre string, year, number int, added time.Time) jellyfin.Source {
		return jellyfin.Source{Href: href, Size: 1000, ModTime: added, Track: audio.Track{Title: title,
			Artist: artist, AlbumArtist: artist, Album: album, Genre: genre, Year: year, Number: number,
			Format: audio.Format{Container: "mp3", Duration: 3 * time.Minute, Bitrate: 320000}}}
	}
	sources := []jellyfin.Source{
		source("a/1.mp3", "Alpha one", "The Alphas", "Alpha", "Rock", 1990, 1, built.Add(-3*time.Hour)),
		source("a/2.mp3", "Alpha two", "The Alphas", "Alpha", "Rock", 1990, 2, built.Add(-3*time.Hour)),
		source("b/1.mp3", "Beta song", "Betty", "Beta", "Jazz", 2000, 1, built.Add(-time.Hour)),
		source("c/1.mp3", "Gamma song", "42 Singers", "Gamma", "Rock", 2010, 1, built.Add(-2*time.Hour)),
	}
	return jellyfin.Build("music", "Music", "/music", "/srv/music", sources, nil, built)
}

func albumNames(albums []*jellyfin.Album) []string {
	var out []string
	for _, a := range albums {
		out = append(out, a.Name)
	}
	return out
}

func TestMarshal(t *testing.T) {
	r := OK("1.2.3")
	r.License = &License{Valid: true}
	body, media, err := Marshal("", "", r)
	if err != nil || media != "application/xml" {
		t.Fatal(err, media)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<subsonic-response xmlns="http://subsonic.org/restapi" ` +
		`status="ok" version="1.16.1" type="go-fs" serverVersion="1.2.3" openSubsonic="true"><license valid="true">` +
		`</license></subsonic-response>`
	if string(body) != want {
		t.Errorf("XML\n%s\nwant\n%s", body, want)
	}
	body, media, err = Marshal("json", "", Failed("1.2.3", ErrNotFound, "gone"))
	if err != nil || media != "application/json" {
		t.Fatal(err, media)
	}
	want = `{"subsonic-response":{"status":"failed","version":"1.16.1","type":"go-fs","serverVersion":"1.2.3",` +
		`"openSubsonic":true,"error":{"code":70,"message":"gone"}}}`
	if string(body) != want {
		t.Errorf("JSON\n%s\nwant\n%s", body, want)
	}
	body, media, err = Marshal("jsonp", "app.cb1", OK(""))
	if err != nil || media != "application/javascript" || !strings.HasPrefix(string(body), `app.cb1({"subsonic-response":`) ||
		!strings.HasSuffix(string(body), "});") {
		t.Errorf("JSONP %s %s %v", body, media, err)
	}
	for _, bad := range []string{"", "1cb", "a(b)", "x;alert(1)", "a b"} {
		if _, _, err := Marshal("jsonp", bad, OK("")); err == nil {
			t.Errorf("callback %q is accepted", bad)
		}
	}
}

// TestDescribedLists checks the lists OpenSubsonic adds to an item are sent
// empty rather than as null, and empty lists of items are left out.
func TestDescribedLists(t *testing.T) {
	l := library()
	d := Describer{}
	r := OK("")
	song := d.Song(l.Tracks[0])
	r.Song = &song
	r.Starred2 = &Starred2{}
	body, _, err := Marshal("json", "", r)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`"genres":[{"name":"Rock"}]`, `"replayGain":{}`, `"starred2":{}`, `"bitRate":320`,
		`"duration":180`, `"contentType":"audio/mpeg"`, `"mediaType":"song"`} {
		if !strings.Contains(text, want) {
			t.Errorf("%s is not in %s", want, text)
		}
	}
	album := d.Album(l.Albums[0])
	encoded, _ := json.Marshal(album)
	if !strings.Contains(string(encoded), `"releaseTypes":[]`) || !strings.Contains(string(encoded), `"moods":[]`) {
		t.Errorf("album %s", encoded)
	}
}

func TestFindAlbums(t *testing.T) {
	libs := []*jellyfin.Library{library()}
	find := func(q AlbumQuery, data func(jellyfin.Item) jellyfin.Record) []string {
		t.Helper()
		albums, err := FindAlbums(libs, data, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		return albumNames(albums)
	}
	for _, c := range []struct {
		q    AlbumQuery
		want []string
	}{
		{AlbumQuery{Type: "alphabeticalByName"}, []string{"Alpha", "Beta", "Gamma"}},
		// 42 Singers sorts before the letters, The Alphas by A
		{AlbumQuery{Type: "alphabeticalByArtist"}, []string{"Gamma", "Alpha", "Beta"}},
		{AlbumQuery{Type: "newest"}, []string{"Beta", "Gamma", "Alpha"}},
		{AlbumQuery{Type: "byYear", FromYear: 1995, ToYear: 2020, HasYears: true}, []string{"Beta", "Gamma"}},
		{AlbumQuery{Type: "byYear", FromYear: 2020, ToYear: 1980, HasYears: true}, []string{"Gamma", "Beta", "Alpha"}},
		{AlbumQuery{Type: "byGenre", Genre: "rock"}, []string{"Alpha", "Gamma"}},
		{AlbumQuery{Type: "alphabeticalByName", Page: Page{Offset: 1, Size: 1}}, []string{"Beta"}},
		{AlbumQuery{Type: "alphabeticalByName", Page: Page{Offset: 5}}, nil},
		{AlbumQuery{Type: "frequent"}, nil},
	} {
		if got := find(c.q, nil); !slices.Equal(got, c.want) {
			t.Errorf("%+v: got %q, want %q", c.q, got, c.want)
		}
	}
	if got := find(AlbumQuery{Type: "random", Page: Page{Size: 2}}, nil); len(got) != 2 {
		t.Errorf("random %q", got)
	}

	// the lists of what was played read the records
	records := map[string]jellyfin.Record{}
	data := func(item jellyfin.Item) jellyfin.Record { return records[item.ItemName()] }
	records["Beta"] = jellyfin.Record{PlayCount: 1, LastPlayed: built, Favorite: true, FavoriteAt: built}
	records["Gamma"] = jellyfin.Record{PlayCount: 5, LastPlayed: built.Add(-time.Hour), Favorite: true,
		FavoriteAt: built.Add(time.Hour)}
	for typ, want := range map[string][]string{
		"frequent": {"Gamma", "Beta"}, "highest": {"Gamma", "Beta"}, "recent": {"Beta", "Gamma"},
		"starred": {"Gamma", "Beta"},
	} {
		if got := find(AlbumQuery{Type: typ}, data); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", typ, got, want)
		}
	}

	for _, q := range []AlbumQuery{{}, {Type: "byYear"}, {Type: "byGenre"}, {Type: "sideways"}} {
		if _, err := FindAlbums(libs, nil, q); err == nil {
			t.Errorf("%+v is answered", q)
		}
	}
	if _, err := FindAlbums(libs, nil, AlbumQuery{}); err.(*Problem).Code != ErrMissing {
		t.Errorf("no type: %v", err)
	}
}

func TestSearch(t *testing.T) {
	libs := []*jellyfin.Library{library()}
	everything := SearchQuery{Query: `""`, Artists: Page{Size: -1}, Albums: Page{Size: -1}, Songs: Page{Size: -1}}
	artists, albums, tracks := Search(libs, everything)
	if len(artists) != 3 || len(albums) != 3 || len(tracks) != 4 {
		t.Fatalf("everything is %d, %d, %d", len(artists), len(albums), len(tracks))
	}
	// page by page, the same order every time
	var paged []*jellyfin.Track
	for offset := 0; offset < 6; offset += 3 {
		_, _, page := Search(libs, SearchQuery{Songs: Page{Offset: offset, Size: 3}})
		paged = append(paged, page...)
	}
	if !slices.Equal(paged, tracks) {
		t.Error("the pages are not everything in order")
	}
	_, albums, tracks = Search(libs, SearchQuery{Query: "ALPHA two", Albums: Page{Size: -1}, Songs: Page{Size: -1}})
	if len(albums) != 0 || len(tracks) != 1 || tracks[0].Name != "Alpha two" {
		t.Errorf("alpha two: %q, %d tracks", albumNames(albums), len(tracks))
	}
	// an album is found by its artist, a count of 0 finds none
	artists, albums, _ = Search(libs, SearchQuery{Query: "betty", Albums: Page{Size: -1}})
	if len(artists) != 0 || !slices.Equal(albumNames(albums), []string{"Beta"}) {
		t.Errorf("betty: %d artists, %q", len(artists), albumNames(albums))
	}
}

func TestArtistIndex(t *testing.T) {
	index := ArtistIndex([]*jellyfin.Library{library()}, Describer{})
	var letters []string
	for _, i := range index {
		letters = append(letters, i.Name)
	}
	if !slices.Equal(letters, []string{"A", "B", "#"}) || index[0].Artist[0].Name != "The Alphas" ||
		index[2].Artist[0].Name != "42 Singers" {
		t.Errorf("index %+v", index)
	}
	genres := AllGenres([]*jellyfin.Library{library()})
	if len(genres) != 2 || genres[1].Value != "Rock" || genres[1].SongCount != 3 || genres[1].AlbumCount != 2 {
		t.Errorf("genres %+v", genres)
	}
}

func TestFolderIDIsStable(t *testing.T) {
	l := library()
	if FolderID(l) != FolderID(library()) || FolderID(l) < 0 {
		t.Errorf("folder id %d", FolderID(l))
	}
}
