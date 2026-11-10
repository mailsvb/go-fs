//go:build !light && !no_subsonic

package httpd

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/subsonic"
)

// subsonicHandler answers a method of the API. A public one is answered
// before the client has logged in.
type subsonicHandler struct {
	serve  func(*subsonicRequest)
	public bool
}

// subsonicMethods are the methods of the API the server answers, by their
// names in lower case. What go-fs keeps none of, playlists, the play queue,
// bookmarks, podcasts, radio stations, shares, videos and chat, is answered
// empty, a change of it as done or as not allowed, so that a client shows
// nothing rather than an error.
var subsonicMethods = map[string]subsonicHandler{}

func init() {
	answers := map[string]func(*subsonicRequest){
		// system
		"ping": func(q *subsonicRequest) { q.reply(q.ok()) },
		"getLicense": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.License = &subsonic.License{Valid: true} }))
		},
		"tokenInfo": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.TokenInfo = &subsonic.TokenInfo{Username: q.user.name} }))
		},
		"getUser":         (*subsonicRequest).getUser,
		"getUsers":        (*subsonicRequest).getUsers,
		"getScanStatus":   (*subsonicRequest).scanStatus,
		"startScan":       (*subsonicRequest).scanStatus,
		"getMusicFolders": (*subsonicRequest).musicFolders,

		// browsing
		"getIndexes":        (*subsonicRequest).indexes,
		"getArtists":        (*subsonicRequest).artists,
		"getMusicDirectory": (*subsonicRequest).directory,
		"getArtist":         (*subsonicRequest).artist,
		"getAlbum":          (*subsonicRequest).album,
		"getSong":           (*subsonicRequest).song,
		"getGenres":         (*subsonicRequest).genres,
		"getArtistInfo": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.ArtistInfo = &subsonic.Empty{} }))
		},
		"getArtistInfo2": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.ArtistInfo2 = &subsonic.Empty{} }))
		},
		"getAlbumInfo": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.AlbumInfo = &subsonic.Empty{} }))
		},
		"getAlbumInfo2": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.AlbumInfo = &subsonic.Empty{} }))
		},
		"getSimilarSongs": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.SimilarSongs = &subsonic.Songs{} }))
		},
		"getSimilarSongs2": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.SimilarSongs2 = &subsonic.Songs{} }))
		},
		"getTopSongs": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.TopSongs = &subsonic.Songs{} }))
		},

		// lists and searches
		"getAlbumList":    func(q *subsonicRequest) { q.albumList(false) },
		"getAlbumList2":   func(q *subsonicRequest) { q.albumList(true) },
		"getRandomSongs":  (*subsonicRequest).randomSongs,
		"getSongsByGenre": (*subsonicRequest).songsByGenre,
		"getStarred":      func(q *subsonicRequest) { q.starred(false) },
		"getStarred2":     func(q *subsonicRequest) { q.starred(true) },
		"getNowPlaying": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.NowPlaying = &subsonic.NowPlaying{} }))
		},
		"search2": func(q *subsonicRequest) { q.search(false) },
		"search3": func(q *subsonicRequest) { q.search(true) },

		// retrieval
		"stream":      func(q *subsonicRequest) { q.send(false) },
		"download":    func(q *subsonicRequest) { q.send(true) },
		"getCoverArt": (*subsonicRequest).coverArt,
		"getAvatar":   func(q *subsonicRequest) { http.NotFound(q.w, q.r) },
		"getLyrics": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) {
				r.Lyrics = &subsonic.Lyrics{Artist: q.str("artist"), Title: q.str("title")}
			}))
		},
		"getLyricsBySongId": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.LyricsList = &subsonic.Empty{} }))
		},

		// annotation
		"star":      func(q *subsonicRequest) { q.star(true) },
		"unstar":    func(q *subsonicRequest) { q.star(false) },
		"scrobble":  (*subsonicRequest).scrobble,
		"setRating": func(q *subsonicRequest) { q.reply(q.ok()) },

		// what go-fs keeps none of
		"getPlaylists": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.Playlists = &subsonic.Empty{} }))
		},
		"getPlaylist":          func(q *subsonicRequest) { q.fail(subsonic.ErrNotFound, "go-fs keeps no playlists") },
		"getPlayQueue":         func(q *subsonicRequest) { q.reply(q.ok()) },
		"getPlayQueueByIndex":  func(q *subsonicRequest) { q.reply(q.ok()) },
		"savePlayQueue":        func(q *subsonicRequest) { q.reply(q.ok()) },
		"savePlayQueueByIndex": func(q *subsonicRequest) { q.reply(q.ok()) },
		"getBookmarks": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.Bookmarks = &subsonic.Empty{} }))
		},
		"createBookmark": func(q *subsonicRequest) { q.reply(q.ok()) },
		"deleteBookmark": func(q *subsonicRequest) { q.reply(q.ok()) },
		"getInternetRadioStations": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.InternetRadioStations = &subsonic.Empty{} }))
		},
		"getPodcasts": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.Podcasts = &subsonic.Empty{} }))
		},
		"getNewestPodcasts": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.NewestPodcasts = &subsonic.Empty{} }))
		},
		"getShares": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.Shares = &subsonic.Empty{} }))
		},
		"getVideos": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.Videos = &subsonic.Empty{} }))
		},
		"getChatMessages": func(q *subsonicRequest) {
			q.reply(with(q, func(r *subsonic.Response) { r.ChatMessages = &subsonic.Empty{} }))
		},
	}
	for name, serve := range answers {
		subsonicMethods[strings.ToLower(name)] = subsonicHandler{serve: serve}
	}
	// a client asks which extensions the server has before it logs in, to
	// know how it may
	subsonicMethods[strings.ToLower("getOpenSubsonicExtensions")] = subsonicHandler{public: true, serve: func(q *subsonicRequest) {
		q.reply(with(q, func(r *subsonic.Response) {
			r.OpenSubsonicExtensions = []subsonic.Extension{
				{Name: "apiKeyAuthentication", Versions: []int{1}},
				{Name: "formPost", Versions: []int{1}},
			}
		}))
	}}
	for _, name := range []string{"createPlaylist", "updatePlaylist", "deletePlaylist", "createShare", "updateShare",
		"deleteShare", "createUser", "updateUser", "deleteUser", "changePassword", "createInternetRadioStation",
		"updateInternetRadioStation", "deleteInternetRadioStation", "createPodcastChannel", "deletePodcastChannel",
		"deletePodcastEpisode", "downloadPodcastEpisode", "refreshPodcasts", "addChatMessage", "jukeboxControl"} {
		subsonicMethods[strings.ToLower(name)] = subsonicHandler{serve: func(q *subsonicRequest) {
			q.fail(subsonic.ErrNotAuthorized, "go-fs does not let a client "+q.method)
		}}
	}
}

