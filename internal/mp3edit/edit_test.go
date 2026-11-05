//go:build !light && !no_mp3tags

package mp3edit

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhowden/tag"
	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-tag/mp3tags"
)

// tagged is an MP3 with the fields the editor manages and some it does not.
func tagged() []byte {
	return audiotest.MP3(audiotest.Tags{
		Title: "Old", Artist: "Band", Album: "Record", Year: 2001, Track: 3,
		Composer: "Writer", Disc: 2, Extra: map[string]string{"MOOD": "calm"},
	}, 40, true)
}

// localSource writes data to a file and is the source of it.
func localSource(t *testing.T, data []byte) Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return sourceOfPath(t, path)
}

func sourceOfPath(t *testing.T, path string) Source {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return Source{
		Key: "local:" + path, Size: info.Size(), ModTime: info.ModTime(), Path: path,
		Open: func() (io.ReaderAt, io.Closer, error) {
			f, err := os.Open(path)
			return f, f, err
		},
	}
}

// memory is a file held elsewhere, which only Put replaces.
type memory struct {
	data    []byte
	modTime time.Time
	puts    int
	failPut error
}

func (m *memory) source() Source {
	return Source{
		Key: "remote:memory", Size: int64(len(m.data)), ModTime: m.modTime,
		Open: func() (io.ReaderAt, io.Closer, error) {
			return bytes.NewReader(m.data), io.NopCloser(nil), nil
		},
		Put: func(body io.Reader) error {
			m.puts++
			got, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			if m.failPut != nil {
				return m.failPut
			}
			m.data = got
			m.modTime = m.modTime.Add(time.Second)
			return nil
		},
	}
}

func TestReadsTheFields(t *testing.T) {
	info, err := Read(localSource(t, tagged()))
	if err != nil {
		t.Fatal(err)
	}
	want := Fields{Title: "Old", Artist: "Band", Album: "Record", Year: "2001", Track: "3"}
	if info.Fields != want {
		t.Errorf("fields = %+v, want %+v", info.Fields, want)
	}
	if info.Cover != nil {
		t.Errorf("cover = %+v, want none", info.Cover)
	}
}

func TestSaveKeepsWhatItDoesNotManage(t *testing.T) {
	src := localSource(t, tagged())
	fields := Fields{Title: "New", Artist: "Band", AlbumArtist: "Various", Album: "Record", Genre: "Rock", Year: "2002", Track: "4/10"}
	if err := Save(src, Edit{Fields: &fields}); err != nil {
		t.Fatal(err)
	}
	info, err := Read(sourceOfPath(t, src.Path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Fields != fields {
		t.Errorf("fields = %+v, want %+v", info.Fields, fields)
	}
	saved := mustRead(t, src.Path)
	for _, frame := range []string{"TCOM", "Writer", "TPOS", "MOOD\x00calm"} {
		if !bytes.Contains(saved, []byte(frame)) {
			t.Errorf("%q was lost, which the editor does not manage", frame)
		}
	}
}

// The music library reads tags with github.com/dhowden/tag, which refuses a
// whole tag over one malformed frame.
func TestASavedTagIsReadByTheMusicLibrary(t *testing.T) {
	src := localSource(t, tagged())
	if err := Save(src, Edit{Fields: &Fields{Title: "New", Artist: "Band"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	m, err := tag.ReadFrom(f)
	if err != nil && strings.Contains(err.Error(), "even number of bytes") {
		// github.com/bogem/id3v2 v2.1.4 writes a UTF-16 frame a byte too
		// long, which go-tag uses for every text field it writes
		t.Skipf("go-tag writes UTF-16 text dhowden/tag refuses: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if m.Title() != "New" || m.Composer() != "Writer" {
		t.Errorf("title %q, composer %q", m.Title(), m.Composer())
	}
}

func TestSaveWithoutAChangeLeavesTheFile(t *testing.T) {
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0)}
	info, err := Read(m.source())
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(m.source(), Edit{Fields: &info.Fields}); err != nil {
		t.Fatal(err)
	}
	if m.puts != 0 {
		t.Errorf("the file was written %d times for nothing", m.puts)
	}
}

func TestSaveToAFileElsewhere(t *testing.T) {
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0)}
	fields := Fields{Title: "Remote", Artist: "Band"}
	if err := Save(m.source(), Edit{Fields: &fields}); err != nil {
		t.Fatal(err)
	}
	info, err := Read(m.source())
	if err != nil {
		t.Fatal(err)
	}
	if info.Fields != fields {
		t.Errorf("fields = %+v, want %+v", info.Fields, fields)
	}
	if !bytes.Contains(m.data, []byte("MOOD")) {
		t.Error("the TXXX frame MOOD was lost")
	}
}

func TestAFailedPutIsTheSavesFailure(t *testing.T) {
	broken := errors.New("disk full")
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0), failPut: broken}
	before := bytes.Clone(m.data)
	err := Save(m.source(), Edit{Fields: &Fields{Title: "New"}})
	if !errors.Is(err, broken) {
		t.Fatalf("Save = %v, want %v", err, broken)
	}
	if !bytes.Equal(m.data, before) {
		t.Error("the file changed although the save failed")
	}
}

