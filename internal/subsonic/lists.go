package subsonic

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/mailsvb/go-fs/internal/jellyfin"
)

// Problem is a call that cannot be answered, by the code of its error.
type Problem struct {
	Code    int
	Message string
}

func (p *Problem) Error() string { return p.Message }

func missing(name string) *Problem {
	return &Problem{Code: ErrMissing, Message: "required parameter is missing: " + name}
}

// The sizes of the lists the clients page through.
const (
	// DefaultSize is how many items a list has when the client does not say.
	DefaultSize = 10
	// MaxSize is the most a list has, whatever the client asks for.
	MaxSize = 500
	// DefaultSearchSize is how many of each kind a search finds when the
	// client does not say.
	DefaultSearchSize = 20
)

// IgnoredArticles are the articles the names of artists are indexed without,
// as jellyfin.SortName drops them.
const IgnoredArticles = "The A An"

// Page is the part of a list a client asks for.
type Page struct {
	Offset int
	Size   int
}

// pageOf is the page of items a client asks for, of at most MaxSize.
func pageOf[T any](items []T, p Page) []T {
	size := p.Size
	if size <= 0 {
		size = DefaultSize
	}
	size = min(size, MaxSize)
	offset := max(p.Offset, 0)
	if offset >= len(items) {
		return nil
	}
	return items[offset:min(offset+size, len(items))]
}

// AlbumQuery is what getAlbumList and getAlbumList2 are asked for.
type AlbumQuery struct {
	// Type is how the albums are chosen and sorted: random, newest,
	// frequent, recent, starred, highest, alphabeticalByName,
	// alphabeticalByArtist, byYear or byGenre.
	Type string
	Page
	// FromYear and ToYear are the years of byYear, which says HasYears; the
	// albums are sorted the other way when FromYear is the later one.
	FromYear, ToYear int
	HasYears         bool
	// Genre is the genre of byGenre.
	Genre string
}

// FindAlbums finds the albums of the libraries a query asks for.
func FindAlbums(libs []*jellyfin.Library, data func(jellyfin.Item) jellyfin.Record, q AlbumQuery) ([]*jellyfin.Album, error) {
	var albums []*jellyfin.Album
	for _, l := range libs {
		albums = append(albums, l.Albums...)
	}
	rec := func(a *jellyfin.Album) jellyfin.Record {
		if data == nil {
			return jellyfin.Record{}
		}
		return data(a)
	}
	switch q.Type {
	case "":
		return nil, missing("type")
	case "random":
		rand.Shuffle(len(albums), func(i, j int) { albums[i], albums[j] = albums[j], albums[i] })
	case "newest":
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			return cmp.Or(b.Added.Compare(a.Added), byName(a, b))
		})
	case "alphabeticalByName":
		slices.SortStableFunc(albums, byName)
	case "alphabeticalByArtist":
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			return cmp.Or(strings.Compare(jellyfin.SortName(a.Artist.Name), jellyfin.SortName(b.Artist.Name)),
				cmp.Compare(a.Year, b.Year), byName(a, b))
		})
	case "frequent", "highest":
		// nothing is rated, so the highest rated are the most played
		albums = slices.DeleteFunc(albums, func(a *jellyfin.Album) bool { return rec(a).PlayCount == 0 })
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			return cmp.Or(cmp.Compare(rec(b).PlayCount, rec(a).PlayCount), byName(a, b))
		})
	case "recent":
		albums = slices.DeleteFunc(albums, func(a *jellyfin.Album) bool { return rec(a).LastPlayed.IsZero() })
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			return cmp.Or(rec(b).LastPlayed.Compare(rec(a).LastPlayed), byName(a, b))
		})
	case "starred":
		albums = slices.DeleteFunc(albums, func(a *jellyfin.Album) bool { return !rec(a).Favorite })
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			return cmp.Or(rec(b).FavoriteAt.Compare(rec(a).FavoriteAt), byName(a, b))
		})
	case "byYear":
		if !q.HasYears {
			return nil, missing("fromYear and toYear")
		}
		from, to := min(q.FromYear, q.ToYear), max(q.FromYear, q.ToYear)
		albums = slices.DeleteFunc(albums, func(a *jellyfin.Album) bool { return a.Year < from || a.Year > to })
		descending := q.FromYear > q.ToYear
		slices.SortStableFunc(albums, func(a, b *jellyfin.Album) int {
			order := cmp.Compare(a.Year, b.Year)
			if descending {
				order = -order
			}
			return cmp.Or(order, byName(a, b))
		})
	case "byGenre":
		if q.Genre == "" {
			return nil, missing("genre")
		}
		albums = slices.DeleteFunc(albums, func(a *jellyfin.Album) bool { return !hasGenre(a.Genres, q.Genre) })
		slices.SortStableFunc(albums, byName)
	default:
		return nil, &Problem{Code: ErrGeneric, Message: "unknown type of album list: " + q.Type}
	}
	return pageOf(albums, q.Page), nil
}