// with is a response that worked, with what fill sets on it.
func with(q *subsonicRequest, fill func(*subsonic.Response)) *subsonic.Response {
	r := q.ok()
	fill(r)
	return r
}

// apiUser is the account as the API describes a user: it streams, downloads,
// sees covers and scrobbles, and changes nothing else.
func (q *subsonicRequest) apiUser() subsonic.User {
	folders := make([]int, len(q.libs))
	for i, l := range q.libs {
		folders[i] = subsonic.FolderID(l)
	}
	return subsonic.User{Username: q.user.name, ScrobblingEnabled: true, DownloadRole: true,
		CoverArtRole: true, StreamRole: true, Folder: folders}
}

func (q *subsonicRequest) getUser() {
	if name := q.str("username"); name != "" && name != q.user.name {
		q.fail(subsonic.ErrNotAuthorized, "an account sees no other")
		return
	}
	user := q.apiUser()
	q.reply(with(q, func(r *subsonic.Response) { r.User = &user }))
}

func (q *subsonicRequest) getUsers() {
	q.reply(with(q, func(r *subsonic.Response) { r.Users = &subsonic.Users{User: []subsonic.User{q.apiUser()}} }))
}

// scanStatus says the libraries are not being read: a repository looks at
// its folder by itself, every pollInterval.
func (q *subsonicRequest) scanStatus() {
	count := 0
	for _, l := range q.libs {
		count += len(l.Tracks)
	}
	q.reply(with(q, func(r *subsonic.Response) { r.ScanStatus = &subsonic.ScanStatus{Count: count} }))
}

