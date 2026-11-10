// Package subsonic is what the HTTP server answers OpenSubsonic clients with,
// such as Symfonium, Feishin, Tempo, DSub and Amperfy, from the music
// libraries of the subsonic repositories: the responses of the API, in XML and
// in JSON, how the items of a library are described in them, and the lists
// and searches the clients ask for. The libraries are those internal/jellyfin
// builds, the same index a jellyfin repository has. Like that package, this
// one knows nothing of HTTP or of accounts: the server works out who asks and
// what the account may see, and hands in the libraries it sees.
//
// It follows the API as https://opensubsonic.netlify.app describes it, at
// version 1.16.1, with the extensions OpenSubsonic adds.
package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"time"
)

// Version is the version of the Subsonic API the server speaks, and Name what
// it says it is.
const (
	Version = "1.16.1"
	Name    = "go-fs"
)

// The codes of the errors of the API.
const (
	// ErrGeneric is any error the others do not name.
	ErrGeneric = 0
	// ErrMissing is a required parameter that was not sent.
	ErrMissing = 10
	// ErrCredentials is a wrong name or password.
	ErrCredentials = 40
	// ErrConflicting is more than one way of logging in at once.
	ErrConflicting = 43
	// ErrAPIKey is an API key that is not accepted.
	ErrAPIKey = 44
	// ErrNotAuthorized is what the account may not do.
	ErrNotAuthorized = 50
	// ErrNotFound is an item that is not there.
	ErrNotFound = 70
)

// Response is the subsonic-response every call is answered with. A list of
// items that is empty is left out of JSON, as the other servers leave it out,
// rather than sent as null; the lists OpenSubsonic adds to an item, such as
// its genres, are always sent, empty where there is nothing. One of the
// fields after Error is set, the one the call answers with; none for a call
// that only says it worked.
type Response struct {
	XMLName       xml.Name `xml:"subsonic-response" json:"-"`
	Xmlns         string   `xml:"xmlns,attr" json:"-"`
	Status        string   `xml:"status,attr" json:"status"`
	Version       string   `xml:"version,attr" json:"version"`
	Type          string   `xml:"type,attr" json:"type"`
	ServerVersion string   `xml:"serverVersion,attr" json:"serverVersion"`
	OpenSubsonic  bool     `xml:"openSubsonic,attr" json:"openSubsonic"`
	Error         *Error   `xml:"error,omitempty" json:"error,omitempty"`

	License                *License         `xml:"license,omitempty" json:"license,omitempty"`
	OpenSubsonicExtensions []Extension      `xml:"openSubsonicExtensions,omitempty" json:"openSubsonicExtensions,omitempty"`
	TokenInfo              *TokenInfo       `xml:"tokenInfo,omitempty" json:"tokenInfo,omitempty"`
	User                   *User            `xml:"user,omitempty" json:"user,omitempty"`
	Users                  *Users           `xml:"users,omitempty" json:"users,omitempty"`
	ScanStatus             *ScanStatus      `xml:"scanStatus,omitempty" json:"scanStatus,omitempty"`
	MusicFolders           *MusicFolders    `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	Indexes                *Indexes         `xml:"indexes,omitempty" json:"indexes,omitempty"`
	Artists                *Indexes         `xml:"artists,omitempty" json:"artists,omitempty"`
	Directory              *Directory       `xml:"directory,omitempty" json:"directory,omitempty"`
	Genres                 *Genres          `xml:"genres,omitempty" json:"genres,omitempty"`
	Artist                 *ArtistWithAlbum `xml:"artist,omitempty" json:"artist,omitempty"`
	Album                  *AlbumWithSongs  `xml:"album,omitempty" json:"album,omitempty"`
	Song                   *Child           `xml:"song,omitempty" json:"song,omitempty"`
	ArtistInfo             *Empty           `xml:"artistInfo,omitempty" json:"artistInfo,omitempty"`
	ArtistInfo2            *Empty           `xml:"artistInfo2,omitempty" json:"artistInfo2,omitempty"`
	AlbumInfo              *Empty           `xml:"albumInfo,omitempty" json:"albumInfo,omitempty"`
	SimilarSongs           *Songs           `xml:"similarSongs,omitempty" json:"similarSongs,omitempty"`
	SimilarSongs2          *Songs           `xml:"similarSongs2,omitempty" json:"similarSongs2,omitempty"`
	TopSongs               *Songs           `xml:"topSongs,omitempty" json:"topSongs,omitempty"`
	AlbumList              *AlbumList       `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2             *AlbumList2      `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	RandomSongs            *Songs           `xml:"randomSongs,omitempty" json:"randomSongs,omitempty"`
	SongsByGenre           *Songs           `xml:"songsByGenre,omitempty" json:"songsByGenre,omitempty"`
	NowPlaying             *NowPlaying      `xml:"nowPlaying,omitempty" json:"nowPlaying,omitempty"`
	Starred                *Starred         `xml:"starred,omitempty" json:"starred,omitempty"`
	Starred2               *Starred2        `xml:"starred2,omitempty" json:"starred2,omitempty"`
	SearchResult2          *Starred         `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	SearchResult3          *Starred2        `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	Lyrics                 *Lyrics          `xml:"lyrics,omitempty" json:"lyrics,omitempty"`
	LyricsList             *Empty           `xml:"lyricsList,omitempty" json:"lyricsList,omitempty"`

	// what go-fs keeps none of, answered empty so that a client shows
	// nothing rather than an error
	Playlists             *Empty `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Bookmarks             *Empty `xml:"bookmarks,omitempty" json:"bookmarks,omitempty"`
	InternetRadioStations *Empty `xml:"internetRadioStations,omitempty" json:"internetRadioStations,omitempty"`
	Podcasts              *Empty `xml:"podcasts,omitempty" json:"podcasts,omitempty"`
	NewestPodcasts        *Empty `xml:"newestPodcasts,omitempty" json:"newestPodcasts,omitempty"`
	Shares                *Empty `xml:"shares,omitempty" json:"shares,omitempty"`
	Videos                *Empty `xml:"videos,omitempty" json:"videos,omitempty"`
	ChatMessages          *Empty `xml:"chatMessages,omitempty" json:"chatMessages,omitempty"`
}

