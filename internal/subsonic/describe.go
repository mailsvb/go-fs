package subsonic

import (
	"crypto/sha256"
	"encoding/binary"
	"path"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/jellyfin"
)

// Describer describes the items of the libraries as the API does, with what
// is kept of the account that asks.
type Describer struct {
	// Data is what is kept of the account and an item, as
	// jellyfin.Listeners.Data has it; nil for nothing.
	Data func(jellyfin.Item) jellyfin.Record
}

func (d Describer) record(item jellyfin.Item) jellyfin.Record {
	if d.Data == nil {
		return jellyfin.Record{}
	}
	return d.Data(item)
}

// starred is when an item was made a favorite, nil for one that is not. One
// made a favorite before go-fs kept when is taken to be one since its library
// was built.
func (d Describer) starred(item jellyfin.Item) *time.Time {
	r := d.record(item)
	if !r.Favorite {
		return nil
	}
	at := r.FavoriteAt
	if at.IsZero() {
		at = item.Library().Built
	}
	return timeOf(at)
}

// FolderID is the id of the music folder a library is, a number as the API
// has it: the same every time, from the name of its repository.
func FolderID(l *jellyfin.Library) int {
	sum := sha256.Sum256([]byte("subsonic\x00" + l.Name))
	return int(binary.BigEndian.Uint32(sum[:4]) >> 1)
}

// Folder is a library as a music folder.
func Folder(l *jellyfin.Library) MusicFolder {
	return MusicFolder{ID: FolderID(l), Name: l.Title}
}

// coverArt is the id the cover of an item is asked for by, "" for an item
// without one. The tracks of an album are given the album's, so that a
// client fetches it once.
func coverArt(item jellyfin.Item) string {
	if _, _, ok := jellyfin.CoverOf(item); ok {
		return item.ItemID()
	}
	return ""
}

// Song describes a track.
func (d Describer) Song(t *jellyfin.Track) Child {
	r := d.record(t)
	suffix := strings.ToLower(strings.TrimPrefix(path.Ext(t.Href), "."))
	c := Child{
		ID: t.ID, Parent: t.Album.ID, Title: t.Name, Album: t.Album.Name, Artist: t.Artist.Name,
		Track: t.Number, Year: t.Year, CoverArt: coverArt(t.Album), Size: t.Size,
		ContentType: MediaType(suffix), Suffix: suffix, Duration: seconds(t.Duration),
		BitRate: (t.Bitrate + 500) / 1000, BitDepth: t.BitDepth, SamplingRate: t.SampleRate,
		ChannelCount: t.Channels, Path: t.Href, PlayCount: r.PlayCount, Played: timeOf(r.LastPlayed),
		DiscNumber: t.Disc, Created: timeOf(t.ModTime), AlbumID: t.Album.ID, ArtistID: t.Artist.ID,
		Type: "music", MediaType: "song", Starred: d.starred(t), SortName: jellyfin.SortName(t.Name),
		DisplayArtist: names(t.Artists), DisplayAlbumArtist: t.Album.Artist.Name, DisplayComposer: t.Composer,
		Genres: genres(t.Genres), Artists: refs(t.Artists), AlbumArtists: refs([]*jellyfin.Artist{t.Album.Artist}),
		ReplayGain: &ReplayGain{TrackGain: t.Gain, AlbumGain: t.AlbumGain},
	}
	if len(t.Genres) > 0 {
		c.Genre = t.Genres[0].Name
	}
	return c
}

// Songs describes tracks.
func (d Describer) Songs(tracks []*jellyfin.Track) []Child {
	out := make([]Child, len(tracks))
	for i, t := range tracks {
		out[i] = d.Song(t)
	}
	return out
}

// Album describes an album.
func (d Describer) Album(a *jellyfin.Album) AlbumID3 {
	r := d.record(a)
	out := AlbumID3{
		ID: a.ID, Name: a.Name, Artist: a.Artist.Name, ArtistID: a.Artist.ID, CoverArt: coverArt(a),
		SongCount: len(a.Tracks), Duration: seconds(a.Duration), PlayCount: r.PlayCount, Created: a.Added.UTC().Truncate(time.Millisecond),
		Starred: d.starred(a), Year: a.Year, Played: timeOf(r.LastPlayed), SortName: jellyfin.SortName(a.Name),
		IsCompilation: a.Artist.Name == VariousArtists, DisplayArtist: a.Artist.Name, Genres: genres(a.Genres),
		Artists: refs([]*jellyfin.Artist{a.Artist}), ReleaseTypes: []string{}, Moods: []string{},
	}
	if len(a.Genres) > 0 {
		out.Genre = a.Genres[0].Name
	}
	return out
}

// VariousArtists is the album artist of a compilation, as the libraries name
// it where no album artist is tagged.
const VariousArtists = "Various Artists"

// Albums describes albums.
func (d Describer) Albums(albums []*jellyfin.Album) []AlbumID3 {
	out := make([]AlbumID3, len(albums))
	for i, a := range albums {
		out[i] = d.Album(a)
	}
	return out
}

// AlbumWithSongs describes an album and its tracks.
func (d Describer) AlbumWithSongs(a *jellyfin.Album) *AlbumWithSongs {
	return &AlbumWithSongs{AlbumID3: d.Album(a), Song: d.Songs(a.Tracks)}
}

