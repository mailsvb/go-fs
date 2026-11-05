// Package mp3edit edits the tags of an MP3 file and changes its volume without
// re-encoding it, for the editor of the file listing. The work is done by the
// modules of go-tag (github.com/mailsvb/go-tag/mp3tags and mp3gain); this
// package gives them a file wherever it is, on the local disk or on a stored
// server, and keeps what an analysis found until the file is saved.
//
// The part is left out by the tag no_mp3tags, and by light: Compiled is then
// false and every function fails with ErrUnavailable. The types are the same
// in both, so that the HTTP server compiles either way.
package mp3edit

import (
	"errors"
	"io"
	"strings"
	"time"
)

var (
	// ErrUnavailable is what every function answers in a binary built
	// without the part.
	ErrUnavailable = errors.New("MP3 tag editing is not part of this binary")
	// ErrOutOfRange is a volume change larger than the gain values of the
	// file's frames allow.
	ErrOutOfRange = errors.New("the volume change is larger than this file allows")
	// ErrTooLarge is a file too large to be held in memory for its volume.
	ErrTooLarge = errors.New("the file is too large to change its volume")
	// ErrCover is a new cover that is not a JPEG or PNG image, or too large.
	ErrCover = errors.New("the cover must be a JPEG or PNG image of at most 10 MiB")
	// ErrNotMP3 is a file without MPEG audio frames go-tag can work on.
	ErrNotMP3 = errors.New("the file holds no MP3 audio this editor can change")
)

// StepDB is the volume change of one step in dB, 20·log10(2^(1/4)): the
// global gain of an MP3 frame changes the level in steps of that size.
const StepDB = 1.5051499783199060

// MaxCover is the largest cover image a save takes.
const MaxCover = 10 << 20

// MaxAudio is the largest file whose volume is analyzed or changed: the whole
// audio is held in memory for that.
const MaxAudio = 512 << 20

// IsMP3 reports whether a name is one the editor offers to edit.
func IsMP3(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".mp3")
}

// Source is an MP3 file wherever it is.
type Source struct {
	// Key names the file for the cache of analyses, with where it is: two
	// files with the same key are the same file.
	Key     string
	Size    int64
	ModTime time.Time
	// Open reads the file. The closer is closed once the reader is done
	// with.
	Open func() (io.ReaderAt, io.Closer, error)
	// Path is the file on the local disk, which a save rewrites beside
	// itself and renames into place; "" for a file elsewhere, which Put
	// writes.
	Path string
	// Put replaces the file, whole, with body. It must leave the file as it
	// was when body fails.
	Put func(body io.Reader) error
}

// Fields are the tag fields the editor changes.
type Fields struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	AlbumArtist string `json:"albumArtist"`
	Album       string `json:"album"`
	Genre       string `json:"genre"`
	Year        string `json:"year"`
	Track       string `json:"track"`
}

// Cover describes the cover image of a file.
type Cover struct {
	MIME string `json:"mime"`
	Size int    `json:"size"`
}

// Gain is the volume information a file's tag holds.
type Gain struct {
	// Steps is the change applied to the audio so far, in steps of StepDB;
	// DB is the same in dB.
	Steps int     `json:"steps"`
	DB    float64 `json:"db"`
	// The ReplayGain values, "" where the tag has none.
	TrackGain string `json:"trackGain"`
	TrackPeak string `json:"trackPeak"`
	AlbumGain string `json:"albumGain"`
	AlbumPeak string `json:"albumPeak"`
}

// Info is what the editor shows of a file.
type Info struct {
	Fields Fields `json:"fields"`
	// Cover is nil for a file without one.
	Cover *Cover `json:"cover"`
	Gain  Gain   `json:"gain"`
}

// Change is a planned volume change.
type Change struct {
	// Steps is the change in steps of StepDB, DB the same in dB.
	Steps int     `json:"steps"`
	DB    float64 `json:"db"`
	// Limited says the change is smaller than the target asks for, to keep
	// the audio from clipping or because the file allows no more.
	Limited bool `json:"limited"`
	// Loudness is the measured loudness in LUFS the change is based on, nil
	// for silence; Peak the sample peak (1.0 is full scale).
	Loudness *float64 `json:"loudness"`
	Peak     float64  `json:"peak"`
}

// Analysis is what measuring a file found.
type Analysis struct {
	// MaxUp and MaxDown are how many steps the volume can be raised and
	// lowered.
	MaxUp   int `json:"maxUp"`
	MaxDown int `json:"maxDown"`
	// Track is the change that brings the file to the target loudness.
	Track Change `json:"track"`
}

// Album is the one change for all files of an album.
type Album struct {
	Change
	// MaxUp and MaxDown are how far the volume of every file of the album
	// can be raised and lowered.
	MaxUp   int `json:"maxUp"`
	MaxDown int `json:"maxDown"`
}

// AlbumLevel is what an album was measured at, which a save of one of its
// files writes the album's ReplayGain values from.
type AlbumLevel struct {
	Loudness *float64 `json:"loudness"`
	Peak     float64  `json:"peak"`
}

// NewCover is a cover a save puts into the file.
type NewCover struct {
	Data []byte
}

// Edit is what a save changes.
type Edit struct {
	// Fields are the new fields, nil to keep them as they are.
	Fields *Fields
	// RemoveCover takes the cover out; Cover, when not nil, replaces it.
	RemoveCover bool
	Cover       *NewCover
	// Steps changes the volume by that many steps of StepDB.
	Steps int
	// ReplayGain writes the file's ReplayGain values from its analysis even
	// when the volume stays as it is; a change of the volume always does.
	ReplayGain bool
	// Album is the album the file was measured with, nil to leave out the
	// album's ReplayGain values when the volume changes.
	Album *AlbumLevel
}
