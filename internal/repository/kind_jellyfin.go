//go:build !light && !no_jellyfin

package repository

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/audio"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
)

// The Jellyfin music library is left out by the tag no_jellyfin, and by
// light.
func init() {
	kinds[config.RepositoryTypeJellyfin] = func(repo config.Repository, folder string, _ *slog.Logger) (kind, Status, error) {
		return &jellyfinKind{repo: repo, folder: folder}, Status{}, nil
	}
}

// jellyfinKind is a music library. Its packages are audio files, which it
// neither signs nor prunes, and its metadata is the index Jellyfin clients
// browse, which it keeps in memory rather than in the folder: it is built
// anew from the files at every start, as the metadata of the other kinds is
// checked anew.
type jellyfinKind struct {
	repo   config.Repository
	folder string
	// published says the index was built since the kind was made, and
	// albums is how many albums it has.
	published bool
	albums    int
}

// jellyfinRelease is an audio file that was read, and jellyfinEntry one ready
// to be listed. Each file is a package of its own, of which there are no
// versions: the group is the file.
type jellyfinRelease struct {
	path  string
	track audio.Track
}

type jellyfinEntry struct{ jellyfin.Source }

func (r *jellyfinRelease) group() string       { return r.path }
func (r *jellyfinRelease) version() string     { return "" }
func (r *jellyfinRelease) compare(release) int { return 0 }

func (e jellyfinEntry) group() string       { return e.Href }
func (e jellyfinEntry) version() string     { return "" }
func (e jellyfinEntry) compare(release) int { return 0 }
func (e jellyfinEntry) listing() string {
	return fmt.Sprintf("%s %d %d", e.Href, e.Size, e.ModTime.UnixNano())
}

// audioTypes are the extensions of the audio files the library lists.
var audioTypes = map[string]bool{".mp3": true, ".flac": true, ".m4a": true, ".aac": true, ".ogg": true,
	".oga": true, ".opus": true, ".wav": true}

func (k *jellyfinKind) keyID() string { return "" }
func (k *jellyfinKind) isPackage(name string) bool {
	return audioTypes[strings.ToLower(filepath.Ext(name))]
}
func (k *jellyfinKind) skipDir(string) bool { return false }

func (k *jellyfinKind) open(path string) (release, error) {
	track, err := audio.Read(path)
	if err != nil {
		return nil, err
	}
	return &jellyfinRelease{path: path, track: track}, nil
}

func (k *jellyfinKind) admit(r release, href string) (entry, error) {
	info, err := os.Stat(filepath.Join(k.folder, filepath.FromSlash(href)))
	if err != nil {
		return nil, err
	}
	return jellyfinEntry{jellyfin.Source{Href: href, Size: info.Size(), ModTime: info.ModTime(),
		Track: r.(*jellyfinRelease).track}}, nil
}

// inputs are the covers beside the tracks, so that one that is added or
// replaced is shown.
func (k *jellyfinKind) inputs(entries []entry) string {
	return jellyfin.Listing(jellyfin.Covers(k.folder, k.folders(entries)))
}

func (k *jellyfinKind) write(entries []entry, now time.Time) error {
	sources := make([]jellyfin.Source, len(entries))
	for i, e := range entries {
		sources[i] = e.(jellyfinEntry).Source
	}
	covers := jellyfin.Covers(k.folder, k.folders(entries))
	library := jellyfin.Build(k.repo.Name, k.repo.DisplayName(), k.repo.Folder, k.folder, sources, covers, now)
	jellyfin.Publish(library)
	k.published = true
	k.albums = len(library.Albums)
	return nil
}

func (k *jellyfinKind) written() bool { return k.published }

// retire takes the library away from the clients when the repository stops.
func (k *jellyfinKind) retire() { jellyfin.Retire(k.repo.Name) }

func (k *jellyfinKind) report(status *Status) { status.Albums = k.albums }

func (k *jellyfinKind) folders(entries []entry) []string {
	hrefs := make([]string, len(entries))
	for i, e := range entries {
		hrefs[i] = e.(jellyfinEntry).Href
	}
	return jellyfin.Folders(hrefs)
}
