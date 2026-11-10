//go:build !light && !no_jellyfin

package httpd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/jellyfin"
)

// serve answers a request of a client that logged in.
func (j *jellyfinRequest) serve() {
	w, r := j.w, j.r
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	post := r.Method == http.MethodPost
	switch {
	case get && j.is("system", "info"):
		writeJSON(w, http.StatusOK, j.info())
	case get && (j.is("users", "me") || j.is("users", "*")):
		writeJSON(w, http.StatusOK, j.userInfo())
	case get && j.is("users"):
		writeJSON(w, http.StatusOK, []any{j.userInfo()})

	case get && (j.is("userviews") || j.is("library", "mediafolders")):
		items := make([]jellyfin.Item, len(j.libs))
		for i, l := range j.libs {
			items[i] = l
		}
		j.page(items, len(items), 0)
	case get && (j.is("items") || j.is("useritems")):
		j.items(j.query())
	case get && (j.is("items", "latest") || j.is("useritems", "latest")):
		j.latest()
	case get && (j.is("items", "resume") || j.is("useritems", "resume")):
		j.resume()
	case get && j.is("items", "counts"):
		j.counts()
	case get && j.is("items", "*"):
		j.item(j.path[1])
	case get && j.is("items", "*", "ancestors"):
		j.ancestors(j.path[1])
	case (get || post) && (j.is("items", "*", "userdata") || j.is("useritems", "*", "userdata")):
		j.itemUserData(j.path[1], post)

	case get && j.is("artists"):
		q := j.query()
		q.Types = []string{"MusicArtist"}
		j.items(q)
	case get && j.is("artists", "albumartists"):
		q := j.query()
		q.Types, q.AlbumArtists = []string{"MusicArtist"}, true
		j.items(q)
	case get && (j.is("genres") || j.is("musicgenres")):
		q := j.query()
		q.Types = []string{"MusicGenre"}
		j.items(q)
	case get && (j.is("artists", "*") || j.is("genres", "*") || j.is("musicgenres", "*")):
		j.named(j.path[0], j.path[1])
	case get && j.is("search", "hints"):
		j.hints()

	case get && j.is("audio", "*", "*") && (strings.HasPrefix(strings.ToLower(j.path[2]), "stream") ||
		strings.EqualFold(j.path[2], "universal")):
		j.send(j.path[1], false)
	case get && j.is("audio", "*", "*") && (strings.EqualFold(j.path[2], "main.m3u8") ||
		strings.EqualFold(j.path[2], "master.m3u8")):
		j.hls(j.path[1])
	case get && (j.is("items", "*", "download") || j.is("items", "*", "file")):
		j.send(j.path[1], true)
	case (get || post) && j.is("items", "*", "playbackinfo"):
		j.playbackInfo(j.path[1])

	case post && (j.is("sessions", "playing") || j.is("sessions", "playing", "progress") ||
		j.is("sessions", "playing", "stopped")):
		j.playing(strings.ToLower(j.path[len(j.path)-1]))
	case post && j.is("playingitems", "*"):
		j.report("playing", j.path[1], j.positionParam())
	case post && j.is("playingitems", "*", "progress"):
		j.report("progress", j.path[1], j.positionParam())
	case r.Method == http.MethodDelete && j.is("playingitems", "*"):
		j.report("stopped", j.path[1], j.positionParam())
	case post && (j.is("sessions", "playing", "ping") || j.is("sessions", "capabilities") ||
		j.is("sessions", "capabilities", "full")):
		w.WriteHeader(http.StatusNoContent)
	case post && j.is("sessions", "logout"):
		j.logout()
	case get && j.is("sessions"):
		writeJSON(w, http.StatusOK, []any{j.session()})
	case (post || r.Method == http.MethodDelete) && (j.is("userfavoriteitems", "*") || j.is("userplayeditems", "*")):
		j.userData(j.path[1], post)
	case get && j.is("displaypreferences", "*"):
		j.displayPreferences(j.path[1])
	case post && j.is("displaypreferences", "*"):
		j.keepDisplayPreferences(j.path[1])
	case get && (j.is("items", "*", "instantmix") || j.is("items", "*", "similar") ||
		j.is("albums", "*", "similar") || j.is("artists", "*", "similar") ||
		j.is("albums", "*", "instantmix") || j.is("artists", "instantmix") ||
		j.is("musicgenres", "instantmix") || j.is("songs", "*", "instantmix") ||
		j.is("studios") || j.is("persons") || j.is("playlists")):
		j.page(nil, 0, 0)

	default:
		j.s.log.Debug("jellyfin unknown endpoint", "method", r.Method, "url", r.URL.Path,
			"client", j.client.Client)
		http.NotFound(w, r)
	}
}