func (q *subsonicRequest) musicFolders() {
	folders := make([]subsonic.MusicFolder, len(q.libs))
	for i, l := range q.libs {
		folders[i] = subsonic.Folder(l)
	}
	q.reply(with(q, func(r *subsonic.Response) { r.MusicFolders = &subsonic.MusicFolders{MusicFolder: folders} }))
}

// indexes answers getIndexes: the artists, unless the libraries were not
// built again since ifModifiedSince.
func (q *subsonicRequest) indexes() {
	libs, ok := q.folders()
	if !ok {
		return
	}
	since, ok := q.number("ifModifiedSince", 0)
	if !ok {
		return
	}
	built := subsonic.LastBuilt(libs).UnixMilli()
	indexes := &subsonic.Indexes{LastModified: built, IgnoredArticles: subsonic.IgnoredArticles}
	if int64(since) < built {
		indexes.Index = subsonic.ArtistIndex(libs, q.describer())
	}
	q.reply(with(q, func(r *subsonic.Response) { r.Indexes = indexes }))
}

func (q *subsonicRequest) artists() {
	libs, ok := q.folders()
	if !ok {
		return
	}
	index := subsonic.ArtistIndex(libs, q.describer())
	q.reply(with(q, func(r *subsonic.Response) {
		r.Artists = &subsonic.Indexes{IgnoredArticles: subsonic.IgnoredArticles, Index: index}
	}))
}

func (q *subsonicRequest) directory() {
	item, ok := q.item("id")
	if !ok {
		return
	}
	dir, ok := q.describer().Directory(item)
	if !ok {
		q.fail(subsonic.ErrNotFound, "directory not found: "+item.ItemID())
		return
	}
	q.reply(with(q, func(r *subsonic.Response) { r.Directory = dir }))
}

func (q *subsonicRequest) artist() {
	item, ok := q.item("id")
	if !ok {
		return
	}
	a, isArtist := item.(*jellyfin.Artist)
	if !isArtist {
		q.fail(subsonic.ErrNotFound, "artist not found: "+item.ItemID())
		return
	}
	q.reply(with(q, func(r *subsonic.Response) { r.Artist = q.describer().ArtistWithAlbums(a) }))
}

func (q *subsonicRequest) album() {
	item, ok := q.item("id")
	if !ok {
		return
	}
	a, isAlbum := item.(*jellyfin.Album)
	if !isAlbum {
		q.fail(subsonic.ErrNotFound, "album not found: "+item.ItemID())
		return
	}
	q.reply(with(q, func(r *subsonic.Response) { r.Album = q.describer().AlbumWithSongs(a) }))
}

func (q *subsonicRequest) song() {
	t, ok := q.track("id")
	if !ok {
		return
	}
	song := q.describer().Song(t)
	q.reply(with(q, func(r *subsonic.Response) { r.Song = &song }))
}

func (q *subsonicRequest) genres() {
	genres := subsonic.AllGenres(q.libs)
	q.reply(with(q, func(r *subsonic.Response) { r.Genres = &subsonic.Genres{Genre: genres} }))
}

// page is the page of a list that the parameters count and offset name,
// sizeName being the parameter of its size.
func (q *subsonicRequest) page(sizeName, offsetName string, size int) (subsonic.Page, bool) {
	size, ok := q.number(sizeName, size)
	if !ok {
		return subsonic.Page{}, false
	}
	offset, ok := q.number(offsetName, 0)
	return subsonic.Page{Size: size, Offset: offset}, ok
}

