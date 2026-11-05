//go:build !light && !no_mp3tags

package mp3edit

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhowden/tag"
	"github.com/mailsvb/go-fs/internal/audio/audiotest"
	"github.com/mailsvb/go-tag/mp3tags"
)

// tagged is an MP3 with the fields the editor manages and some it does not,
// whose frames have a global gain of 150.
func tagged() []byte {
	return withGain(audiotest.MP3(audiotest.Tags{
		Title: "Old", Artist: "Band", Album: "Record", Year: 2001, Track: 3,
		Composer: "Writer", Disc: 2, Extra: map[string]string{"MOOD": "calm"},
	}, 40, true), 150)
}

// withGain sets the global gain of every audio frame of an MP3 that
// audiotest.MP3 built: MPEG-1 Layer III stereo frames of 417 bytes without a
// CRC, whose side information follows the 4 bytes of the header. A gain of 0,
// which audiotest leaves, is silence that no volume change touches. The Xing
// frame is no audio and is left as it is.
func withGain(mp3 []byte, gain byte) []byte {
	const frameSize = 417
	start := bytes.Index(mp3, []byte{0xff, 0xfb, 0x90, 0x00})
	for off := start; start >= 0 && off+frameSize <= len(mp3); off += frameSize {
		if string(mp3[off+36:off+40]) == "Xing" {
			continue
		}
		side := mp3[off+4 : off+36]
		// main_data_begin, private_bits and scfsi take 20 bits; each granule
		// of each channel takes 59, and its global gain is 8 bits 21 in
		for i := range 4 {
			at := 20 + i*59 + 21
			for b := range 8 {
				bit := at + b
				mask := byte(0x80 >> (bit % 8))
				if gain&(0x80>>b) != 0 {
					side[bit/8] |= mask
				} else {
					side[bit/8] &^= mask
				}
			}
		}
	}
	return mp3
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
	// from 150 up to 255, and down to 1: 0 is silence, which stays as it is
	if analysis.MaxUp != 105 || analysis.MaxDown != 149 {
		t.Errorf("range = +%d/-%d, want +105/-149", analysis.MaxUp, analysis.MaxDown)
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
	err := Save(m.source(), Edit{Steps: 106})
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

// The change is recorded as MP3Gain records it, so that either undoes what
// the other did.
func TestVolumeChangeIsRecordedForMP3Gain(t *testing.T) {
	src := localSource(t, tagged())
	if err := Save(src, Edit{Steps: 2}); err != nil {
		t.Fatal(err)
	}
	saved := mustRead(t, src.Path)
	if !bytes.Contains(saved, []byte("APETAGEX")) || !bytes.Contains(saved, []byte("MP3GAIN_UNDO\x00-002,-002,N")) {
		t.Error("the file has no APEv2 item MP3GAIN_UNDO of -002,-002,N")
	}
	if bytes.Contains(saved, []byte("GO-TAG_GAIN_STEPS")) {
		t.Error("the change was recorded the old way too")
	}
}

// What older versions of go-tag recorded is read, and replaced at the next
// change.
func TestAnOldRecordOfTheChangeIsRead(t *testing.T) {
	plain := tagged()
	original, err := mp3tags.ReadAudioFrom(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	// raised by 3 steps by an older go-tag, which noted it in a TXXX frame
	old := withGain(audiotest.MP3(audiotest.Tags{Title: "Old", Extra: map[string]string{"GO-TAG_GAIN_STEPS": "3"}}, 40, true), 153)
	src := localSource(t, old)
	info, err := Read(src)
	if err != nil {
		t.Fatal(err)
	}
	if info.Gain.Steps != 3 {
		t.Fatalf("gain = %+v, want the 3 steps of the old record", info.Gain)
	}
	if err := Save(src, Edit{Steps: -3}); err != nil {
		t.Fatal(err)
	}
	restored, err := mp3tags.ReadAudio(src.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Error("undoing the old change did not restore the audio")
	}
	saved := mustRead(t, src.Path)
	if bytes.Contains(saved, []byte("GO-TAG_GAIN_STEPS")) {
		t.Error("the old record is still there")
	}
	if info, err := Read(sourceOfPath(t, src.Path)); err != nil || info.Gain.Steps != 0 {
		t.Errorf("gain after undoing = %+v, %v", info.Gain, err)
	}
}

// mp3gain undoes a change go-fs made, and go-fs one that mp3gain made. It
// runs where mp3gain is installed.
func TestMP3GainAndTheEditorUndoEachOther(t *testing.T) {
	tool, err := exec.LookPath("mp3gain")
	if err != nil {
		t.Skip("mp3gain is not installed")
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
			t.Fatalf("mp3gain %v: %v\n%s", args, err, out)
		}
	}
	src := localSource(t, tagged())
	original, err := mp3tags.ReadAudio(src.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := Save(src, Edit{Steps: 2}); err != nil {
		t.Fatal(err)
	}
	run("-q", "-u", src.Path)
	if restored, err := mp3tags.ReadAudio(src.Path); err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("mp3gain -u did not restore the audio go-fs changed (%v)", err)
	}

	run("-q", "-c", "-g", "3", src.Path)
	info, err := Read(sourceOfPath(t, src.Path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Gain.Steps != 3 {
		t.Fatalf("gain after mp3gain -g 3 = %+v", info.Gain)
	}
	if err := Save(sourceOfPath(t, src.Path), Edit{Steps: -3}); err != nil {
		t.Fatal(err)
	}
	if restored, err := mp3tags.ReadAudio(src.Path); err != nil || !bytes.Equal(restored, original) {
		t.Errorf("go-fs did not restore the audio mp3gain changed (%v)", err)
	}
}