// page answers a list of items as Jellyfin answers one.
func (j *jellyfinRequest) page(items []jellyfin.Item, total, start int) {
	out := j.describer().Items(items)
	writeJSON(j.w, http.StatusOK, map[string]any{"Items": out, "TotalRecordCount": total, "StartIndex": start})
}

// query reads the parameters of a list of items.
func (j *jellyfinRequest) query() jellyfin.Query {
	p := j.params
	q := jellyfin.Query{
		ParentID:       jellyfinIDOf(p["parentid"]),
		Types:          list(p["includeitemtypes"], ",|"),
		Recursive:      strings.EqualFold(p["recursive"], "true"),
		IDs:            ids(p["ids"]),
		Search:         strings.TrimSpace(p["searchterm"]),
		NameStartsWith: p["namestartswith"],
		ArtistIDs:      append(ids(p["artistids"]), ids(p["contributingartistids"])...),
		AlbumArtistIDs: ids(p["albumartistids"]),
		AlbumIDs:       ids(p["albumids"]),
		GenreIDs:       ids(p["genreids"]),
		Genres:         list(p["genres"], "|"),
		SortBy:         list(p["sortby"], ","),
		Data:           j.data(),
	}
	if favorite, err := strconv.ParseBool(p["isfavorite"]); err == nil {
		q.Favorite = &favorite
	}
	if played, err := strconv.ParseBool(p["isplayed"]); err == nil {
		q.Played = &played
	}
	for _, year := range list(p["years"], ",") {
		if n, err := strconv.Atoi(year); err == nil {
			q.Years = append(q.Years, n)
		}
	}
	for _, order := range list(p["sortorder"], ",") {
		q.Descending = append(q.Descending, strings.EqualFold(order, "Descending"))
	}
	yes, no := true, false
	for _, filter := range list(p["filters"], ",") {
		// a favorite is what Jellyfin calls a like as well
		switch strings.ToLower(filter) {
		case "isfavorite", "likes", "isfavoriteorlikes":
			q.Favorite = &yes
		case "isplayed":
			q.Played = &yes
		case "isunplayed":
			q.Played = &no
		case "isresumable":
			q.Resumable = true
		case "isnotfolder":
			// what a mix of an artist or a genre is played from
			if len(q.Types) == 0 {
				q.Types = []string{"Audio"}
			}
		}
	}
	if len(q.Types) == 0 && strings.EqualFold(p["mediatypes"], "Audio") {
		q.Types = []string{"Audio"}
	}
	q.Start, _ = strconv.Atoi(p["startindex"])
	q.Limit, _ = strconv.Atoi(p["limit"])
	return q
}

func (j *jellyfinRequest) items(q jellyfin.Query) {
	items, total := jellyfin.Find(j.libs, q)
	if len(q.IDs) > total {
		// a client asks for what it kept, such as its queue, which may be
		// of another server or of files no longer there
		j.s.log.Debug("jellyfin items not found", "user", j.user.name, "asked", len(q.IDs), "found", total)
	}
	j.page(items, total, min(max(q.Start, 0), total))
}

// latest answers the albums added last, which is what a music library shows
// of what is new, as a bare list.
func (j *jellyfinRequest) latest() {
	limit, err := strconv.Atoi(j.params["limit"])
	if err != nil || limit <= 0 {
		limit = 20
	}
	items, _ := jellyfin.Find(j.libs, jellyfin.Query{ParentID: jellyfinIDOf(j.params["parentid"]),
		Types: []string{"MusicAlbum"}, Recursive: true, SortBy: []string{"DateCreated"},
		Descending: []bool{true}, Limit: limit})
	writeJSON(j.w, http.StatusOK, j.describer().Items(items))
}