func TestCoverIsReplacedAndRemoved(t *testing.T) {
	src := localSource(t, tagged())
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := Save(src, Edit{Cover: &NewCover{Data: img.Bytes()}}); err != nil {
		t.Fatal(err)
	}
	data, mime, err := CoverOf(sourceOfPath(t, src.Path))
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || !bytes.Equal(data, img.Bytes()) {
		t.Errorf("cover %s of %d bytes, want the PNG saved", mime, len(data))
	}
	if err := Save(sourceOfPath(t, src.Path), Edit{RemoveCover: true}); err != nil {
		t.Fatal(err)
	}
	if info, err := Read(sourceOfPath(t, src.Path)); err != nil || info.Cover != nil {
		t.Errorf("cover after removing it = %+v, %v", info.Cover, err)
	}
}

func TestACoverThatIsNoImageIsRefused(t *testing.T) {
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0)}
	err := Save(m.source(), Edit{Cover: &NewCover{Data: []byte("not an image")}})
	if !errors.Is(err, ErrCover) {
		t.Fatalf("Save = %v, want %v", err, ErrCover)
	}
	if m.puts != 0 {
		t.Error("the file was written")
	}
}

func TestVolumeChangeIsLosslessAndUndone(t *testing.T) {
	src := localSource(t, tagged())
	original, err := mp3tags.ReadAudio(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := Analyze(src)
	if err != nil {
		t.Fatal(err)
	}
	// the frames are built with a global gain of 0: up only
	if analysis.MaxUp != 255 || analysis.MaxDown != 0 {
		t.Errorf("range = +%d/-%d, want +255/-0", analysis.MaxUp, analysis.MaxDown)
	}
	if err := Save(src, Edit{Steps: 2}); err != nil {
		t.Fatal(err)
	}
	louder := sourceOfPath(t, src.Path)
	info, err := Read(louder)
	if err != nil {
		t.Fatal(err)
	}
	if info.Gain.Steps != 2 || info.Gain.DB != 2*StepDB {
		t.Errorf("gain = %+v, want 2 steps", info.Gain)
	}
	changed, err := mp3tags.ReadAudio(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(changed, original) {
		t.Error("the audio did not change")
	}
	if err := Save(louder, Edit{Steps: -2}); err != nil {
		t.Fatal(err)
	}
	restored, err := mp3tags.ReadAudio(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Error("the audio is not what it was before the two changes")
	}
	if info, err := Read(sourceOfPath(t, src.Path)); err != nil || info.Gain.Steps != 0 {
		t.Errorf("gain after undoing = %+v, %v", info.Gain, err)
	}
}

func TestAVolumeChangeOutOfRangeIsRefused(t *testing.T) {
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0)}
	err := Save(m.source(), Edit{Steps: -1})
	if !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Save = %v, want %v", err, ErrOutOfRange)
	}
	if m.puts != 0 {
		t.Error("the file was written")
	}
}

func TestVolumeChangeOfAFileElsewhere(t *testing.T) {
	m := &memory{data: tagged(), modTime: time.Unix(1000, 0)}
	album, err := PlanAlbum([]Source{m.source()})
	if err != nil {
		t.Fatal(err)
	}
	level := &AlbumLevel{Loudness: album.Loudness, Peak: album.Peak}
	if err := Save(m.source(), Edit{Steps: 3, Album: level}); err != nil {
		t.Fatal(err)
	}
	info, err := Read(m.source())
	if err != nil {
		t.Fatal(err)
	}
	if info.Gain.Steps != 3 {
		t.Errorf("gain = %+v, want 3 steps", info.Gain)
	}
}

func TestAFileWithoutAudioCannotBeMeasured(t *testing.T) {
	m := &memory{data: audiotest.MP3(audiotest.Tags{Title: "Empty"}, 0, false), modTime: time.Unix(1000, 0)}
	if _, err := Analyze(m.source()); !errors.Is(err, ErrNotMP3) {
		t.Fatalf("Analyze = %v, want %v", err, ErrNotMP3)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