// Error is what a call that failed says of why.
type Error struct {
	Code    int    `xml:"code,attr" json:"code"`
	Message string `xml:"message,attr,omitempty" json:"message,omitempty"`
}

// Empty is an element with nothing in it.
type Empty struct{}

// License says the server needs none.
type License struct {
	Valid bool `xml:"valid,attr" json:"valid"`
}

// Extension is an extension of OpenSubsonic the server has, by the versions
// of it it speaks.
type Extension struct {
	Name     string `xml:"name,attr" json:"name"`
	Versions []int  `xml:"versions" json:"versions"`
}

// TokenInfo is whose an API key is.
type TokenInfo struct {
	Username string `xml:"username,attr" json:"username"`
}

// User is an account, as the API describes one: what it may do, and the
// music folders it sees.
type User struct {
	Username          string `xml:"username,attr" json:"username"`
	Email             string `xml:"email,attr,omitempty" json:"email,omitempty"`
	ScrobblingEnabled bool   `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	AdminRole         bool   `xml:"adminRole,attr" json:"adminRole"`
	SettingsRole      bool   `xml:"settingsRole,attr" json:"settingsRole"`
	DownloadRole      bool   `xml:"downloadRole,attr" json:"downloadRole"`
	UploadRole        bool   `xml:"uploadRole,attr" json:"uploadRole"`
	PlaylistRole      bool   `xml:"playlistRole,attr" json:"playlistRole"`
	CoverArtRole      bool   `xml:"coverArtRole,attr" json:"coverArtRole"`
	CommentRole       bool   `xml:"commentRole,attr" json:"commentRole"`
	PodcastRole       bool   `xml:"podcastRole,attr" json:"podcastRole"`
	StreamRole        bool   `xml:"streamRole,attr" json:"streamRole"`
	JukeboxRole       bool   `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	ShareRole         bool   `xml:"shareRole,attr" json:"shareRole"`
	VideoRole         bool   `xml:"videoConversionRole,attr" json:"videoConversionRole"`
	Folder            []int  `xml:"folder" json:"folder"`
}