// resume answers the tracks that are resumed where they were stopped, the
// one played last first.
func (j *jellyfinRequest) resume() {
	q := j.query()
	audio := func(kind string) bool { return strings.EqualFold(kind, "Audio") }
	if media := list(j.params["mediatypes"], ","); len(media) > 0 && !slices.ContainsFunc(media, audio) ||
		len(q.Types) > 0 && !slices.ContainsFunc(q.Types, audio) {
		j.page(nil, 0, 0)
		return
	}
	q.Types, q.Recursive, q.Resumable = []string{"Audio"}, true, true
	if len(q.SortBy) == 0 {
		q.SortBy, q.Descending = []string{"DatePlayed"}, []bool{true}
	}
	j.items(q)
}

func (j *jellyfinRequest) counts() {
	counts := map[string]int{"SongCount": 0, "AlbumCount": 0, "ArtistCount": 0, "MovieCount": 0,
		"SeriesCount": 0, "EpisodeCount": 0, "MusicVideoCount": 0, "BoxSetCount": 0, "BookCount": 0,
		"ItemCount": 0, "ProgramCount": 0, "TrailerCount": 0}
	for _, l := range j.libs {
		counts["SongCount"] += len(l.Tracks)
		counts["AlbumCount"] += len(l.Albums)
		counts["ArtistCount"] += len(l.Artists)
		counts["ItemCount"] += len(l.Tracks) + len(l.Albums)
	}
	writeJSON(j.w, http.StatusOK, counts)
}

// lookup finds an item the account may see, and answers 404 for one it may
// not or that is not there.
func (j *jellyfinRequest) lookup(id string) (jellyfin.Item, bool) {
	item := jellyfin.Lookup(j.libs, jellyfinIDOf(id))
	if item == nil {
		http.NotFound(j.w, j.r)
		return nil, false
	}
	return item, true
}

func (j *jellyfinRequest) item(id string) {
	if item, ok := j.lookup(id); ok {
		writeJSON(j.w, http.StatusOK, j.describer().Item(item))
	}
}

// ancestors are the album of a track and the library of an item.
func (j *jellyfinRequest) ancestors(id string) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	var parents []jellyfin.Item
	if t, ok := item.(*jellyfin.Track); ok {
		parents = append(parents, t.Album)
	}
	if _, ok := item.(*jellyfin.Library); !ok {
		parents = append(parents, item.Library())
	}
	writeJSON(j.w, http.StatusOK, j.describer().Items(parents))
}

// named answers an artist or a genre by its name, as /Artists/{name} asks for
// one.
func (j *jellyfinRequest) named(kind, name string) {
	for _, l := range j.libs {
		if strings.EqualFold(kind, "artists") {
			for _, a := range l.Artists {
				if strings.EqualFold(a.Name, name) {
					writeJSON(j.w, http.StatusOK, j.describer().Item(a))
					return
				}
			}
			continue
		}
		for _, g := range l.Genres {
			if strings.EqualFold(g.Name, name) {
				writeJSON(j.w, http.StatusOK, j.describer().Item(g))
				return
			}
		}
	}
	http.NotFound(j.w, j.r)
}

// hints answers a search as /Search/Hints answers one.
func (j *jellyfinRequest) hints() {
	q := j.query()
	if len(q.Types) == 0 {
		q.Types = []string{"MusicArtist", "MusicAlbum", "Audio"}
	}
	q.Recursive = true
	items, total := jellyfin.Find(j.libs, q)
	hints := make([]map[string]any, len(items))
	for i, item := range j.describer().Items(items) {
		hint := map[string]any{"ItemId": item.ID, "Id": item.ID, "Name": item.Name, "Type": item.Type,
			"IsFolder": item.IsFolder, "MediaType": item.MediaType, "RunTimeTicks": item.RunTimeTicks,
			"Album": item.Album, "AlbumId": item.AlbumID, "AlbumArtist": item.AlbumArtist,
			"Artists": item.Artists, "ProductionYear": item.ProductionYear, "IndexNumber": item.IndexNumber,
			"ParentIndexNumber": item.ParentIndexNumber}
		if tag := item.ImageTags["Primary"]; tag != "" {
			hint["PrimaryImageTag"] = tag
			if h := item.ImageBlurHashes["Primary"][tag]; h != "" {
				hint["BlurHash"] = h
			}
		}
		hints[i] = hint
	}
	writeJSON(j.w, http.StatusOK, map[string]any{"SearchHints": hints, "TotalRecordCount": total})
}

