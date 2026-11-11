//go:build !light && (!no_jellyfin || !no_subsonic)

package httpd

import (
	"bytes"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/jellyfin"
	"github.com/mailsvb/go-fs/internal/logging"
)

// What the Jellyfin API and the OpenSubsonic API both do with the music
// libraries: decide what an account sees of them, and send the files of their
// tracks and the images of their covers.

// musicReadable reports whether an account may read a track of a library, as
// S3 decides it: by its paths and its right to retrieve files, whether or not
// the path is public, since a client that logged in reads as its account.
func musicReadable(user *account, virtual string) bool {
	return user.allows(virtual) && granted(user.perms, actRead)
}

// musicLibraries are the libraries of the repositories of type kind, as much
// of each as the account may see; a library of which it may see nothing is
// not shown.
func musicLibraries(kind string, user *account) []*jellyfin.Library {
	key := fmt.Sprintf("%s\x00%v\x00%+v", user.name, user.paths, user.perms)
	readable := func(virtual string) bool { return musicReadable(user, virtual) }
	var out []*jellyfin.Library
	for _, l := range jellyfin.Libraries(kind) {
		view := l.For(key, readable)
		if len(view.Tracks) == 0 && !readable(l.Folder) {
			continue
		}
		out = append(out, view)
	}
	return out
}

// serveTrack sends the file of a track as it is, which is how every stream is
// played here, with ranges so that a player can seek. download names it for
// saving. via names the API and client the program that asks, for the log.
func (s *Server) serveTrack(set *settings, w http.ResponseWriter, r *http.Request, user *account,
	t *jellyfin.Track, download bool, via, client string) {
	file, err := logging.Open(s.logFor(r), t.OnDisk())
	if err != nil {
		s.logFor(r).Warn(via+" cannot open a track", "file", t.Virtual(), "error", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	name := path.Base(t.Href)
	if download {
		w.Header().Set("Content-Disposition", disposition(name))
	}
	w.Header().Set("Content-Type", typeOf(name).Media)
	started := time.Now()
	http.ServeContent(w, r, name, info.ModTime(), file)
	if recorder, ok := w.(*responseRecorder); ok && (r.Method == http.MethodHead ||
		recorder.status != http.StatusOK && recorder.status != http.StatusPartialContent) {
		return
	}
	bytesSent := info.Size()
	if recorder, ok := w.(*responseRecorder); ok {
		bytesSent = recorder.bytes
	}
	s.logFor(r).Info("http download", "user", user.name, "file", t.Virtual(), "bytes", bytesSent,
		"size", info.Size(), "via", via, "client", client,
		"address", clientAddress(set, r), "took", time.Since(started).Round(time.Millisecond))
}

// serveCover sends the image of an item: the image file beside its tracks,
// or the picture a track carries. ok is false for an item without one, of
// which nothing was sent.
func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, item jellyfin.Item, via string) bool {
	file, track, ok := jellyfin.CoverOf(item)
	if !ok {
		return false
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if file != "" {
		f, err := logging.Open(s.logFor(r), file)
		if err != nil {
			http.NotFound(w, r)
			return true
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return true
		}
		w.Header().Set("Content-Type", typeOf(file).Media)
		http.ServeContent(w, r, path.Base(file), info.ModTime(), f)
		return true
	}
	picture, media, err := audio.Picture(track.OnDisk())
	if err != nil {
		s.logFor(r).Debug(via+" cannot read the picture of a track", "file", track.Virtual(), "error", err)
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", media)
	http.ServeContent(w, r, "", track.ModTime, bytes.NewReader(picture))
	return true
}