// Users are the accounts an account sees, which is itself.
type Users struct {
	User []User `xml:"user" json:"user,omitempty"`
}

// ScanStatus says whether the libraries are being read, and how many tracks
// they have.
type ScanStatus struct {
	Scanning bool `xml:"scanning,attr" json:"scanning"`
	Count    int  `xml:"count,attr" json:"count"`
}

// MusicFolders are the libraries an account sees.
type MusicFolders struct {
	MusicFolder []MusicFolder `xml:"musicFolder" json:"musicFolder,omitempty"`
}

// MusicFolder is a library.
type MusicFolder struct {
	ID   int    `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

// Indexes are the artists by the letter their names sort by, as getIndexes
// and getArtists answer.
type Indexes struct {
	// LastModified is when the libraries were built last, in milliseconds
	// since 1970; getIndexes has it, getArtists does not.
	LastModified    int64   `xml:"lastModified,attr,omitempty" json:"lastModified,omitempty"`
	IgnoredArticles string  `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Index           []Index `xml:"index" json:"index,omitempty"`
}

// Index is the artists whose names sort by one letter, "#" for those that
// sort by no letter.
type Index struct {
	Name   string      `xml:"name,attr" json:"name"`
	Artist []ArtistID3 `xml:"artist" json:"artist,omitempty"`
}

// Directory is a library, an artist or an album, as getMusicDirectory
// browses them: the artists of a library, the albums of an artist, the
// tracks of an album.
type Directory struct {
	ID        string     `xml:"id,attr" json:"id"`
	Parent    string     `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	Name      string     `xml:"name,attr" json:"name"`
	Starred   *time.Time `xml:"starred,attr,omitempty" json:"starred,omitempty"`
	PlayCount int        `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	Child     []Child    `xml:"child" json:"child,omitempty"`
}

// Genres are the genres of the libraries.
type Genres struct {
	Genre []Genre `xml:"genre" json:"genre,omitempty"`
}