// image answers the cover of an item: the image beside its tracks, or the
// picture a track carries. It is sent as it is; the size a client asks for is
// not made. serveJellyfin answers it before it asks for an account.
func (j *jellyfinRequest) image(id, kind string) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	if !strings.EqualFold(kind, "Primary") || !j.s.serveCover(j.w, j.r, item, "jellyfin") {
		http.NotFound(j.w, j.r)
	}
}

// track finds a track the account may read.
func (j *jellyfinRequest) track(id string) (*jellyfin.Track, bool) {
	item, ok := j.lookup(id)
	if !ok {
		return nil, false
	}
	t, isTrack := item.(*jellyfin.Track)
	if !isTrack || !j.readable(t.Virtual()) {
		http.NotFound(j.w, j.r)
		return nil, false
	}
	return t, true
}

// send sends the file of a track as it is, which is how every stream is
// played here, with ranges so that a player can seek. download names it for
// saving.
func (j *jellyfinRequest) send(id string, download bool) {
	if t, ok := j.track(id); ok {
		j.s.serveTrack(j.set, j.w, j.r, j.user, t, download, "jellyfin", j.client.Client)
	}
}

// hls answers a client that asks for a stream to be transcoded and sent in
// segments, which nothing here does: the playlist of one segment, the file
// as it is, which players take for MP3 and for AAC without a container. Of
// any other file they take no segment, and the client has to be told to play
// it as it is.
func (j *jellyfinRequest) hls(id string) {
	t, ok := j.track(id)
	if !ok {
		return
	}
	if t.Container != "mp3" && t.Container != "aac" {
		j.s.log.Debug("jellyfin cannot stream as HLS without transcoding, turn transcoding off in the client",
			"user", j.user.name, "file", t.Virtual(), "container", t.Container, "client", j.client.Client)
		http.Error(j.w, "this server does not transcode: play the file as it is", http.StatusUnsupportedMediaType)
		return
	}
	// the segment is fetched by itself, so it carries the token the
	// playlist is asked for with
	seconds := t.Duration.Seconds()
	playlist := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n"+
		"#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:%.3f,\nstream.%s?ApiKey=%s\n#EXT-X-ENDLIST\n",
		max(1, int(math.Ceil(seconds))), seconds, t.Container, url.QueryEscape(j.client.Token))
	j.w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	j.w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(j.w, playlist)
}

// playbackInfo says how a track is played: as its file is.
func (j *jellyfinRequest) playbackInfo(id string) {
	t, ok := j.track(id)
	if !ok {
		return
	}
	session := make([]byte, 16)
	_, _ = rand.Read(session)
	writeJSON(j.w, http.StatusOK, map[string]any{
		"MediaSources":  []jellyfin.MediaSource{j.describer().MediaSource(t)},
		"PlaySessionId": hex.EncodeToString(session),
	})
}

// jellyfinBodyLimit is the most of a body of a report or a view that is
// read.
const jellyfinBodyLimit = 64 << 10

// playing takes a report of what a client plays, in the body Jellyfin gives
// it: PlaybackStartInfo, PlaybackProgressInfo or PlaybackStopInfo.
func (j *jellyfinRequest) playing(event string) {
	var body struct {
		ItemID        string `json:"ItemId"`
		PositionTicks *int64
	}
	err := json.NewDecoder(io.LimitReader(j.r.Body, jellyfinBodyLimit)).Decode(&body)
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(j.w, "the body is not the JSON of a playback report", http.StatusBadRequest)
		return
	}
	j.report(event, body.ItemID, body.PositionTicks)
}

// positionParam is where a client is in a track, as the older reports put
// it in the query; nil when they do not.
func (j *jellyfinRequest) positionParam() *int64 {
	position, err := strconv.ParseInt(j.params["positionticks"], 10, 64)
	if err != nil {
		return nil
	}
	return &position
}