// SongQuery is what getRandomSongs is asked for: tracks of a genre, of the
// years between FromYear and ToYear, where they are not 0.
type SongQuery struct {
	Size     int
	Genre    string
	FromYear int
	ToYear   int
}

// RandomSongs picks tracks of the libraries at random.
func RandomSongs(libs []*jellyfin.Library, q SongQuery) []*jellyfin.Track {
	var tracks []*jellyfin.Track
	for _, l := range libs {
		for _, t := range l.Tracks {
			if q.Genre != "" && !hasGenre(t.Genres, q.Genre) ||
				q.FromYear != 0 && t.Year < q.FromYear || q.ToYear != 0 && t.Year > q.ToYear {
				continue
			}
			tracks = append(tracks, t)
		}
	}
	rand.Shuffle(len(tracks), func(i, j int) { tracks[i], tracks[j] = tracks[j], tracks[i] })
	return pageOf(tracks, Page{Size: q.Size})
}

// SongsByGenre are the tracks of a genre, by album.
func SongsByGenre(libs []*jellyfin.Library, genre string, p Page) []*jellyfin.Track {
	var tracks []*jellyfin.Track
	for _, l := range libs {
		for _, t := range l.Tracks {
			if hasGenre(t.Genres, genre) {
				tracks = append(tracks, t)
			}
		}
	}
	return pageOf(tracks, p)
}

// Favorites are the artists, the albums and the tracks an account made
// favorites.
func Favorites(libs []*jellyfin.Library, data func(jellyfin.Item) jellyfin.Record) ([]*jellyfin.Artist, []*jellyfin.Album, []*jellyfin.Track) {
	var artists []*jellyfin.Artist
	var albums []*jellyfin.Album
	var tracks []*jellyfin.Track
	if data == nil {
		return nil, nil, nil
	}
	for _, l := range libs {
		for _, a := range l.Artists {
			if data(a).Favorite {
				artists = append(artists, a)
			}
		}
		for _, a := range l.Albums {
			if data(a).Favorite {
				albums = append(albums, a)
			}
		}
		for _, t := range l.Tracks {
			if data(t).Favorite {
				tracks = append(tracks, t)
			}
		}
	}
	return artists, albums, tracks
}

// AllGenres are the genres of the libraries, one for each name whatever its
// case, by name.
func AllGenres(libs []*jellyfin.Library) []Genre {
	byName := map[string]*Genre{}
	var out []*Genre
	for _, l := range libs {
		for _, g := range l.Genres {
			key := strings.ToLower(g.Name)
			found, ok := byName[key]
			if !ok {
				found = &Genre{Value: g.Name}
				byName[key] = found
				out = append(out, found)
			}
			found.SongCount += len(g.Tracks)
			found.AlbumCount += len(g.Albums)
		}
	}
	slices.SortFunc(out, func(a, b *Genre) int {
		return strings.Compare(strings.ToLower(a.Value), strings.ToLower(b.Value))
	})
	genres := make([]Genre, len(out))
	for i, g := range out {
		genres[i] = *g
	}
	return genres
}

