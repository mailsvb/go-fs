package jellyfin

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
)

// Query is what a client asks /Items and the lists like it for, as its
// parameters name it.
type Query struct {
	// ParentID is the item whose children are asked for: a library, an
	// album, an artist or a genre; "" for the libraries themselves, or
	// everything in them with Recursive or Types.
	ParentID string
	// Types are the types of item asked for, as Jellyfin names them, such as
	// MusicAlbum and Audio; none is what the parent has.
	Types     []string
	Recursive bool
	// IDs asks for these items, whatever else is asked.
	IDs []string

	Search         string
	NameStartsWith string
	ArtistIDs      []string
	AlbumArtistIDs []string
	AlbumIDs       []string
	GenreIDs       []string
	Genres         []string
	Years          []int
	// AlbumArtists leaves out the artists of no album, who only appear on
	// the albums of others.
	AlbumArtists bool
	// Favorites asks for the favorites only, of which there are none: the
	// library keeps nothing of its listeners.
	Favorites bool

	// SortBy are the keys the items are sorted by, the first first, and
	// Descending says which of them sort the other way; the one before
	// applies to a key it does not reach.
	SortBy     []string
	Descending []bool
	// Start is the first item answered of all that are found, and Limit how
	// many at most, 0 for all.
	Start int
	Limit int
}