// AlbumDir describes an album as a directory of its tracks.
func (d Describer) AlbumDir(a *jellyfin.Album) Child {
	r := d.record(a)
	c := Child{
		ID: a.ID, Parent: a.Artist.ID, IsDir: true, Title: a.Name, Album: a.Name, Artist: a.Artist.Name,
		Year: a.Year, CoverArt: coverArt(a), Duration: seconds(a.Duration), PlayCount: r.PlayCount,
		Played: timeOf(r.LastPlayed), Created: timeOf(a.Added), AlbumID: a.ID, ArtistID: a.Artist.ID,
		MediaType: "album", Starred: d.starred(a), SortName: jellyfin.SortName(a.Name),
		DisplayArtist: a.Artist.Name, DisplayAlbumArtist: a.Artist.Name, Genres: genres(a.Genres),
		Artists: refs([]*jellyfin.Artist{a.Artist}), AlbumArtists: refs([]*jellyfin.Artist{a.Artist}),
	}
	if len(a.Genres) > 0 {
		c.Genre = a.Genres[0].Name
	}
	return c
}

// AlbumDirs describes albums as directories.
func (d Describer) AlbumDirs(albums []*jellyfin.Album) []Child {
	out := make([]Child, len(albums))
	for i, a := range albums {
		out[i] = d.AlbumDir(a)
	}
	return out
}

// Artist describes an artist.
func (d Describer) Artist(a *jellyfin.Artist) ArtistID3 {
	return ArtistID3{ID: a.ID, Name: a.Name, CoverArt: coverArt(a), AlbumCount: len(a.Albums),
		Starred: d.starred(a), SortName: jellyfin.SortName(a.Name)}
}

// Artists describes artists.
func (d Describer) Artists(artists []*jellyfin.Artist) []ArtistID3 {
	out := make([]ArtistID3, len(artists))
	for i, a := range artists {
		out[i] = d.Artist(a)
	}
	return out
}

// ArtistWithAlbums describes an artist and the albums it is the album artist
// of.
func (d Describer) ArtistWithAlbums(a *jellyfin.Artist) *ArtistWithAlbum {
	return &ArtistWithAlbum{ArtistID3: d.Artist(a), Album: d.Albums(a.Albums)}
}

// Directory describes a library, an artist or an album as getMusicDirectory
// browses it: a library as the directory of its artists, an artist as that
// of its albums, an album as that of its tracks. ok is false for any other
// item, a track or a genre, which is no directory.
func (d Describer) Directory(item jellyfin.Item) (*Directory, bool) {
	r := d.record(item)
	dir := &Directory{ID: item.ItemID(), Name: item.ItemName(), Starred: d.starred(item), PlayCount: r.PlayCount}
	switch v := item.(type) {
	case *jellyfin.Library:
		dir.Starred, dir.PlayCount = nil, 0
		for _, a := range albumArtists(v.Artists) {
			dir.Child = append(dir.Child, Child{ID: a.ID, Parent: v.ID, IsDir: true, Title: a.Name,
				Artist: a.Name, ArtistID: a.ID, CoverArt: coverArt(a), MediaType: "artist",
				Starred: d.starred(a), SortName: jellyfin.SortName(a.Name), DisplayArtist: a.Name,
				Genres: []ItemGenre{}, Artists: refs([]*jellyfin.Artist{a}), AlbumArtists: []ArtistRef{}})
		}
	case *jellyfin.Artist:
		dir.Parent = v.Library().ID
		dir.Child = d.AlbumDirs(v.Albums)
	case *jellyfin.Album:
		dir.Parent = v.Artist.ID
		dir.Child = d.Songs(v.Tracks)
	default:
		return nil, false
	}
	return dir, true
}

// albumArtists are the artists of albums, of the artists of a library: those
// who are on the albums of others only are not browsed by.
func albumArtists(artists []*jellyfin.Artist) []*jellyfin.Artist {
	var out []*jellyfin.Artist
	for _, a := range artists {
		if len(a.Albums) > 0 {
			out = append(out, a)
		}
	}
	return out
}

// MediaType is the media type of an audio file by its extension.
func MediaType(suffix string) string {
	switch suffix {
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "m4a":
		return "audio/mp4"
	case "aac":
		return "audio/aac"
	case "ogg", "oga":
		return "audio/ogg"
	case "opus":
		return "audio/opus"
	case "wav":
		return "audio/wav"
	}
	return "application/octet-stream"
}

func seconds(d time.Duration) int { return int(d.Round(time.Second) / time.Second) }

// timeOf is a time as the API has it, in UTC to the millisecond; nil for
// none.
func timeOf(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC().Truncate(time.Millisecond)
	return &t
}

func names(artists []*jellyfin.Artist) string {
	parts := make([]string, len(artists))
	for i, a := range artists {
		parts[i] = a.Name
	}
	return strings.Join(parts, ", ")
}

func refs(artists []*jellyfin.Artist) []ArtistRef {
	out := make([]ArtistRef, len(artists))
	for i, a := range artists {
		out[i] = ArtistRef{ID: a.ID, Name: a.Name}
	}
	return out
}

func genres(gs []*jellyfin.Genre) []ItemGenre {
	out := make([]ItemGenre, len(gs))
	for i, g := range gs {
		out[i] = ItemGenre{Name: g.Name}
	}
	return out
}
