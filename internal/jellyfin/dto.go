package jellyfin

import (
	"time"
)

// The answers are the parts of Jellyfin's BaseItemDto and MediaSourceInfo
// that music clients read. Jellyfin names its fields in Pascal case, and
// clients written against its schema expect the lists of an item to be there
// even when they are empty.

// BaseItem is an item as Jellyfin describes it.
type BaseItem struct {
	Name              string            `json:"Name"`
	ServerID          string            `json:"ServerId"`
	ID                string            `json:"Id"`
	Etag              string            `json:"Etag,omitempty"`
	DateCreated       string            `json:"DateCreated,omitempty"`
	SortName          string            `json:"SortName,omitempty"`
	PremiereDate      string            `json:"PremiereDate,omitempty"`
	Container         string            `json:"Container,omitempty"`
	RunTimeTicks      int64             `json:"RunTimeTicks,omitempty"`
	ProductionYear    int               `json:"ProductionYear,omitempty"`
	IndexNumber       int               `json:"IndexNumber,omitempty"`
	ParentIndexNumber int               `json:"ParentIndexNumber,omitempty"`
	IsFolder          bool              `json:"IsFolder"`
	Type              string            `json:"Type"`
	CollectionType    string            `json:"CollectionType,omitempty"`
	ParentID          string            `json:"ParentId,omitempty"`
	ChildCount        int               `json:"ChildCount,omitempty"`
	SongCount         int               `json:"SongCount,omitempty"`
	AlbumCount        int               `json:"AlbumCount,omitempty"`
	Genres            []string          `json:"Genres"`
	GenreItems        []NameID          `json:"GenreItems"`
	Artists           []string          `json:"Artists"`
	ArtistItems       []NameID          `json:"ArtistItems"`
	Album             string            `json:"Album,omitempty"`
	AlbumID           string            `json:"AlbumId,omitempty"`
	AlbumImageTag     string            `json:"AlbumPrimaryImageTag,omitempty"`
	AlbumArtist       string            `json:"AlbumArtist,omitempty"`
	AlbumArtists      []NameID          `json:"AlbumArtists"`
	MediaType         string            `json:"MediaType"`
	LocationType      string            `json:"LocationType"`
	ImageTags         map[string]string `json:"ImageTags"`
	BackdropImageTags []string          `json:"BackdropImageTags"`
	ImageAspectRatio  float64           `json:"PrimaryImageAspectRatio,omitempty"`
	CanDownload       bool              `json:"CanDownload"`
	HasLyrics         bool              `json:"HasLyrics"`
	UserData          UserData          `json:"UserData"`
	MediaSources      []MediaSource     `json:"MediaSources,omitempty"`
}

// NameID names an item another one refers to.
type NameID struct {
	Name string `json:"Name"`
	ID   string `json:"Id"`
}

// UserData is what Jellyfin keeps of a listener and an item, as Listeners
// keeps it.
type UserData struct {
	PlaybackPositionTicks int64   `json:"PlaybackPositionTicks"`
	PlayedPercentage      float64 `json:"PlayedPercentage,omitempty"`
	// UnplayedItemCount is how many tracks of an album were not played.
	UnplayedItemCount *int   `json:"UnplayedItemCount,omitempty"`
	PlayCount         int    `json:"PlayCount"`
	IsFavorite        bool   `json:"IsFavorite"`
	LastPlayedDate    string `json:"LastPlayedDate,omitempty"`
	Played            bool   `json:"Played"`
	Key               string `json:"Key"`
	ItemID            string `json:"ItemId"`
}

// MediaSource is a way a track can be played, of which there is one: the
// file as it is. Nothing is transcoded.
type MediaSource struct {
	Protocol                string        `json:"Protocol"`
	ID                      string        `json:"Id"`
	Type                    string        `json:"Type"`
	Container               string        `json:"Container"`
	Size                    int64         `json:"Size"`
	Name                    string        `json:"Name"`
	IsRemote                bool          `json:"IsRemote"`
	ETag                    string        `json:"ETag"`
	RunTimeTicks            int64         `json:"RunTimeTicks,omitempty"`
	SupportsTranscoding     bool          `json:"SupportsTranscoding"`
	SupportsDirectStream    bool          `json:"SupportsDirectStream"`
	SupportsDirectPlay      bool          `json:"SupportsDirectPlay"`
	SupportsProbing         bool          `json:"SupportsProbing"`
	IsInfiniteStream        bool          `json:"IsInfiniteStream"`
	RequiresOpening         bool          `json:"RequiresOpening"`
	RequiresClosing         bool          `json:"RequiresClosing"`
	MediaStreams            []MediaStream `json:"MediaStreams"`
	MediaAttachments        []any         `json:"MediaAttachments"`
	Formats                 []string      `json:"Formats"`
	Bitrate                 int           `json:"Bitrate,omitempty"`
	DefaultAudioStreamIndex int           `json:"DefaultAudioStreamIndex"`
}