func (q *subsonicRequest) albumList(id3 bool) {
	libs, ok := q.folders()
	if !ok {
		return
	}
	page, ok := q.page("size", "offset", subsonic.DefaultSize)
	if !ok {
		return
	}
	query := subsonic.AlbumQuery{Type: q.str("type"), Page: page, Genre: q.str("genre")}
	if q.str("fromYear") != "" && q.str("toYear") != "" {
		if query.FromYear, ok = q.number("fromYear", 0); !ok {
			return
		}
		if query.ToYear, ok = q.number("toYear", 0); !ok {
			return
		}
		query.HasYears = true
	}
	albums, err := subsonic.FindAlbums(libs, q.s.subsonic.listeners.Data(q.user.name), query)
	if err != nil {
		q.failed(err)
		return
	}
	d := q.describer()
	q.reply(with(q, func(r *subsonic.Response) {
		if id3 {
			r.AlbumList2 = &subsonic.AlbumList2{Album: d.Albums(albums)}
		} else {
			r.AlbumList = &subsonic.AlbumList{Album: d.AlbumDirs(albums)}
		}
	}))
}

func (q *subsonicRequest) randomSongs() {
	libs, ok := q.folders()
	if !ok {
		return
	}
	size, ok := q.number("size", subsonic.DefaultSize)
	if !ok {
		return
	}
	from, ok := q.number("fromYear", 0)
	if !ok {
		return
	}
	to, ok := q.number("toYear", 0)
	if !ok {
		return
	}
	tracks := subsonic.RandomSongs(libs, subsonic.SongQuery{Size: size, Genre: q.str("genre"), FromYear: from, ToYear: to})
	q.reply(with(q, func(r *subsonic.Response) { r.RandomSongs = &subsonic.Songs{Song: q.describer().Songs(tracks)} }))
}

func (q *subsonicRequest) songsByGenre() {
	genre, ok := q.required("genre")
	if !ok {
		return
	}
	libs, ok := q.folders()
	if !ok {
		return
	}
	page, ok := q.page("count", "offset", subsonic.DefaultSize)
	if !ok {
		return
	}
	tracks := subsonic.SongsByGenre(libs, genre, page)
	q.reply(with(q, func(r *subsonic.Response) { r.SongsByGenre = &subsonic.Songs{Song: q.describer().Songs(tracks)} }))
}

func (q *subsonicRequest) starred(id3 bool) {
	libs, ok := q.folders()
	if !ok {
		return
	}
	artists, albums, tracks := subsonic.Favorites(libs, q.s.subsonic.listeners.Data(q.user.name))
	d := q.describer()
	q.reply(with(q, func(r *subsonic.Response) {
		if id3 {
			r.Starred2 = &subsonic.Starred2{Artist: d.Artists(artists), Album: d.Albums(albums), Song: d.Songs(tracks)}
		} else {
			r.Starred = &subsonic.Starred{Artist: d.Artists(artists), Album: d.AlbumDirs(albums), Song: d.Songs(tracks)}
		}
	}))
}

func (q *subsonicRequest) search(id3 bool) {
	libs, ok := q.folders()
	if !ok {
		return
	}
	query := subsonic.SearchQuery{Query: q.str("query")}
	// -1 is a size the client did not send, which is the default
	if query.Artists, ok = q.page("artistCount", "artistOffset", -1); !ok {
		return
	}
	if query.Albums, ok = q.page("albumCount", "albumOffset", -1); !ok {
		return
	}
	if query.Songs, ok = q.page("songCount", "songOffset", -1); !ok {
		return
	}
	artists, albums, tracks := subsonic.Search(libs, query)
	d := q.describer()
	q.reply(with(q, func(r *subsonic.Response) {
		if id3 {
			r.SearchResult3 = &subsonic.Starred2{Artist: d.Artists(artists), Album: d.Albums(albums), Song: d.Songs(tracks)}
		} else {
			r.SearchResult2 = &subsonic.Starred{Artist: d.Artists(artists), Album: d.AlbumDirs(albums), Song: d.Songs(tracks)}
		}
	}))
}

