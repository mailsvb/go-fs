//go:build !light && !no_subsonic

package repository

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/jellyfin"
)

// TestSubsonicPublishesItsOwnLibrary checks a subsonic repository publishes
// its library to the OpenSubsonic clients only, and takes it away when it
// stops.
func TestSubsonicPublishesItsOwnLibrary(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "music")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(config.Repository{Name: "sub", Type: config.RepositoryTypeSubsonic, Folder: "/music"},
		base, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jellyfin.Retire("subsonic", "sub") })
	// written here rather than by putFile, which a binary without Jellyfin
	// has not
	album := filepath.Join(folder, "Band", "First")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	track := audiotest.MP3(audiotest.Tags{Title: "One", Artist: "Band", Album: "First", Track: 1}, 20, true)
	if err := os.WriteFile(filepath.Join(album, "01.mp3"), track, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.update(time.Now()); err != nil {
		t.Fatal(err)
	}
	libs := jellyfin.Libraries("subsonic")
	if len(libs) != 1 || libs[0].Name != "sub" || len(libs[0].Tracks) != 1 {
		t.Fatalf("published to subsonic: %v", libs)
	}
	for _, l := range jellyfin.Libraries("jellyfin") {
		if l.Name == "sub" {
			t.Error("a subsonic library is published to Jellyfin clients")
		}
	}
	if r.status.Albums != 1 {
		t.Errorf("status %+v", r.status)
	}
	r.kind.(retirer).retire()
	if len(jellyfin.Libraries("subsonic")) != 0 {
		t.Error("a retired subsonic library is still published")
	}
}