// MediaStream is the audio stream of a track.
type MediaStream struct {
	Codec         string `json:"Codec"`
	Type          string `json:"Type"`
	Index         int    `json:"Index"`
	IsDefault     bool   `json:"IsDefault"`
	IsExternal    bool   `json:"IsExternal"`
	Channels      int    `json:"Channels,omitempty"`
	ChannelLayout string `json:"ChannelLayout,omitempty"`
	SampleRate    int    `json:"SampleRate,omitempty"`
	BitRate       int    `json:"BitRate,omitempty"`
	DisplayTitle  string `json:"DisplayTitle"`
}

// Describer describes items to one listener of one server.
type Describer struct {
	ServerID string
	// Data is what is kept of the listener and an item, as Listeners.Data
	// has it; nil for nothing.
	Data func(Item) Record
}

// Items describes items.
func (d Describer) Items(items []Item) []BaseItem {
	out := make([]BaseItem, len(items))
	for i, item := range items {
		out[i] = d.Item(item)
	}
	return out
}

// Item describes an item.
func (d Describer) Item(item Item) BaseItem {
	b := BaseItem{
		Name: item.ItemName(), ServerID: d.ServerID, ID: item.ItemID(), Type: item.ItemType(),
		Etag: item.Library().ID[:16], SortName: sortName(item.ItemName()), IsFolder: true,
		MediaType: "Unknown", LocationType: "FileSystem", ImageTags: map[string]string{},
		Genres: []string{}, GenreItems: []NameID{}, Artists: []string{}, ArtistItems: []NameID{},
		AlbumArtists: []NameID{}, BackdropImageTags: []string{},
		UserData: d.UserData(item),
	}
	if tag := imageTag(item); tag != "" {
		b.ImageTags["Primary"] = tag
		b.ImageAspectRatio = 1
	}
	switch v := item.(type) {
	case *Library:
		b.CollectionType = "music"
		b.ChildCount = len(v.Albums)
		b.DateCreated = date(v.Built)
	case *Album:
		b.ParentID = v.lib.ID
		b.ChildCount = len(v.Tracks)
		b.SongCount = len(v.Tracks)
		b.RunTimeTicks = ticks(v.Duration)
		b.DateCreated = date(v.Added)
		b.ProductionYear = v.Year
		b.PremiereDate = premiere(v.Year)
		b.AlbumArtist = v.Artist.Name
		b.AlbumArtists = []NameID{nameID(v.Artist)}
		seen := map[*Artist]bool{}
		for _, t := range v.Tracks {
			if !seen[t.Artist] {
				seen[t.Artist] = true
				b.Artists = append(b.Artists, t.Artist.Name)
				b.ArtistItems = append(b.ArtistItems, nameID(t.Artist))
			}
		}
		for _, g := range v.Genres {
			b.Genres = append(b.Genres, g.Name)
			b.GenreItems = append(b.GenreItems, nameID(g))
		}
	case *Artist:
		b.ChildCount = len(v.Albums)
		b.AlbumCount = len(v.Albums)
		b.SongCount = len(v.Tracks)
	case *Genre:
		b.ChildCount = len(v.Albums)
		b.AlbumCount = len(v.Albums)
		b.SongCount = len(v.Tracks)
	case *Track:
		b.IsFolder = false
		b.MediaType = "Audio"
		b.CanDownload = true
		b.ParentID = v.Album.ID
		b.Container = container(v)
		b.RunTimeTicks = ticks(v.Duration)
		b.DateCreated = date(v.ModTime)
		b.ProductionYear = v.Year
		b.PremiereDate = premiere(v.Year)
		b.IndexNumber = v.Number
		b.ParentIndexNumber = v.Disc
		b.Album = v.Album.Name
		b.AlbumID = v.Album.ID
		b.AlbumImageTag = v.Album.coverTag
		b.AlbumArtist = v.Album.Artist.Name
		b.AlbumArtists = []NameID{nameID(v.Album.Artist)}
		b.Artists = []string{v.Artist.Name}
		b.ArtistItems = []NameID{nameID(v.Artist)}
		if v.Genre != nil {
			b.Genres = []string{v.Genre.Name}
			b.GenreItems = []NameID{nameID(v.Genre)}
		}
		b.MediaSources = []MediaSource{d.MediaSource(v)}
	}
	return b
}

