//go:build !light && (!no_jellyfin || !no_subsonic)

package repository

import (
	"encoding/json"
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

// newMusicKind makes the kind of a jellyfin or a subsonic repository, which
// kind_jellyfin.go and kind_subsonic.go register, each behind its own tag.
func newMusicKind(repo config.Repository, folder string, _ *slog.Logger) (kind, Status, error) {
	return &musicKind{repo: repo, folder: folder}, Status{}, nil
}

// musicKind is a music library, of a jellyfin or a subsonic repository. Its
// packages are audio files, which it neither signs nor prunes, and its
// metadata is the index the clients browse, which it keeps in memory rather
// than in the folder: it is built anew from the files at every start, as the
// metadata of the other kinds is checked anew. It is published under the type
// of the repository, so that the clients of each API see their own
// libraries.
type musicKind struct {
	repo   config.Repository
	folder string
	// published says the index was built since the kind was made, and
	// albums is how many albums it has.
	published bool
	albums    int
}

// musicRelease is an audio file that was read, and musicEntry one ready to
// be listed. Each file is a package of its own, of which there are no
// versions: the group is the file.
type musicRelease struct {
	path  string
	track audio.Track
}

type musicEntry struct{ jellyfin.Source }

func (r *musicRelease) group() string       { return r.path }
func (r *musicRelease) version() string     { return "" }
func (r *musicRelease) compare(release) int { return 0 }

func (e musicEntry) group() string       { return e.Href }
func (e musicEntry) version() string     { return "" }
func (e musicEntry) compare(release) int { return 0 }
func (e musicEntry) listing() string {
	return fmt.Sprintf("%s %d %d", e.Href, e.Size, e.ModTime.UnixNano())
}

// audioTypes are the extensions of the audio files the library lists.
var audioTypes = map[string]bool{".mp3": true, ".flac": true, ".m4a": true, ".aac": true, ".ogg": true,
	".oga": true, ".opus": true, ".wav": true}

func (k *musicKind) keyID() string { return "" }
func (k *musicKind) isPackage(name string) bool {
	return audioTypes[strings.ToLower(filepath.Ext(name))]
}
func (k *musicKind) skipDir(string) bool { return false }

func (k *musicKind) open(path string) (release, error) {
	track, err := audio.Read(path)
	if err != nil {
		return nil, err
	}
	return &musicRelease{path: path, track: track}, nil
}

func (k *musicKind) admit(r release, href string) (entry, error) {
	info, err := os.Stat(filepath.Join(k.folder, filepath.FromSlash(href)))
	if err != nil {
		return nil, err
	}
	return musicEntry{jellyfin.Source{Href: href, Size: info.Size(), ModTime: info.ModTime(),
		Track: r.(*musicRelease).track}}, nil
}

func (k *musicKind) decode(raw []byte) (entry, error) {
	var e musicEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	return e, nil
}

// inputs are the covers beside the tracks, so that one that is added or
// replaced is shown.
func (k *musicKind) inputs(entries []entry) string {
	return jellyfin.Listing(jellyfin.Covers(k.folder, k.folders(entries)))
}

func (k *musicKind) write(entries []entry, now time.Time) error {
	sources := make([]jellyfin.Source, len(entries))
	for i, e := range entries {
		sources[i] = e.(musicEntry).Source
	}
	covers := jellyfin.Covers(k.folder, k.folders(entries))
	library := jellyfin.Build(k.repo.Name, k.repo.DisplayName(), k.repo.Folder, k.folder, sources, covers, now)
	jellyfin.Publish(k.repo.Type, library)
	k.published = true
	k.albums = len(library.Albums)
	return nil
}

func (k *musicKind) written() bool { return k.published }

// retire takes the library away from the clients when the repository stops.
func (k *musicKind) retire() { jellyfin.Retire(k.repo.Type, k.repo.Name) }

func (k *musicKind) report(status *Status) { status.Albums = k.albums }

func (k *musicKind) folders(entries []entry) []string {
	hrefs := make([]string, len(entries))
	for i, e := range entries {
		hrefs[i] = e.(musicEntry).Href
	}
	return jellyfin.Folders(hrefs)
}