// Find answers a query from the libraries an account sees: the page of the
// items asked for, and how many there are in all.
func Find(libs []*Library, q Query) ([]Item, int) {
	items := candidates(libs, q)
	items = slices.DeleteFunc(items, func(item Item) bool { return !q.matches(item) })
	q.sort(items)
	total := len(items)
	start := min(max(q.Start, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return items[start:end], total
}

// Lookup finds an item of the libraries by its id.
func Lookup(libs []*Library, id string) Item {
	for _, l := range libs {
		if item := l.Item(id); item != nil {
			return item
		}
	}
	return nil
}

// Item types by the name Jellyfin gives them, as a query names them.
const (
	typeAlbum  = "musicalbum"
	typeAudio  = "audio"
	typeArtist = "musicartist"
	typeGenre  = "musicgenre"
)

// candidates are the items the query is about, before they are filtered.
func candidates(libs []*Library, q Query) []Item {
	types := map[string]bool{}
	for _, t := range q.Types {
		types[strings.ToLower(t)] = true
	}
	wants := func(t string) bool { return len(types) == 0 || types[t] }

	if len(q.IDs) > 0 {
		var items []Item
		for _, id := range q.IDs {
			if item := Lookup(libs, id); item != nil && (len(types) == 0 ||
				types[strings.ToLower(item.ItemType())]) {
				items = append(items, item)
			}
		}
		return items
	}

	var parent Item
	if q.ParentID != "" {
		if parent = Lookup(libs, q.ParentID); parent == nil {
			return nil
		}
	}
	// what an album, an artist or a genre holds is its albums, or its
	// tracks when those are asked for
	tracksOr := func(albums []*Album, tracks []*Track) []Item {
		if len(types) == 0 {
			if len(albums) == 0 {
				return appendAll(nil, tracks)
			}
			return appendAll(nil, albums)
		}
		var items []Item
		if types[typeAlbum] {
			items = appendAll(items, albums)
		}
		if types[typeAudio] {
			items = appendAll(items, tracks)
		}
		return items
	}
	switch p := parent.(type) {
	case *Album:
		if !wants(typeAudio) {
			return nil
		}
		return appendAll(nil, p.Tracks)
	case *Artist:
		return tracksOr(p.Albums, p.Tracks)
	case *Genre:
		return tracksOr(p.Albums, p.Tracks)
	case *Track:
		return nil
	case *Library:
		libs = []*Library{p}
	case nil:
		if len(types) == 0 && !q.Recursive {
			return appendAll(nil, libs)
		}
	}

	if len(types) == 0 {
		// a library is shown as its albums, which is what its folders
		// are in a music library
		types[typeAlbum] = true
	}
	var items []Item
	for _, l := range libs {
		if types[typeAlbum] {
			items = appendAll(items, l.Albums)
		}
		if types[typeAudio] {
			items = appendAll(items, l.Tracks)
		}
		if types[typeArtist] {
			items = appendAll(items, l.Artists)
		}
		if types[typeGenre] {
			items = appendAll(items, l.Genres)
		}
	}
	return items
}

func appendAll[T Item](items []Item, more []T) []Item {
	for _, item := range more {
		items = append(items, item)
	}
	return items
}

// matches reports whether an item passes the filters of the query.
func (q Query) matches(item Item) bool {
	if q.Favorites {
		return false
	}
	if a, ok := item.(*Artist); ok && q.AlbumArtists && len(a.Albums) == 0 {
		return false
	}
	if q.Search != "" && !strings.Contains(strings.ToLower(item.ItemName()), strings.ToLower(q.Search)) {
		return false
	}
	if q.NameStartsWith != "" && !strings.HasPrefix(sortName(item.ItemName()), strings.ToLower(q.NameStartsWith)) {
		return false
	}
	var artists, albumArtists, albums, genres []Item
	year := 0
	switch v := item.(type) {
	case *Track:
		artists = []Item{v.Artist, v.Album.Artist}
		albumArtists = []Item{v.Album.Artist}
		albums = []Item{v.Album}
		if v.Genre != nil {
			genres = []Item{v.Genre}
		}
		year = v.Year
	case *Album:
		artists = []Item{v.Artist}
		for _, t := range v.Tracks {
			artists = append(artists, t.Artist)
		}
		albumArtists = []Item{v.Artist}
		albums = []Item{v}
		genres = appendAll(nil, v.Genres)
		year = v.Year
	case *Artist:
		artists = []Item{v}
		albumArtists = []Item{v}
	case *Genre:
		genres = []Item{v}
	}
	if len(q.ArtistIDs) > 0 && !anyID(artists, q.ArtistIDs) ||
		len(q.AlbumArtistIDs) > 0 && !anyID(albumArtists, q.AlbumArtistIDs) ||
		len(q.AlbumIDs) > 0 && !anyID(albums, q.AlbumIDs) ||
		len(q.GenreIDs) > 0 && !anyID(genres, q.GenreIDs) {
		return false
	}
	if len(q.Genres) > 0 && !slices.ContainsFunc(genres, func(g Item) bool {
		return slices.ContainsFunc(q.Genres, func(name string) bool { return strings.EqualFold(name, g.ItemName()) })
	}) {
		return false
	}
	if len(q.Years) > 0 && !slices.Contains(q.Years, year) {
		return false
	}
	return true
}

func anyID(items []Item, ids []string) bool {
	for _, item := range items {
		if slices.Contains(ids, item.ItemID()) {
			return true
		}
	}
	return false
}

// sort orders the items by the keys of the query. Without a key they stay
// as they were found, which is by name, and the tracks of an album in their
// order on it.
func (q Query) sort(items []Item) {
	var keys []func(a, b Item) int
	for i, key := range q.SortBy {
		compare := sortKey(strings.ToLower(key))
		if compare == nil {
			continue
		}
		descending := false
		if len(q.Descending) > 0 {
			descending = q.Descending[min(i, len(q.Descending)-1)]
		}
		if strings.EqualFold(key, "Random") {
			rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
			return
		}
		if descending {
			keys = append(keys, func(a, b Item) int { return compare(b, a) })
		} else {
			keys = append(keys, compare)
		}
	}
	if len(keys) == 0 {
		return
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		for _, compare := range keys {
			if c := compare(a, b); c != 0 {
				return c
			}
		}
		return 0
	})
}

// sortKey compares two items by a key of SortBy; nil for a key the library
// has nothing to sort by, such as how often a track was played.
func sortKey(key string) func(a, b Item) int {
	switch key {
	case "sortname", "name":
		return func(a, b Item) int { return strings.Compare(sortName(a.ItemName()), sortName(b.ItemName())) }
	case "album":
		return func(a, b Item) int {
			return cmp.Or(strings.Compare(sortName(albumName(a)), sortName(albumName(b))), position(a, b))
		}
	case "albumartist":
		return func(a, b Item) int { return strings.Compare(sortName(albumArtist(a)), sortName(albumArtist(b))) }
	case "artist":
		return func(a, b Item) int { return strings.Compare(sortName(artistName(a)), sortName(artistName(b))) }
	case "datecreated", "datelastcontentadded":
		return func(a, b Item) int { return added(a).Compare(added(b)) }
	case "productionyear", "premieredate":
		return func(a, b Item) int { return cmp.Compare(yearOf(a), yearOf(b)) }
	case "parentindexnumber", "indexnumber":
		return position
	case "runtime":
		return func(a, b Item) int { return cmp.Compare(durationOf(a), durationOf(b)) }
	case "random":
		return func(Item, Item) int { return 0 }
	}
	return nil
}

func position(a, b Item) int {
	ta, ok1 := a.(*Track)
	tb, ok2 := b.(*Track)
	if !ok1 || !ok2 {
		return 0
	}
	return byPosition(ta, tb)
}
