//go:build !light && !no_jellyfin

package httpd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
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
		j.page(nil, 0, 0)
	case get && j.is("items", "counts"):
		j.counts()
	case get && j.is("items", "*"):
		j.item(j.path[1])
	case get && j.is("items", "*", "ancestors"):
		j.ancestors(j.path[1])

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

	case get && (j.is("items", "*", "images", "*") || j.is("items", "*", "images", "*", "*")):
		j.image(j.path[1], j.path[3])
	case get && j.is("audio", "*", "*") && (strings.HasPrefix(strings.ToLower(j.path[2]), "stream") ||
		strings.EqualFold(j.path[2], "universal")):
		j.send(j.path[1], false)
	case get && (j.is("items", "*", "download") || j.is("items", "*", "file")):
		j.send(j.path[1], true)
	case (get || post) && j.is("items", "*", "playbackinfo"):
		j.playbackInfo(j.path[1])

	case post && (j.is("sessions", "playing") || j.is("sessions", "playing", "progress") ||
		j.is("sessions", "playing", "stopped") || j.is("sessions", "playing", "ping") ||
		j.is("sessions", "capabilities") || j.is("sessions", "capabilities", "full") ||
		j.is("sessions", "logout")):
		// nothing is kept of what a client plays, and a token cannot be
		// taken back: it is good until it runs out or the password changes
		j.s.log.Debug("jellyfin report", "user", j.user.name, "url", r.URL.Path, "client", j.client.Client)
		w.WriteHeader(http.StatusNoContent)
	case get && j.is("sessions"):
		writeJSON(w, http.StatusOK, []any{j.session()})
	case (post || r.Method == http.MethodDelete) && (j.is("userfavoriteitems", "*") || j.is("userplayeditems", "*")):
		j.userData(j.path[1], post)
	case get && j.is("displaypreferences", "*"):
		writeJSON(w, http.StatusOK, map[string]any{"Id": j.path[1], "SortBy": "SortName", "SortOrder": "Ascending",
			"RememberIndexing": false, "RememberSorting": false, "PrimaryImageHeight": 250,
			"PrimaryImageWidth": 250, "ScrollDirection": "Horizontal", "ShowBackdrop": false,
			"ShowSidebar": false, "IndexBy": "", "CustomPrefs": map[string]string{}, "Client": j.client.Client})
	case post && j.is("displaypreferences", "*"):
		w.WriteHeader(http.StatusNoContent)
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
		Favorites:      strings.EqualFold(p["isfavorite"], "true"),
	}
	for _, year := range list(p["years"], ",") {
		if n, err := strconv.Atoi(year); err == nil {
			q.Years = append(q.Years, n)
		}
	}
	for _, order := range list(p["sortorder"], ",") {
		q.Descending = append(q.Descending, strings.EqualFold(order, "Descending"))
	}
	for _, filter := range list(p["filters"], ",") {
		// nothing was ever played, and nothing is a favorite
		switch strings.ToLower(filter) {
		case "isfavorite", "isplayed", "isresumable", "likes", "isfavoriteorlikes":
			q.Favorites = true
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
		}
		hints[i] = hint
	}
	writeJSON(j.w, http.StatusOK, map[string]any{"SearchHints": hints, "TotalRecordCount": total})
}

// image answers the cover of an item: the image beside its tracks, or the
// picture a track carries. It is sent as it is; the size a client asks for is
// not made.
func (j *jellyfinRequest) image(id, kind string) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	file, track, ok := jellyfin.CoverOf(item)
	if !ok || !strings.EqualFold(kind, "Primary") {
		http.NotFound(j.w, j.r)
		return
	}
	j.w.Header().Set("Cache-Control", "private, max-age=86400")
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			http.NotFound(j.w, j.r)
			return
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			http.NotFound(j.w, j.r)
			return
		}
		j.w.Header().Set("Content-Type", typeOf(file).Media)
		http.ServeContent(j.w, j.r, path.Base(file), info.ModTime(), f)
		return
	}
	picture, media, err := audio.Picture(track.OnDisk())
	if err != nil {
		j.s.log.Debug("jellyfin cannot read the picture of a track", "file", track.Virtual(), "error", err)
		http.NotFound(j.w, j.r)
		return
	}
	j.w.Header().Set("Content-Type", media)
	http.ServeContent(j.w, j.r, "", track.ModTime, bytes.NewReader(picture))
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
	t, ok := j.track(id)
	if !ok {
		return
	}
	file, err := os.Open(t.OnDisk())
	if err != nil {
		j.s.log.Warn("jellyfin cannot open a track", "file", t.Virtual(), "error", err)
		http.NotFound(j.w, j.r)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(j.w, j.r)
		return
	}
	name := path.Base(t.Href)
	if download {
		j.w.Header().Set("Content-Disposition", disposition(name))
	}
	j.w.Header().Set("Content-Type", typeOf(name).Media)
	started := time.Now()
	http.ServeContent(j.w, j.r, name, info.ModTime(), file)
	if recorder, ok := j.w.(*responseRecorder); ok && (j.r.Method == http.MethodHead ||
		recorder.status != http.StatusOK && recorder.status != http.StatusPartialContent) {
		return
	}
	bytesSent := info.Size()
	if recorder, ok := j.w.(*responseRecorder); ok {
		bytesSent = recorder.bytes
	}
	j.s.log.Info("http download", "user", j.user.name, "file", t.Virtual(), "bytes", bytesSent,
		"size", info.Size(), "via", "jellyfin", "client", j.client.Client,
		"address", clientAddress(j.set, j.r), "took", time.Since(started).Round(time.Millisecond))
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

// userData answers marking an item a favorite, or played, as if it were
// kept, which it is not.
func (j *jellyfinRequest) userData(id string, set bool) {
	item, ok := j.lookup(id)
	if !ok {
		return
	}
	data := jellyfin.UserData{Key: item.ItemID(), ItemID: item.ItemID()}
	if strings.EqualFold(j.path[0], "userfavoriteitems") {
		data.IsFavorite = set
	} else {
		data.Played = set
	}
	writeJSON(j.w, http.StatusOK, data)
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
