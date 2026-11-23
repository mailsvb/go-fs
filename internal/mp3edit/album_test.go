//go:build !light && !no_mp3tags

package mp3edit

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-tag/mp3gain"
)

// albumOf is an album of files of different levels, each a file elsewhere of
// its own name, so that none shares the analysis of another.
func albumOf(t *testing.T, name string, gains ...byte) []Source {
	t.Helper()
	srcs := make([]Source, len(gains))
	for i, gain := range gains {
		m := &memory{data: withGain(audiotest.MP3(audiotest.Tags{Title: "Song"}, 40, true), gain),
			modTime: time.Unix(1000, 0)}
		srcs[i] = m.source()
		srcs[i].Key = fmt.Sprintf("remote:%s/%d.mp3", name, i)
	}
	return srcs
}

// The files of an album are measured at once, and the album is planned as if
// they were measured one after the other.
func TestAnAlbumIsPlannedAsItsFilesOneByOne(t *testing.T) {
	gains := []byte{120, 135, 150, 160, 170, 180, 140, 155}
	var results []mp3gain.Result
	up, down := 1<<30, 1<<30
	for _, src := range albumOf(t, "one-by-one", gains...) {
		res, err := analysis(src)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, res)
		up, down = min(up, res.MaxUp()), min(down, res.MaxDown())
	}
	want := Album{Change: changeOf(mp3gain.PlanAlbum(results)), MaxUp: up, MaxDown: down}

	got, err := PlanAlbum(albumOf(t, "at-once", gains...))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("PlanAlbum = %+v, want %+v", got, want)
	}
}

// Of the files of an album that cannot be measured, the first names the
// failure.
func TestAnAlbumFailsWithItsFirstFileThatCannotBeMeasured(t *testing.T) {
	srcs := albumOf(t, "broken", 120, 135, 150, 160, 170, 180)
	empty := audiotest.MP3(audiotest.Tags{Title: "Empty"}, 0, false)
	for _, i := range []int{2, 4} {
		m := &memory{data: empty, modTime: time.Unix(1000, 0)}
		key := srcs[i].Key
		srcs[i] = m.source()
		srcs[i].Key = key
	}
	_, err := PlanAlbum(srcs)
	if !errors.Is(err, ErrNotMP3) {
		t.Fatalf("PlanAlbum = %v, want %v", err, ErrNotMP3)
	}
	if want := srcs[2].Key + ": "; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Errorf("PlanAlbum = %q, want it to start with %q", err, want)
	}
}
