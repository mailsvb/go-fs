// Package jellyfin is the music library of a jellyfin or a subsonic
// repository: the index of its audio files by album, artist and genre, which
// the repository builds from its folder, what the listeners did with it, and
// the answers the HTTP server gives Jellyfin clients from it; internal/subsonic
// gives those of OpenSubsonic clients from the same index. It knows nothing of
// HTTP or of accounts: the server asks it what a client may see, and passes in
// what the account may read.
//
// The libraries live here rather than on the repository that builds them,
// because the server that answers from them is built apart from the
// repositories, as internal/repository keeps the statuses the admin interface
// shows.
package jellyfin

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mailsvb/go-fs/internal/audio"
)

// Source is an audio file of a repository's folder, as the repository read
// it.
type Source struct {
	// Href is the file's path in the folder, with forward slashes.
	Href    string
	Size    int64
	ModTime time.Time
	audio.Track
}

// Library is the index of one repository's folder. It is built once and not
// changed after: a change of the folder builds a new one.
type Library struct {
	// Name is the repository's name, and Title what clients show of it.
	Name  string
	Title string
	// Folder is the repository's folder as the HTTP server serves it, and
	// Path where it is on disk.
	Folder string
	Path   string
	ID     string
	Built  time.Time

	Tracks  []*Track
	Albums  []*Album
	Artists []*Artist
	Genres  []*Genre

	sources []Source
	covers  map[string]Cover
	byID    map[string]Item

	// views are the parts of the library accounts that may not read all of
	// it see, by the key the server gives the account; see For.
	mu    sync.Mutex
	views map[string]*Library
}

// Item is what a client browses: a library, an album, an artist, a genre or
// a track.
type Item interface {
	ItemID() string
	ItemName() string
	// ItemType is the type Jellyfin gives the item.
	ItemType() string
	// Library is the library the item is in.
	Library() *Library
}

// Track is an audio file of the library.
type Track struct {
	ID   string
	Name string
	Href string
	// Artists are the artists of the track, of which there is at least one,
	// and Artist the first of them. Album is the album it is on, which is
	// never nil, and Genres the genres it is tagged with.
	Artist   *Artist
	Artists  []*Artist
	Album    *Album
	Genres   []*Genre
	Composer string
	Number   int
	Disc     int
	Year     int
	Size     int64
	ModTime  time.Time
	// Picture says that the file carries a picture.
	Picture bool
	// Gain and AlbumGain are the ReplayGain of the track and of its album
	// as the track is tagged, nil where it is not.
	Gain      *float64
	AlbumGain *float64
	audio.Format

	lib *Library
}

// Album is the tracks of one album: those tagged with it and its album
// artist, or, where no album artist is tagged, those tagged with it in one
// folder, which is how a compilation is laid out.
type Album struct {
	ID   string
	Name string
	// Artist is the album artist.
	Artist *Artist
	Year   int
	Tracks []*Track
	Genres []*Genre
	// Gain is the ReplayGain of the album as its tracks are tagged, nil
	// where none is.
	Gain *float64
	// Added is when the newest of its files was put into the folder.
	Added    time.Time
	Duration time.Duration

	// cover is the image beside the tracks on disk, "" when there is none,
	// and coverTrack the track that carries the picture otherwise.
	cover      string
	coverTrack *Track
	coverTag   string
	// blurHash is the blurhash of the cover, "" when there is none or it
	// cannot be decoded.
	blurHash string
	lib      *Library
}

// Artist is an artist of tracks, of albums, or of both.
type Artist struct {
	ID     string
	Name   string
	Albums []*Album
	Tracks []*Track
	lib    *Library
}

// Genre is the tracks tagged with one genre, and their albums.
type Genre struct {
	ID     string
	Name   string
	Albums []*Album
	Tracks []*Track
	lib    *Library
}

func (l *Library) ItemID() string      { return l.ID }
func (l *Library) ItemName() string    { return l.Title }
func (l *Library) ItemType() string    { return "CollectionFolder" }
func (l *Library) Library() *Library   { return l }
func (t *Track) ItemID() string        { return t.ID }
func (t *Track) ItemName() string      { return t.Name }
func (t *Track) ItemType() string      { return "Audio" }
func (t *Track) Library() *Library     { return t.lib }
func (a *Album) ItemID() string        { return a.ID }
func (a *Album) ItemName() string      { return a.Name }
func (a *Album) ItemType() string      { return "MusicAlbum" }
func (a *Album) Library() *Library     { return a.lib }
func (a *Artist) ItemID() string       { return a.ID }
func (a *Artist) ItemName() string     { return a.Name }
func (a *Artist) ItemType() string     { return "MusicArtist" }
func (a *Artist) Library() *Library    { return a.lib }
func (g *Genre) ItemID() string        { return g.ID }
func (g *Genre) ItemName() string      { return g.Name }
func (g *Genre) ItemType() string      { return "MusicGenre" }
func (g *Genre) Library() *Library     { return g.lib }
func (t *Track) Virtual() string       { return path.Join(t.lib.Folder, t.Href) }
func (t *Track) OnDisk() string        { return joinDisk(t.lib.Path, t.Href) }
func (l *Library) Item(id string) Item { return l.byID[id] }

