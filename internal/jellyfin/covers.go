package jellyfin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
)

// Cover is the image of an album that is beside its tracks on disk.
type Cover struct {
	// Name is the image's name in its folder.
	Name    string
	ModTime time.Time
}

// coverNames are what an image beside the tracks is called to be their
// cover, the preferred first, and coverTypes what it may be.
var (
	coverNames = []string{"cover", "folder", "front", "album"}
	coverTypes = []string{".jpg", ".jpeg", ".png", ".webp"}
)

// Covers finds the cover beside the tracks in each of dirs, folders of the
// repository's folder onDisk by their path in it with forward slashes, "."
// for the folder itself. A folder without one is left out.
func Covers(onDisk string, dirs []string) map[string]Cover {
	found := map[string]Cover{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(joinDisk(onDisk, dir))
		if err != nil {
			continue
		}
		best, rank := Cover{}, len(coverNames)*len(coverTypes)
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			ext := strings.ToLower(path.Ext(e.Name()))
			base := strings.ToLower(strings.TrimSuffix(e.Name(), path.Ext(e.Name())))
			n, t := slices.Index(coverNames, base), slices.Index(coverTypes, ext)
			if n < 0 || t < 0 || n*len(coverTypes)+t >= rank {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			best, rank = Cover{Name: e.Name(), ModTime: info.ModTime()}, n*len(coverTypes)+t
		}
		if best.Name != "" {
			found[dir] = best
		}
	}
	return found
}

// Folders are the folders the sources are in, each once, sorted.
func Folders(sources []string) []string {
	var dirs []string
	for _, href := range sources {
		dirs = append(dirs, path.Dir(href))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// Listing identifies the covers, so that a repository rebuilds its library
// when one is added, replaced or removed.
func Listing(covers map[string]Cover) string {
	var b strings.Builder
	dirs := make([]string, 0, len(covers))
	for dir := range covers {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	for _, dir := range dirs {
		fmt.Fprintf(&b, "cover %s/%s %d\n", dir, covers[dir].Name, covers[dir].ModTime.UnixNano())
	}
	return b.String()
}

// findCover gives an album the cover beside its first track, or else the
// picture the first of its tracks that carries one carries.
func (l *Library) findCover(a *Album) {
	dir := path.Dir(a.Tracks[0].Href)
	if c, ok := l.covers[dir]; ok {
		a.cover = joinDisk(l.Path, path.Join(dir, c.Name))
		a.coverTag = tagOf(a.cover, c.ModTime)
		return
	}
	for _, t := range a.Tracks {
		if t.Picture {
			a.coverTrack = t
			a.coverTag = tagOf(t.ID, t.ModTime)
			return
		}
	}
}

// CoverOf is where the image of an item is: an image file on disk, or the
// track whose picture it is. An artist's, a library's and a genre's is the
// cover of the first of its albums that has one, and a track's the cover of
// its album. ok is false for an item without an image.
func CoverOf(item Item) (file string, track *Track, ok bool) {
	a := albumOf(item)
	if a == nil {
		return "", nil, false
	}
	return a.cover, a.coverTrack, true
}

// hashes are the blurhashes of the covers by their tags, which change when a
// cover does, so that a cover is decoded once rather than at every build of a
// library and of the part of it an account sees. A cover that cannot be
// decoded has "".
var hashes = struct {
	sync.Mutex
	byTag map[string]string
}{byTag: map[string]string{}}

// hashCovers gives the albums the blurhashes of their covers, decoding those
// not decoded before side by side.
func hashCovers(albums []*Album) {
	var todo []*Album
	hashes.Lock()
	for _, a := range albums {
		if a.coverTag == "" {
			continue
		}
		if h, ok := hashes.byTag[a.coverTag]; ok {
			a.blurHash = h
		} else {
			todo = append(todo, a)
		}
	}
	hashes.Unlock()
	if len(todo) == 0 {
		return
	}
	work := make(chan *Album)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(todo)) {
		wg.Go(func() {
			for a := range work {
				a.blurHash = coverHash(a)
			}
		})
	}
	for _, a := range todo {
		work <- a
	}
	close(work)
	wg.Wait()
	hashes.Lock()
	for _, a := range todo {
		hashes.byTag[a.coverTag] = a.blurHash
	}
	hashes.Unlock()
}

// coverHash decodes the cover of an album to its blurhash, "" when it cannot.
func coverHash(a *Album) string {
	var r io.Reader
	if a.cover != "" {
		f, err := os.Open(a.cover)
		if err != nil {
			return ""
		}
		defer func() { _ = f.Close() }()
		r = f
	} else {
		picture, _, err := audio.Picture(a.coverTrack.OnDisk())
		if err != nil {
			return ""
		}
		r = bytes.NewReader(picture)
	}
	img, err := decodeCover(r)
	if err != nil {
		return ""
	}
	return blurHash(img)
}

// blurHashOf is the blurhash of the image of an item, "" for an item without
// one or whose image cannot be decoded.
func blurHashOf(item Item) string {
	if a := albumOf(item); a != nil {
		return a.blurHash
	}
	return ""
}

// imageTag is what identifies the image of an item, which clients cache it
// by; "" for an item without one.
func imageTag(item Item) string {
	if a := albumOf(item); a != nil {
		return a.coverTag
	}
	return ""
}

// albumOf is the album whose cover is the image of item, nil when none.
func albumOf(item Item) *Album {
	switch v := item.(type) {
	case *Album:
		if v.coverTag != "" {
			return v
		}
	case *Track:
		return albumOf(v.Album)
	case *Library:
		return firstCovered(v.Albums)
	case *Genre:
		return firstCovered(v.Albums)
	case *Artist:
		if a := firstCovered(v.Albums); a != nil {
			return a
		}
		for _, t := range v.Tracks {
			if t.Album.coverTag != "" {
				return t.Album
			}
		}
	}
	return nil
}

// firstCovered is the first of albums that has a cover, nil when none has.
func firstCovered(albums []*Album) *Album {
	for _, a := range albums {
		if a.coverTag != "" {
			return a
		}
	}
	return nil
}

func tagOf(what string, modTime time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d", what, modTime.UnixNano()))
	return hex.EncodeToString(sum[:8])
}

// joinDisk is where a path of the repository's folder, with forward
// slashes, is on disk.
func joinDisk(onDisk, href string) string {
	return filepath.Join(onDisk, filepath.FromSlash(href))
}