// UserData describes what is kept of the listener and an item.
func (d Describer) UserData(item Item) UserData {
	u := UserData{Key: item.ItemID(), ItemID: item.ItemID()}
	if d.Data == nil {
		return u
	}
	r := d.Data(item)
	u.Played, u.PlayCount, u.IsFavorite = r.Played, r.PlayCount, r.Favorite
	u.LastPlayedDate = date(r.LastPlayed)
	u.PlaybackPositionTicks = r.Position
	switch v := item.(type) {
	case *Track:
		if runtime := ticks(v.Duration); r.Position > 0 && runtime > 0 {
			u.PlayedPercentage = 100 * float64(r.Position) / float64(runtime)
		}
	case *Album:
		unplayed := 0
		for _, t := range v.Tracks {
			if !d.Data(t).Played {
				unplayed++
			}
		}
		u.UnplayedItemCount = &unplayed
	}
	return u
}

// MediaSource describes the file of a track, which is played as it is.
func (d Describer) MediaSource(t *Track) MediaSource {
	layout := ""
	switch t.Channels {
	case 1:
		layout = "mono"
	case 2:
		layout = "stereo"
	}
	return MediaSource{
		Protocol: "File", ID: t.ID, Type: "Default", Container: container(t), Size: t.Size, Name: t.Name,
		ETag: tagOf(t.ID, t.ModTime), RunTimeTicks: ticks(t.Duration), SupportsDirectStream: true,
		SupportsDirectPlay: true, SupportsProbing: true, Bitrate: t.Bitrate,
		MediaAttachments: []any{}, Formats: []string{},
		MediaStreams: []MediaStream{{Codec: t.Codec, Type: "Audio", IsDefault: true, Channels: t.Channels,
			ChannelLayout: layout, SampleRate: t.SampleRate, BitRate: t.Bitrate, DisplayTitle: t.Codec}},
	}
}

// container is the container of a track as Jellyfin names it, which calls
// an Opus file Ogg, as it is.
func container(t *Track) string {
	if t.Container == "opus" {
		return "ogg"
	}
	return t.Container
}

func nameID(item Item) NameID { return NameID{Name: item.ItemName(), ID: item.ItemID()} }

// ticks is a duration in the unit of Jellyfin, a tenth of a microsecond.
func ticks(d time.Duration) int64 { return int64(d / 100) }

func date(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.0000000Z")
}

func premiere(year int) string {
	if year <= 0 {
		return ""
	}
	return date(time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC))
}

// What the sort keys read of an item.

func albumName(item Item) string {
	switch v := item.(type) {
	case *Track:
		return v.Album.Name
	case *Album:
		return v.Name
	}
	return ""
}

func albumArtist(item Item) string {
	switch v := item.(type) {
	case *Track:
		return v.Album.Artist.Name
	case *Album:
		return v.Artist.Name
	}
	return item.ItemName()
}

func artistName(item Item) string {
	if t, ok := item.(*Track); ok {
		return t.Artist.Name
	}
	return albumArtist(item)
}

func added(item Item) time.Time {
	switch v := item.(type) {
	case *Track:
		return v.ModTime
	case *Album:
		return v.Added
	case *Library:
		return v.Built
	}
	return time.Time{}
}

func yearOf(item Item) int {
	switch v := item.(type) {
	case *Track:
		return v.Year
	case *Album:
		return v.Year
	}
	return 0
}

func durationOf(item Item) time.Duration {
	switch v := item.(type) {
	case *Track:
		return v.Duration
	case *Album:
		return v.Duration
	}
	return 0
}