// Fallbacks for what neither the tags nor the folders say.
const (
	unknownArtist = "Unknown artist"
	unknownAlbum  = "Unknown album"
	// variousArtists is the album artist of an album of tracks by more than
	// one artist where none is tagged.
	variousArtists = "Various Artists"
)

// Build indexes the audio files of a repository. covers are the images
// beside them, by folder, as Covers finds them.
func Build(name, title, folder, onDisk string, sources []Source, covers map[string]Cover, built time.Time) *Library {
	if title == "" {
		title = name
	}
	l := &Library{Name: name, Title: title, Folder: folder, Path: onDisk, ID: newID(name), Built: built,
		sources: sources, covers: covers, byID: map[string]Item{}}
	l.byID[l.ID] = l

	artists := map[string]*Artist{}
	artist := func(name string) *Artist {
		key := strings.ToLower(name)
		if a, ok := artists[key]; ok {
			return a
		}
		a := &Artist{ID: newID(l.Name, "artist", key), Name: name, lib: l}
		artists[key] = a
		return a
	}
	genres := map[string]*Genre{}
	albums := map[string]*Album{}
	// tagged says the album artist of an album came from a tag rather than
	// from its tracks
	tagged := map[*Album]bool{}

	for _, s := range sources {
		dir := path.Dir(s.Href)
		t := &Track{ID: newID(l.Name, s.Href), Href: s.Href, Composer: s.Composer, Number: s.Number,
			Disc: s.Disc, Year: s.Year, Size: s.Size, ModTime: s.ModTime, Picture: s.Picture,
			Gain: s.TrackGain, AlbumGain: s.AlbumGain, Format: s.Format, lib: l}
		t.Name = s.Title
		if t.Name == "" {
			t.Name, t.Number = fromFileName(path.Base(s.Href), t.Number)
		}
		folders := strings.Split(dir, "/")
		artistNames := splitNames(cmp.Or(s.Artist, s.AlbumArtist))
		albumName := s.Album
		if dir != "." {
			albumName = cmp.Or(albumName, folders[len(folders)-1])
			if len(folders) > 1 && len(artistNames) == 0 {
				artistNames = []string{folders[len(folders)-2]}
			}
		}
		if len(artistNames) == 0 {
			artistNames = []string{unknownArtist}
		}
		for _, name := range artistNames {
			t.Artists = append(t.Artists, artist(name))
		}
		t.Artist = t.Artists[0]
		albumName = cmp.Or(albumName, unknownAlbum)

		var key string
		if s.AlbumArtist != "" {
			key = "artist\x00" + strings.ToLower(s.AlbumArtist) + "\x00" + strings.ToLower(albumName)
		} else {
			key = "folder\x00" + dir + "\x00" + strings.ToLower(albumName)
		}
		a, ok := albums[key]
		if !ok {
			a = &Album{ID: newID(l.Name, "album", key), Name: albumName, lib: l}
			albums[key] = a
			if s.AlbumArtist != "" {
				a.Artist = artist(s.AlbumArtist)
				tagged[a] = true
			}
		}
		t.Album = a
		a.Tracks = append(a.Tracks, t)

		for _, name := range splitNames(s.Genre) {
			gkey := strings.ToLower(name)
			g, ok := genres[gkey]
			if !ok {
				g = &Genre{ID: newID(l.Name, "genre", gkey), Name: name, lib: l}
				genres[gkey] = g
			}
			t.Genres = append(t.Genres, g)
			g.Tracks = append(g.Tracks, t)
		}
		l.Tracks = append(l.Tracks, t)
	}

	for _, a := range albums {
		slices.SortStableFunc(a.Tracks, byPosition)
		if !tagged[a] {
			a.Artist = a.Tracks[0].Artist
			for _, t := range a.Tracks[1:] {
				if t.Artist != a.Artist {
					a.Artist = artist(variousArtists)
					break
				}
			}
		}
		seen := map[*Genre]bool{}
		for _, t := range a.Tracks {
			a.Year = max(a.Year, t.Year)
			a.Duration += t.Duration
			if t.ModTime.After(a.Added) {
				a.Added = t.ModTime
			}
			a.Gain = cmp.Or(a.Gain, t.AlbumGain)
			for _, g := range t.Genres {
				if !seen[g] {
					seen[g] = true
					a.Genres = append(a.Genres, g)
					g.Albums = append(g.Albums, a)
				}
			}
		}
		l.findCover(a)
		a.Artist.Albums = append(a.Artist.Albums, a)
		l.Albums = append(l.Albums, a)
	}
	for _, t := range l.Tracks {
		for _, a := range t.Artists {
			a.Tracks = append(a.Tracks, t)
		}
	}
	for _, a := range artists {
		slices.SortFunc(a.Albums, byName)
		slices.SortStableFunc(a.Tracks, byAlbum)
		l.Artists = append(l.Artists, a)
	}
	for _, g := range genres {
		slices.SortFunc(g.Albums, byName)
		slices.SortStableFunc(g.Tracks, byAlbum)
		l.Genres = append(l.Genres, g)
	}
	slices.SortFunc(l.Albums, byName)
	hashCovers(l.Albums)
	slices.SortFunc(l.Artists, byName)
	slices.SortFunc(l.Genres, byName)
	slices.SortStableFunc(l.Tracks, byAlbum)
	for _, t := range l.Tracks {
		l.byID[t.ID] = t
	}
	for _, a := range l.Albums {
		l.byID[a.ID] = a
	}
	for _, a := range l.Artists {
		l.byID[a.ID] = a
	}
	for _, g := range l.Genres {
		l.byID[g.ID] = g
	}
	return l
}

