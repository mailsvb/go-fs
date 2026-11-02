//go:build !light && !no_jellyfin

package repository

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
)

func newJellyfinRunner(t *testing.T) (*runner, string) {
	t.Helper()
	base := t.TempDir()
	folder := filepath.Join(base, "music")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(config.Repository{Name: "music", Type: config.RepositoryTypeJellyfin, Folder: "/music",
		Title: "Our music"}, base, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jellyfin.Retire("music") })
	return r, folder
}

func putFile(t *testing.T, folder, href string, data []byte) {
	t.Helper()
	path := filepath.Join(folder, filepath.FromSlash(href))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// published is the library of the repository music that clients browse.
func published(t *testing.T) *jellyfin.Library {
	t.Helper()
	for _, l := range jellyfin.Libraries() {
		if l.Name == "music" {
			return l
		}
	}
	t.Fatal("the library is not published")
	return nil
}

func titles(l *jellyfin.Library) []string {
	var out []string
	for _, tr := range l.Tracks {
		out = append(out, tr.Name)
	}
	return out
}

func TestJellyfinPublishesTheLibrary(t *testing.T) {
	r, folder := newJellyfinRunner(t)
	putFile(t, folder, "Band/First/01.mp3", audiotest.MP3(audiotest.Tags{Title: "One", Artist: "Band",
		Album: "First", Track: 1}, 20, true))
	putFile(t, folder, "Band/First/02.flac", audiotest.FLAC(audiotest.Tags{Title: "Two", Artist: "Band",
		Album: "First", Track: 2}, 3))
	putFile(t, folder, "Band/First/notes.txt", []byte("not music"))
	putFile(t, folder, "broken.mp3", []byte("not music either"))
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	l := published(t)
	if l.Title != "Our music" || l.Folder != "/music" || !slices.Equal(titles(l), []string{"One", "Two"}) {
		t.Errorf("library %q at %q with %q", l.Title, l.Folder, titles(l))
	}
	if r.status.Packages != 2 || r.status.Albums != 1 || r.status.KeyID != "" {
		t.Errorf("status %+v", r.status)
	}
	if _, ok := r.skipped["broken.mp3"]; !ok || len(r.skipped) != 1 {
		t.Errorf("skipped %v", r.skipped)
	}

	// nothing changed, nothing is built
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	if published(t) != l {
		t.Error("the library was built again with nothing changed")
	}

	// a cover is a change
	putFile(t, folder, "Band/First/cover.png", audiotest.PNG)
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	next := published(t)
	if next == l {
		t.Fatal("a new cover did not build the library again")
	}
	if file, _, ok := jellyfin.CoverOf(next.Albums[0]); !ok || filepath.Base(file) != "cover.png" {
		t.Errorf("cover %q", file)
	}

	// so is a track that is taken away
	if err := os.Remove(filepath.Join(folder, "Band", "First", "01.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := titles(published(t)); !slices.Equal(got, []string{"Two"}) {
		t.Errorf("after the removal %q", got)
	}
}

func TestJellyfinRemovesNothing(t *testing.T) {
	r, folder := newJellyfinRunner(t)
	// two versions of one song are two songs
	putFile(t, folder, "a.mp3", audiotest.MP3(audiotest.Tags{Title: "Song"}, 5, true))
	putFile(t, folder, "b.mp3", audiotest.MP3(audiotest.Tags{Title: "Song"}, 5, true))
	r.repo.Keep = 1
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.mp3", "b.mp3"} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestJellyfinRetiresWithTheRepository(t *testing.T) {
	base := t.TempDir()
	putFile(t, filepath.Join(base, "music"), "song.mp3", audiotest.MP3(audiotest.Tags{Title: "Song"}, 5, true))
	cfg := config.Config{
		General:      config.General{BaseFolderFiles: base},
		Repositories: []config.Repository{{Name: "music", Type: config.RepositoryTypeJellyfin, Folder: "/music"}},
	}
	m, err := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for Statuses()["music"].Packages != 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if published(t).Title != "music" {
		t.Error("a library without a title is not called after its repository")
	}
	cfg.Repositories = nil
	if err := m.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	for _, l := range jellyfin.Libraries() {
		if l.Name == "music" {
			t.Error("the library of a repository taken out is still published")
		}
	}
}

// A restart takes the tracks read from the cache and publishes the library
// without reading any again.
func TestJellyfinRestartReadsNoTrackAgain(t *testing.T) {
	r, folder := newJellyfinRunner(t)
	withCache(t, r)
	putFile(t, folder, "Band/First/01.mp3", audiotest.MP3(audiotest.Tags{Title: "One", Artist: "Band",
		Album: "First", Track: 1}, 20, true))
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	disguise(t, filepath.Join(folder, "Band", "First", "01.mp3"))
	jellyfin.Retire("music")

	again := restart(t, r)
	if err := again.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	if l := published(t); !slices.Equal(titles(l), []string{"One"}) || len(again.skipped) != 0 {
		t.Errorf("after a restart the library has %q and skipped %v", titles(l), again.skipped)
	}
	if again.status.Packages != 1 || again.status.Albums != 1 {
		t.Errorf("after a restart the status is %+v", again.status)
	}
}
