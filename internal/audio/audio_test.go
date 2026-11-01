package audio

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/audio/audiotest"
)

var tags = audiotest.Tags{Title: "Song", Artist: "Someone", AlbumArtist: "The Band", Album: "First",
	Genre: "Rock", Track: 3, Disc: 2, Year: 2001, Picture: audiotest.PNG}

func write(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func near(got time.Duration, seconds float64) bool {
	return math.Abs(got.Seconds()-seconds) < 0.05
}

func TestReadsTagsAndLength(t *testing.T) {
	cases := []struct {
		name      string
		content   []byte
		container string
		codec     string
		seconds   float64
		// Opus carries its picture in a comment, which the test file has
		// none of
		picture bool
	}{
		{"song.mp3", audiotest.MP3(tags, 100, true), "mp3", "mp3", audiotest.MP3Seconds(100), true},
		{"song.flac", audiotest.FLAC(tags, 7), "flac", "flac", 7, true},
		{"song.m4a", audiotest.M4A(tags, 5), "m4a", "aac", 5, true},
		{"song.opus", audiotest.Opus(tags, 4), "opus", "opus", 4, false},
	}
	for _, c := range cases {
		track, err := Read(write(t, c.name, c.content))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if track.Container != c.container || track.Codec != c.codec {
			t.Errorf("%s: container %q codec %q", c.name, track.Container, track.Codec)
		}
		if !near(track.Duration, c.seconds) {
			t.Errorf("%s: duration %v, want %.2fs", c.name, track.Duration, c.seconds)
		}
		if track.Title != "Song" || track.Artist != "Someone" || track.AlbumArtist != "The Band" ||
			track.Album != "First" || track.Genre != "Rock" || track.Number != 3 || track.Year != 2001 {
			t.Errorf("%s: tags %+v", c.name, track)
		}
		if track.Disc != 2 {
			t.Errorf("%s: disc %d", c.name, track.Disc)
		}
		if track.Picture != c.picture {
			t.Errorf("%s: picture %v", c.name, track.Picture)
		}
		if track.SampleRate == 0 || track.Channels != 2 || track.Bitrate == 0 {
			t.Errorf("%s: format %+v", c.name, track.Format)
		}
	}
}

func TestReadsThePicture(t *testing.T) {
	for name, content := range map[string][]byte{
		"a.mp3":  audiotest.MP3(tags, 3, true),
		"a.flac": audiotest.FLAC(tags, 1),
		"a.m4a":  audiotest.M4A(tags, 1),
	} {
		picture, media, err := Picture(write(t, name, content))
		if err != nil || !bytes.Equal(picture, audiotest.PNG) || media != "image/png" {
			t.Errorf("%s: %d bytes of %q, %v", name, len(picture), media, err)
		}
	}
	if _, _, err := Picture(write(t, "plain.mp3", audiotest.MP3(audiotest.Tags{Title: "x"}, 3, true))); err == nil {
		t.Error("a picture is read from a file that carries none")
	}
}

func TestReadsAConstantBitrateMP3BySize(t *testing.T) {
	// 417 bytes a frame at 128 kbit/s
	track, err := Read(write(t, "cbr.mp3", audiotest.MP3(audiotest.Tags{}, 200, false)))
	if err != nil {
		t.Fatal(err)
	}
	if want := 200 * 417 * 8 / 128000.0; !near(track.Duration, want) || track.Bitrate != 128000 {
		t.Errorf("duration %v bitrate %d, want %.2fs", track.Duration, track.Bitrate, want)
	}
	if track.Title != "" || track.Picture {
		t.Errorf("an untagged file has tags: %+v", track)
	}
}

func TestReadsAWAVE(t *testing.T) {
	track, err := Read(write(t, "a.wav", audiotest.WAV(2)))
	if err != nil {
		t.Fatal(err)
	}
	if track.Container != "wav" || track.Codec != "pcm" || !near(track.Duration, 2) ||
		track.SampleRate != 8000 || track.Channels != 1 || track.Bitrate != 64000 {
		t.Errorf("%+v", track.Format)
	}
}

func TestSkipsJunkInFrontOfTheFirstFrame(t *testing.T) {
	content := append(append([]byte{0xff, 0x00, 0xff, 0xfb, 0x00}, make([]byte, 300)...),
		audiotest.MP3(audiotest.Tags{}, 50, true)...)
	track, err := Read(write(t, "junk.mp3", content))
	if err != nil {
		t.Fatal(err)
	}
	if !near(track.Duration, audiotest.MP3Seconds(50)) {
		t.Errorf("duration %v", track.Duration)
	}
}

func TestRefusesWhatIsNoAudio(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty.mp3":   nil,
		"text.mp3":    bytes.Repeat([]byte("this is no music "), 100),
		"cut.flac":    audiotest.FLAC(tags, 1)[:20],
		"nomoov.m4a":  audiotest.M4A(tags, 1)[:30],
		"header.opus": audiotest.Opus(tags, 1)[:20],
		"onlytag.mp3": audiotest.MP3(tags, 0, false),
	} {
		if _, err := Read(write(t, name, content)); !errors.Is(err, ErrNotAudio) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