// report takes a report of a client that started playing a track, is at a
// position in it, or stopped. One of no track, as a client sends while it
// plays nothing, is taken and forgotten.
func (j *jellyfinRequest) report(event, id string, position *int64) {
	if id == "" {
		j.w.WriteHeader(http.StatusNoContent)
		return
	}
	t, ok := j.track(id)
	if !ok {
		return
	}
	report := jellyfin.Report{User: j.user.name, Device: j.client.DeviceID, Client: j.client.Client, Track: t}
	if position != nil {
		report.Position, report.HasPosition = time.Duration(*position)*100, true
	}
	listeners := j.s.jellyfin.listeners
	var err error
	switch event {
	case "playing":
		err = listeners.Started(report)
	case "progress":
		err = listeners.Progress(report)
	case "stopped":
		err = listeners.Stopped(report)
	}
	if err != nil {
		j.s.log.Warn("jellyfin cannot keep what was played in the persistent data file", "user", j.user.name,
			"file", t.Virtual(), "error", err)
	}
	j.w.WriteHeader(http.StatusNoContent)
}

// userData marks an item a favorite, or played, or takes that back. Marking
// an album played marks its tracks.
func (j *jellyfinRequest) userData(id string, set bool) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	var err error
	if strings.EqualFold(j.path[0], "userfavoriteitems") {
		_, err = j.s.jellyfin.listeners.Update(j.user.name, item.ItemID(),
			func(r *jellyfin.Record) { r.Favorite = set })
	} else {
		var date time.Time
		if value := j.params["dateplayed"]; set && value != "" {
			if date, err = jellyfinDate(value); err != nil {
				http.Error(j.w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		err = j.markPlayed(item, set, date)
	}
	if err != nil {
		j.userDataFailed(item, err)
		return
	}
	writeJSON(j.w, http.StatusOK, j.describer().UserData(item))
}

// itemUserData answers what is kept of the account and an item, and changes
// it as UpdateUserItemDataDto says, field by field: what the body leaves out
// stays as it is.
func (j *jellyfinRequest) itemUserData(id string, update bool) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	if !update {
		writeJSON(j.w, http.StatusOK, j.describer().UserData(item))
		return
	}
	var body struct {
		IsFavorite            *bool
		Played                *bool
		PlaybackPositionTicks *int64
		PlayCount             *int
		LastPlayedDate        *string
	}
	if err := json.NewDecoder(io.LimitReader(j.r.Body, jellyfinBodyLimit)).Decode(&body); err != nil {
		http.Error(j.w, "the body is not the JSON of user data", http.StatusBadRequest)
		return
	}
	var last time.Time
	if body.LastPlayedDate != nil && *body.LastPlayedDate != "" {
		var err error
		if last, err = jellyfinDate(*body.LastPlayedDate); err != nil {
			http.Error(j.w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// what is played of an album, an artist or a genre is what is played
	// of its tracks
	if _, isTrack := item.(*jellyfin.Track); !isTrack && body.Played != nil {
		if err := j.markPlayed(item, *body.Played, time.Time{}); err != nil {
			j.userDataFailed(item, err)
			return
		}
		body.Played = nil
	}
	_, err := j.s.jellyfin.listeners.Update(j.user.name, item.ItemID(), func(r *jellyfin.Record) {
		if body.IsFavorite != nil {
			r.Favorite = *body.IsFavorite
		}
		if body.Played != nil {
			r.Played = *body.Played
		}
		if body.PlaybackPositionTicks != nil {
			r.Position = max(*body.PlaybackPositionTicks, 0)
		}
		if body.PlayCount != nil {
			r.PlayCount = max(*body.PlayCount, 0)
		}
		if body.LastPlayedDate != nil {
			r.LastPlayed = last
		}
	})
	if err != nil {
		j.userDataFailed(item, err)
		return
	}
	writeJSON(j.w, http.StatusOK, j.describer().UserData(item))
}

// markPlayed marks the tracks of an item played, on date when it is not
// zero, or not played.
func (j *jellyfinRequest) markPlayed(item jellyfin.Item, played bool, date time.Time) error {
	tracks := jellyfin.TracksOf(item)
	if played {
		return j.s.jellyfin.listeners.MarkPlayed(j.user.name, tracks, date)
	}
	return j.s.jellyfin.listeners.MarkUnplayed(j.user.name, tracks)
}

func (j *jellyfinRequest) userDataFailed(item jellyfin.Item, err error) {
	j.s.log.Warn("jellyfin cannot keep user data in the persistent data file", "user", j.user.name,
		"item", item.ItemName(), "error", err)
	http.Error(j.w, "the user data cannot be kept", http.StatusInternalServerError)
}

// jellyfinDate reads a date as a client sends one: in ISO 8601, with or
// without its zone, which is UTC then.
func jellyfinDate(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date", value)
}

// preferencesKey is where the views a client sets for an id are kept: by
// account, by id and by the client the query names, or the one that asks.
func (j *jellyfinRequest) preferencesKey(id string) string {
	client := j.params["client"]
	if client == "" {
		client = j.client.Client
	}
	return j.user.name + "\x00" + id + "\x00" + client
}

// displayPreferences answers the views a client set for an id, or those it
// starts with.
func (j *jellyfinRequest) displayPreferences(id string) {
	var kept json.RawMessage
	found, err := j.s.jellyfin.preferences.Get(j.preferencesKey(id), &kept)
	if err != nil {
		j.s.log.Warn("jellyfin cannot read display preferences from the persistent data file",
			"user", j.user.name, "error", err)
	}
	if found {
		writeJSON(j.w, http.StatusOK, kept)
		return
	}
	writeJSON(j.w, http.StatusOK, map[string]any{"Id": id, "SortBy": "SortName", "SortOrder": "Ascending",
		"RememberIndexing": false, "RememberSorting": false, "PrimaryImageHeight": 250,
		"PrimaryImageWidth": 250, "ScrollDirection": "Horizontal", "ShowBackdrop": false,
		"ShowSidebar": false, "IndexBy": "", "CustomPrefs": map[string]string{}, "Client": j.client.Client})
}

// keepDisplayPreferences keeps the views a client sets for an id, as it
// sends them.
func (j *jellyfinRequest) keepDisplayPreferences(id string) {
	body, err := io.ReadAll(io.LimitReader(j.r.Body, jellyfinBodyLimit+1))
	if err != nil {
		http.Error(j.w, "the body cannot be read", http.StatusBadRequest)
		return
	}
	if len(body) > jellyfinBodyLimit {
		http.Error(j.w, "the display preferences are too large", http.StatusRequestEntityTooLarge)
		return
	}
	var prefs map[string]any
	if err := json.Unmarshal(body, &prefs); err != nil || prefs == nil {
		http.Error(j.w, "the body is not the JSON of display preferences", http.StatusBadRequest)
		return
	}
	if err := j.s.jellyfin.preferences.Put(j.preferencesKey(id), json.RawMessage(body)); err != nil {
		j.s.log.Warn("jellyfin cannot keep display preferences in the persistent data file",
			"user", j.user.name, "error", err)
	}
	j.w.WriteHeader(http.StatusNoContent)
}

// logout takes back the token the client logged out with, until it would
// have run out. Without a persistent data file it cannot be: it stays good,
// as a token did before there was one.
func (j *jellyfinRequest) logout() {
	claims, _ := j.s.jellyfinClaims(j.set, j.client.Token)
	switch {
	case claims == nil || claims.ID == "" || claims.ExpiresAt == nil:
	case j.s.jellyfin.revoked == nil:
		j.s.log.Debug("jellyfin logout without a persistent data file, the token stays good",
			"user", j.user.name, "client", j.client.Client)
	default:
		if err := j.s.jellyfin.revoked.PutUntil(claims.ID, struct{}{}, claims.ExpiresAt.Time); err != nil {
			j.s.log.Warn("jellyfin cannot keep a token logged out with in the persistent data file",
				"user", j.user.name, "error", err)
		}
	}
	j.s.log.Info("jellyfin logout", "user", j.user.name, "client", j.client.Client, "device", j.client.Device,
		"address", clientAddress(j.set, j.r))
	j.w.WriteHeader(http.StatusNoContent)
}

// list splits a parameter at any of seps, leaving out what is empty.
func list(value, seps string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func ids(value string) []string {
	out := list(value, ",|")
	for i, id := range out {
		out[i] = jellyfinIDOf(id)
	}
	return out
}

// jellyfinIDOf is an id as the libraries have it: a client may send it as a
// GUID, with dashes, and in capitals.
func jellyfinIDOf(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}