// Genre is a genre, by how many tracks and albums have it.
type Genre struct {
	Value      string `xml:",chardata" json:"value"`
	SongCount  int    `xml:"songCount,attr" json:"songCount"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
}

// ItemGenre is a genre of a track or an album.
type ItemGenre struct {
	Name string `xml:"name,attr" json:"name"`
}

// ArtistRef is an artist of a track or an album.
type ArtistRef struct {
	ID   string `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

// ArtistID3 is an artist.
type ArtistID3 struct {
	ID             string     `xml:"id,attr" json:"id"`
	Name           string     `xml:"name,attr" json:"name"`
	CoverArt       string     `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	AlbumCount     int        `xml:"albumCount,attr" json:"albumCount"`
	Starred        *time.Time `xml:"starred,attr,omitempty" json:"starred,omitempty"`
	SortName       string     `xml:"sortName,attr" json:"sortName"`
	MusicBrainzID  string     `xml:"musicBrainzId,attr" json:"musicBrainzId"`
	ArtistImageURL string     `xml:"artistImageUrl,attr,omitempty" json:"artistImageUrl,omitempty"`
}

// ArtistWithAlbum is an artist with its albums, as getArtist answers.
type ArtistWithAlbum struct {
	ArtistID3
	Album []AlbumID3 `xml:"album" json:"album,omitempty"`
}

// AlbumID3 is an album.
type AlbumID3 struct {
	ID            string      `xml:"id,attr" json:"id"`
	Name          string      `xml:"name,attr" json:"name"`
	Artist        string      `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID      string      `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt      string      `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount     int         `xml:"songCount,attr" json:"songCount"`
	Duration      int         `xml:"duration,attr" json:"duration"`
	PlayCount     int         `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	Created       time.Time   `xml:"created,attr" json:"created"`
	Starred       *time.Time  `xml:"starred,attr,omitempty" json:"starred,omitempty"`
	Year          int         `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre         string      `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	Played        *time.Time  `xml:"played,attr,omitempty" json:"played,omitempty"`
	SortName      string      `xml:"sortName,attr" json:"sortName"`
	MusicBrainzID string      `xml:"musicBrainzId,attr" json:"musicBrainzId"`
	IsCompilation bool        `xml:"isCompilation,attr" json:"isCompilation"`
	DisplayArtist string      `xml:"displayArtist,attr" json:"displayArtist"`
	Genres        []ItemGenre `xml:"genres" json:"genres"`
	Artists       []ArtistRef `xml:"artists" json:"artists"`
	ReleaseTypes  []string    `xml:"releaseTypes" json:"releaseTypes"`
	Moods         []string    `xml:"moods" json:"moods"`
}

// AlbumWithSongs is an album with its tracks, as getAlbum answers.
type AlbumWithSongs struct {
	AlbumID3
	Song []Child `xml:"song" json:"song,omitempty"`
}

// Child is a track, or, with IsDir, an album as a directory of tracks.
type Child struct {
	ID                 string      `xml:"id,attr" json:"id"`
	Parent             string      `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir              bool        `xml:"isDir,attr" json:"isDir"`
	Title              string      `xml:"title,attr" json:"title"`
	Album              string      `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist             string      `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track              int         `xml:"track,attr,omitempty" json:"track,omitempty"`
	Year               int         `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre              string      `xml:"genre,attr,omitempty" json:"genre,omitempty"`
	CoverArt           string      `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Size               int64       `xml:"size,attr,omitempty" json:"size,omitempty"`
	ContentType        string      `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix             string      `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	Duration           int         `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	BitRate            int         `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	BitDepth           int         `xml:"bitDepth,attr,omitempty" json:"bitDepth,omitempty"`
	SamplingRate       int         `xml:"samplingRate,attr,omitempty" json:"samplingRate,omitempty"`
	ChannelCount       int         `xml:"channelCount,attr,omitempty" json:"channelCount,omitempty"`
	Path               string      `xml:"path,attr,omitempty" json:"path,omitempty"`
	PlayCount          int         `xml:"playCount,attr,omitempty" json:"playCount,omitempty"`
	Played             *time.Time  `xml:"played,attr,omitempty" json:"played,omitempty"`
	DiscNumber         int         `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
	Created            *time.Time  `xml:"created,attr,omitempty" json:"created,omitempty"`
	AlbumID            string      `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID           string      `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type               string      `xml:"type,attr,omitempty" json:"type,omitempty"`
	MediaType          string      `xml:"mediaType,attr" json:"mediaType"`
	IsVideo            bool        `xml:"isVideo,attr" json:"isVideo"`
	Starred            *time.Time  `xml:"starred,attr,omitempty" json:"starred,omitempty"`
	BPM                int         `xml:"bpm,attr" json:"bpm"`
	Comment            string      `xml:"comment,attr" json:"comment"`
	SortName           string      `xml:"sortName,attr" json:"sortName"`
	MusicBrainzID      string      `xml:"musicBrainzId,attr" json:"musicBrainzId"`
	DisplayArtist      string      `xml:"displayArtist,attr" json:"displayArtist"`
	DisplayAlbumArtist string      `xml:"displayAlbumArtist,attr" json:"displayAlbumArtist"`
	DisplayComposer    string      `xml:"displayComposer,attr" json:"displayComposer"`
	ExplicitStatus     string      `xml:"explicitStatus,attr" json:"explicitStatus"`
	Genres             []ItemGenre `xml:"genres" json:"genres"`
	Artists            []ArtistRef `xml:"artists" json:"artists"`
	AlbumArtists       []ArtistRef `xml:"albumArtists" json:"albumArtists"`
	ReplayGain         *ReplayGain `xml:"replayGain,omitempty" json:"replayGain,omitempty"`
}

// ReplayGain is the ReplayGain a track is tagged with. A track has one,
// empty where it is tagged with none.
type ReplayGain struct {
	TrackGain *float64 `xml:"trackGain,attr,omitempty" json:"trackGain,omitempty"`
	AlbumGain *float64 `xml:"albumGain,attr,omitempty" json:"albumGain,omitempty"`
}

// Songs is a list of tracks.
type Songs struct {
	Song []Child `xml:"song" json:"song,omitempty"`
}

// AlbumList is a list of albums as directories, as getAlbumList answers.
type AlbumList struct {
	Album []Child `xml:"album" json:"album,omitempty"`
}

// AlbumList2 is a list of albums, as getAlbumList2 answers.
type AlbumList2 struct {
	Album []AlbumID3 `xml:"album" json:"album,omitempty"`
}

// NowPlaying is what is being played, which go-fs does not tell.
type NowPlaying struct {
	Entry []Child `xml:"entry" json:"entry,omitempty"`
}

// Starred is the favorites of an account, as getStarred answers, and what
// search2 finds.
type Starred struct {
	Artist []ArtistID3 `xml:"artist" json:"artist,omitempty"`
	Album  []Child     `xml:"album" json:"album,omitempty"`
	Song   []Child     `xml:"song" json:"song,omitempty"`
}

// Starred2 is the favorites of an account, as getStarred2 answers, and what
// search3 finds.
type Starred2 struct {
	Artist []ArtistID3 `xml:"artist" json:"artist,omitempty"`
	Album  []AlbumID3  `xml:"album" json:"album,omitempty"`
	Song   []Child     `xml:"song" json:"song,omitempty"`
}

// Lyrics are the lyrics of a track, of which go-fs has none.
type Lyrics struct {
	Artist string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Title  string `xml:"title,attr,omitempty" json:"title,omitempty"`
	Value  string `xml:",chardata" json:"value"`
}

// OK is the response of a call that worked; the server sets what it answers
// with on it, and its own version.
func OK(serverVersion string) *Response {
	return &Response{Xmlns: "http://subsonic.org/restapi", Status: "ok", Version: Version, Type: Name,
		ServerVersion: serverVersion, OpenSubsonic: true}
}

// Failed is the response of a call that failed.
func Failed(serverVersion string, code int, message string) *Response {
	r := OK(serverVersion)
	r.Status = "failed"
	r.Error = &Error{Code: code, Message: message}
	return r
}

// Marshal encodes a response in the format a client asks for by its f
// parameter: xml, which is what it gets when it asks for none, json, or
// jsonp, JSON wrapped into a call of callback. It returns the body and its
// media type.
func Marshal(format, callback string, r *Response) ([]byte, string, error) {
	switch format {
	case "json", "jsonp":
		body, err := json.Marshal(struct {
			Response *Response `json:"subsonic-response"`
		}{r})
		if err != nil {
			return nil, "", err
		}
		if format == "json" {
			return body, "application/json", nil
		}
		if !validCallback(callback) {
			return nil, "", fmt.Errorf("callback %q is not the name of a function", callback)
		}
		return []byte(callback + "(" + string(body) + ");"), "application/javascript", nil
	}
	body, err := xml.Marshal(r)
	if err != nil {
		return nil, "", err
	}
	return append([]byte(xml.Header), body...), "application/xml", nil
}

// validCallback reports whether a JSONP callback is a name a script can
// call, dots allowed, and nothing a page could be made to run.
func validCallback(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		letter := r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && !(i > 0 && (r == '.' || r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