// ArtistIndex is the artists of albums of the libraries by the letter their
// names sort by: A to Z, and # last for those that sort by no letter.
func ArtistIndex(libs []*jellyfin.Library, d Describer) []Index {
	byLetter := map[string][]*jellyfin.Artist{}
	for _, l := range libs {
		for _, a := range albumArtists(l.Artists) {
			letter := "#"
			if r := []rune(jellyfin.SortName(a.Name)); len(r) > 0 && unicode.IsLetter(r[0]) {
				letter = string(unicode.ToUpper(r[0]))
			}
			byLetter[letter] = append(byLetter[letter], a)
		}
	}
	letters := make([]string, 0, len(byLetter))
	for letter := range byLetter {
		letters = append(letters, letter)
	}
	slices.SortFunc(letters, func(a, b string) int {
		return cmp.Or(cmp.Compare(boolInt(a == "#"), boolInt(b == "#")), strings.Compare(a, b))
	})
	out := make([]Index, len(letters))
	for i, letter := range letters {
		artists := byLetter[letter]
		slices.SortStableFunc(artists, func(a, b *jellyfin.Artist) int {
			return cmp.Or(strings.Compare(jellyfin.SortName(a.Name), jellyfin.SortName(b.Name)), strings.Compare(a.ID, b.ID))
		})
		out[i] = Index{Name: letter, Artist: d.Artists(artists)}
	}
	return out
}

// LastBuilt is when the newest of the libraries was built.
func LastBuilt(libs []*jellyfin.Library) time.Time {
	var last time.Time
	for _, l := range libs {
		if l.Built.After(last) {
			last = l.Built
		}
	}
	return last
}

// SearchQuery is what search2 and search3 are asked for: the words to find,
// and a page of each kind of item.
type SearchQuery struct {
	Query                  string
	Artists, Albums, Songs Page
}

// Search finds the artists, the albums and the tracks whose names have every
// word of the query, whatever their case: an album also by its artist, a
// track also by its artist and its album. An empty query finds everything,
// which is how clients such as Symfonium read a whole library, page by page,
// in an order that is the same every time.
func Search(libs []*jellyfin.Library, q SearchQuery) ([]*jellyfin.Artist, []*jellyfin.Album, []*jellyfin.Track) {
	// clients send "" for everything, with its quotes
	words := strings.Fields(strings.ToLower(strings.Trim(strings.TrimSpace(q.Query), `"*`)))
	has := func(texts ...string) bool {
		joined := strings.ToLower(strings.Join(texts, "\x00"))
		for _, w := range words {
			if !strings.Contains(joined, w) {
				return false
			}
		}
		return true
	}
	var artists []*jellyfin.Artist
	var albums []*jellyfin.Album
	var tracks []*jellyfin.Track
	for _, l := range libs {
		for _, a := range l.Artists {
			if has(a.Name) {
				artists = append(artists, a)
			}
		}
		for _, a := range l.Albums {
			if has(a.Name, a.Artist.Name) {
				albums = append(albums, a)
			}
		}
		for _, t := range l.Tracks {
			if has(t.Name, names(t.Artists), t.Album.Name) {
				tracks = append(tracks, t)
			}
		}
	}
	return searchPage(artists, q.Artists), searchPage(albums, q.Albums), searchPage(tracks, q.Songs)
}

// searchPage is the page of the found items of one kind: a client that asks
// for 0 of a kind gets none, and one that says nothing the default.
func searchPage[T any](items []T, p Page) []T {
	if p.Size == 0 {
		return nil
	}
	if p.Size < 0 {
		p.Size = DefaultSearchSize
	}
	return pageOf(items, p)
}

func byName(a, b *jellyfin.Album) int {
	return cmp.Or(strings.Compare(jellyfin.SortName(a.Name), jellyfin.SortName(b.Name)), strings.Compare(a.ID, b.ID))
}

func hasGenre(genres []*jellyfin.Genre, name string) bool {
	for _, g := range genres {
		if strings.EqualFold(g.Name, name) {
			return true
		}
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
