// Package audio reads what a music library shows of an audio file: the tags
// of MP3, FLAC, MP4 (M4A), Ogg Vorbis and Opus files, which
// github.com/dhowden/tag reads, and what the tags do not say, the length of
// the track and how it is encoded, which this package reads from the
// container itself.
//
// A file without tags is no error: the library names such a track after its
// file and folders. A file that is no audio this package recognizes is one.
package audio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dhowden/tag"
)

// Track is what is read of one audio file.
type Track struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genre       string
	Composer    string
	// Number and Disc are the track and disc numbers, 0 when not tagged.
	Number int
	Disc   int
	// Year is the year of the release, 0 when not tagged.
	Year int
	// Picture says that the file carries a picture, the cover of its album
	// as a rule, which Picture reads when it is asked for.
	Picture bool

	Format
}

// Format is how a file is encoded, as its container says.
type Format struct {
	// Container is the kind of file: mp3, flac, m4a, ogg, opus, wav or aac.
	Container string
	// Codec is what the audio in the container is: mp3, flac, aac, alac,
	// vorbis, opus or pcm. It is the container where the container says no
	// more.
	Codec string
	// Duration is the length of the track, 0 when it cannot be told.
	Duration time.Duration
	// Bitrate is bits per second, on average, 0 when it cannot be told.
	Bitrate int
	// SampleRate is samples per second, and Channels how many there are,
	// both 0 when they cannot be told.
	SampleRate int
	Channels   int
}

// ErrNotAudio is the error of a file that is not audio this package reads.
var ErrNotAudio = errors.New("is no audio file of a kind go-fs reads")

// Read reads the tags and the format of the file at path.
func Read(path string) (Track, error) {
	file, err := os.Open(path)
	if err != nil {
		return Track{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Track{}, err
	}
	format, err := Probe(file, info.Size())
	if err != nil {
		return Track{}, err
	}
	track := Track{Format: format}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Track{}, err
	}
	// a tag that cannot be read is a track without tags rather than no
	// track: the music plays whatever its tag says
	if m, err := tag.ReadFrom(file); err == nil {
		track.Title = clean(m.Title())
		track.Artist = clean(m.Artist())
		track.AlbumArtist = clean(m.AlbumArtist())
		track.Album = clean(m.Album())
		track.Genre = clean(m.Genre())
		track.Composer = clean(m.Composer())
		track.Number, _ = m.Track()
		track.Disc, _ = m.Disc()
		track.Year = m.Year()
		track.Picture = m.Picture() != nil && len(m.Picture().Data) > 0
	}
	return track, nil
}

// Picture reads the picture the file at path carries, and its media type.
func Picture(path string) ([]byte, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }()
	m, err := tag.ReadFrom(file)
	if err != nil {
		return nil, "", err
	}
	p := m.Picture()
	if p == nil || len(p.Data) == 0 {
		return nil, "", fmt.Errorf("%s carries no picture", path)
	}
	media := p.MIMEType
	if media == "" || !strings.Contains(media, "/") {
		media = pictureType(p.Ext)
	}
	return p.Data, media, nil
}

// pictureType is the media type of a picture by the extension it is tagged
// with, which is all an ID3v2.2 tag says of it.
func pictureType(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	}
	return "image/jpeg"
}

// clean takes away what a tag carries around its text: blanks, and the NUL
// that pads an ID3v1 field and ends an ID3v2 one.
func clean(s string) string {
	return strings.TrimSpace(strings.Trim(s, "\x00"))
}