// For is the part of the library an account sees: the tracks allowed says
// it may read, and the albums, artists and genres of those. key names the
// account and its rights, so that the part is worked out once per build of
// the library and not on every request.
func (l *Library) For(key string, allowed func(virtual string) bool) *Library {
	l.mu.Lock()
	defer l.mu.Unlock()
	if view, ok := l.views[key]; ok {
		return view
	}
	var visible []Source
	for _, s := range l.sources {
		if allowed(path.Join(l.Folder, s.Href)) {
			visible = append(visible, s)
		}
	}
	view := l
	if len(visible) != len(l.sources) {
		view = Build(l.Name, l.Title, l.Folder, l.Path, visible, l.covers, l.Built)
	}
	if l.views == nil {
		l.views = map[string]*Library{}
	}
	l.views[key] = view
	return view
}

// byPosition orders the tracks of an album: by disc, by number, and by name
// where they are not numbered.
func byPosition(a, b *Track) int {
	return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(a.Number, b.Number),
		strings.Compare(sortName(a.Name), sortName(b.Name)), strings.Compare(a.Href, b.Href))
}

// byAlbum orders tracks by their album, and on it by their position.
func byAlbum(a, b *Track) int {
	return cmp.Or(byName(a.Album, b.Album), byPosition(a, b))
}

// byName orders items by the name they are sorted by, and then by id, so that
// the order is the same every time.
func byName[T Item](a, b T) int {
	return cmp.Or(strings.Compare(sortName(a.ItemName()), sortName(b.ItemName())),
		strings.Compare(a.ItemID(), b.ItemID()))
}

// SortName is the name an item is sorted by, as the libraries sort: without
// case, and without the article in front of it.
func SortName(name string) string { return sortName(name) }

// sortName is the name an item is sorted by: without case, and without the
// article in front of it, as Jellyfin sorts.
func sortName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, article := range []string{"the ", "a ", "an "} {
		if rest, ok := strings.CutPrefix(name, article); ok && rest != "" {
			return rest
		}
	}
	return name
}

// splitNames are the names a tag holds, of artists or of genres: more than one
// where a semicolon parts them, or the NUL that ID3v2.4 parts them with. A
// slash parts none, for the names like AC/DC it is in.
func splitNames(tag string) []string {
	var names []string
	seen := map[string]bool{}
	for _, name := range strings.FieldsFunc(tag, func(r rune) bool { return r == ';' || r == 0 }) {
		name = strings.TrimSpace(name)
		if key := strings.ToLower(name); name != "" && !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	return names
}

// fromFileName names an untagged track after its file: without the
// extension, and without the number in front of the name, which it is taken
// as where no number is tagged.
func fromFileName(name string, number int) (string, int) {
	name = strings.TrimSuffix(name, path.Ext(name))
	digits := strings.IndexFunc(name, func(r rune) bool { return !unicode.IsDigit(r) })
	if digits <= 0 || digits > 3 {
		return name, number
	}
	rest := strings.TrimLeft(name[digits:], " .-_")
	if rest == "" || rest == name[digits:] && name[digits] != ' ' {
		return name, number
	}
	if number == 0 {
		number, _ = strconv.Atoi(name[:digits])
	}
	return rest, number
}

// newID makes the id of an item from what identifies it, in the form of a
// Jellyfin id: 32 hexadecimal digits. The same item has the same id in every
// build, so that what a client remembers of the library stays good.
func newID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// published are the libraries of the running repositories, by the type of
// the repository, jellyfin or subsonic, and by name: the clients of one API
// see the libraries of its repositories only.
var published = struct {
	sync.RWMutex
	byKind map[string]map[string]*Library
}{byKind: map[string]map[string]*Library{}}

// Publish makes a library the one of its repository, of type kind, that
// clients browse.
func Publish(kind string, l *Library) {
	published.Lock()
	defer published.Unlock()
	if published.byKind[kind] == nil {
		published.byKind[kind] = map[string]*Library{}
	}
	published.byKind[kind][l.Name] = l
}

// Retire takes the library of a repository of type kind away, which is
// stopped.
func Retire(kind, name string) {
	published.Lock()
	defer published.Unlock()
	delete(published.byKind[kind], name)
}

// Libraries are the published libraries of the repositories of type kind, by
// title.
func Libraries(kind string) []*Library {
	published.RLock()
	defer published.RUnlock()
	out := make([]*Library, 0, len(published.byKind[kind]))
	for _, l := range published.byKind[kind] {
		out = append(out, l)
	}
	slices.SortFunc(out, byName)
	return out
}