// send sends the file of a track as it is: go-fs transcodes nothing, so the
// format, the bitrate and the offset a client asks for are not heeded.
func (q *subsonicRequest) send(download bool) {
	if t, ok := q.track("id"); ok {
		q.s.serveTrack(q.set, q.w, q.r, q.user, t, download, "subsonic", q.client)
	}
}

// coverArt sends the cover of an item, at its own size. A cover that is not
// there is a 404, for the image loaders of the clients, with the error in
// the body.
func (q *subsonicRequest) coverArt() {
	id, ok := q.required("id")
	if !ok {
		return
	}
	item := jellyfin.Lookup(q.libs, id)
	if item == nil || !q.s.serveCover(q.w, q.r, item, "subsonic") {
		q.replyWith(http.StatusNotFound, subsonic.Failed(q.s.serverVersion(), subsonic.ErrNotFound, "no cover: "+id))
	}
}

// star makes the items id, albumId and artistId name favorites, or no
// longer.
func (q *subsonicRequest) star(favorite bool) {
	ids := slices.Concat(q.params["id"], q.params["albumId"], q.params["artistId"])
	if len(ids) == 0 {
		q.fail(subsonic.ErrMissing, "required parameter is missing: id, albumId or artistId")
		return
	}
	items := make([]jellyfin.Item, 0, len(ids))
	for _, id := range ids {
		item := jellyfin.Lookup(q.libs, id)
		if item == nil {
			q.fail(subsonic.ErrNotFound, "not found: "+id)
			return
		}
		items = append(items, item)
	}
	now := time.Now()
	for _, item := range items {
		_, err := q.s.subsonic.listeners.Update(q.user.name, item.ItemID(), func(r *jellyfin.Record) {
			if favorite && !r.Favorite {
				r.FavoriteAt = now
			}
			r.Favorite = favorite
			if !favorite {
				r.FavoriteAt = time.Time{}
			}
		})
		if err != nil {
			q.s.log.Warn("subsonic cannot keep a favorite in the persistent data file", "user", q.user.name,
				"item", item.ItemID(), "error", err)
			q.fail(subsonic.ErrGeneric, "the favorite cannot be kept")
			return
		}
	}
	q.reply(q.ok())
}

// scrobble counts plays of tracks a client says it played, at the times it
// says, or now. A client that only says it began to play a track, with
// submission=false, has nothing counted: the play is counted when it says
// it played the track.
func (q *subsonicRequest) scrobble() {
	ids := q.params["id"]
	if len(ids) == 0 {
		q.fail(subsonic.ErrMissing, "required parameter is missing: id")
		return
	}
	tracks := make([]*jellyfin.Track, 0, len(ids))
	for _, id := range ids {
		t, ok := q.trackOf(id)
		if !ok {
			return
		}
		tracks = append(tracks, t)
	}
	if q.str("submission") == "false" {
		q.reply(q.ok())
		return
	}
	times := q.params["time"]
	for i, t := range tracks {
		at := time.Now()
		if i < len(times) {
			ms, err := strconv.ParseInt(times[i], 10, 64)
			if err != nil {
				q.fail(subsonic.ErrGeneric, "parameter time is not a number: "+times[i])
				return
			}
			at = time.UnixMilli(ms)
		}
		if err := q.s.subsonic.listeners.MarkPlayed(q.user.name, []*jellyfin.Track{t}, at); err != nil {
			q.s.log.Warn("subsonic cannot keep a play in the persistent data file", "user", q.user.name,
				"file", t.Virtual(), "error", err)
			q.fail(subsonic.ErrGeneric, "the play cannot be kept")
			return
		}
	}
	q.reply(q.ok())
}

// trackOf finds a track the account may read by its id.
func (q *subsonicRequest) trackOf(id string) (*jellyfin.Track, bool) {
	t, isTrack := jellyfin.Lookup(q.libs, id).(*jellyfin.Track)
	if !isTrack || !musicReadable(q.user, t.Virtual()) {
		q.fail(subsonic.ErrNotFound, "song not found: "+id)
		return nil, false
	}
	return t, true
}
