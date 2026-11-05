//go:build !light && !no_mp3tags

package mp3edit

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"sync"

	"github.com/mailsvb/go-tag/mp3gain"
	"github.com/mailsvb/go-tag/mp3tags"
)

// Compiled says the binary was built with the part.
const Compiled = true

// Genres are the names the genre field suggests, in alphabetical order.
func Genres() []string { return mp3tags.SortedGenres() }

// keep is how existing tags are written: everything the editor does not
// manage stays as it is.
var keep = mp3tags.Overwrite(false)

// Read returns the fields, the cover and the volume information of a file.
func Read(src Source) (Info, error) {
	t, err := readTags(src)
	if err != nil {
		return Info{}, err
	}
	info := Info{
		Fields: Fields{Title: t.Title, Artist: t.Artist, AlbumArtist: t.AlbumArtist, Album: t.Album,
			Genre: t.Genre, Year: t.Year, Track: t.Track},
		Gain: Gain{Steps: t.Gain.Steps, DB: float64(t.Gain.Steps) * StepDB,
			TrackGain: t.Gain.TrackGain, TrackPeak: t.Gain.TrackPeak,
			AlbumGain: t.Gain.AlbumGain, AlbumPeak: t.Gain.AlbumPeak},
	}
	if t.Cover != nil && len(t.Cover.Data) > 0 {
		info.Cover = &Cover{MIME: coverType(t.Cover), Size: len(t.Cover.Data)}
	}
	return info, nil
}

// CoverOf returns the cover image of a file and its media type, nil for a
// file without one.
func CoverOf(src Source) (data []byte, mime string, err error) {
	t, err := readTags(src)
	if err != nil || t.Cover == nil || len(t.Cover.Data) == 0 {
		return nil, "", err
	}
	return t.Cover.Data, coverType(t.Cover), nil
}

// coverType is the media type of a cover, as the tag names it or, where it
// names none that is an image, as its bytes say.
func coverType(p *mp3tags.Picture) string {
	switch p.MIME {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return p.MIME
	case "image/jpg":
		return "image/jpeg"
	}
	return http.DetectContentType(p.Data)
}

func readTags(src Source) (mp3tags.Tags, error) {
	r, closer, err := src.Open()
	if err != nil {
		return mp3tags.Tags{}, err
	}
	defer func() { _ = closer.Close() }()
	return mp3tags.ReadFrom(r, src.Size)
}

// Analyze measures the loudness of a file and plans the change that brings
// it to the target loudness. What it found is kept until the file changes, for
// the album it is planned with and for its save.
func Analyze(src Source) (Analysis, error) {
	res, err := analysis(src)
	if err != nil {
		return Analysis{}, err
	}
	return Analysis{MaxUp: res.MaxUp(), MaxDown: res.MaxDown(), Track: changeOf(mp3gain.PlanTrack(res))}, nil
}

// PlanAlbum plans the one change for all files of an album, measuring those
// not measured yet: the album as a whole reaches the target loudness, and the
// files keep their levels to each other.
func PlanAlbum(srcs []Source) (Album, error) {
	if len(srcs) == 0 {
		return Album{}, errors.New("an album needs at least one file")
	}
	results := make([]mp3gain.Result, 0, len(srcs))
	up, down := math.MaxInt, math.MaxInt
	for _, src := range srcs {
		res, err := analysis(src)
		if err != nil {
			return Album{}, fmt.Errorf("%s: %w", src.Key, err)
		}
		results = append(results, res)
		up, down = min(up, res.MaxUp()), min(down, res.MaxDown())
	}
	return Album{Change: changeOf(mp3gain.PlanAlbum(results)), MaxUp: up, MaxDown: down}, nil
}

func changeOf(c mp3gain.Change) Change {
	return Change{Steps: c.Steps, DB: c.DB(), Limited: c.Limited, Loudness: lufs(c.Loudness), Peak: c.Peak}
}

// lufs is a loudness as JSON carries it: nil for silence, which is -Inf.
func lufs(v float64) *float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	v = math.Round(v*100) / 100
	return &v
}

// Save writes an edit to a file. The fields the editor does not manage, and
// every other tag of the file, stay as they are; the audio is only touched by
// a change of the volume. A file another save is writing waits for it.
func Save(src Source, e Edit) error {
	unlock := lock(src.Key)
	defer unlock()

	r, closer, err := src.Open()
	if err != nil {
		return err
	}
	closeOnce := sync.OnceValue(closer.Close)
	defer func() { _ = closeOnce() }()

	before, err := mp3tags.ReadFrom(r, src.Size)
	if err != nil {
		return err
	}
	t := before
	if f := e.Fields; f != nil {
		t.Title, t.Artist, t.AlbumArtist, t.Album = f.Title, f.Artist, f.AlbumArtist, f.Album
		t.Genre, t.Year, t.Track = f.Genre, f.Year, f.Track
	}
	switch {
	case e.Cover != nil:
		kind := http.DetectContentType(e.Cover.Data)
		if len(e.Cover.Data) == 0 || len(e.Cover.Data) > MaxCover || (kind != "image/jpeg" && kind != "image/png") {
			return ErrCover
		}
		t.Cover = &mp3tags.Picture{MIME: kind, Data: e.Cover.Data}
	case e.RemoveCover:
		t.Cover = nil
	}
	if e.Steps != 0 || e.ReplayGain || e.Album != nil {
		res, err := analysisFrom(src, r)
		if err != nil {
			return err
		}
		if e.Steps > res.MaxUp() || -e.Steps > res.MaxDown() {
			return ErrOutOfRange
		}
		track := mp3gain.Change{Steps: e.Steps, Loudness: res.Loudness, Peak: res.Peak}
		t.Gain.TrackGain, t.Gain.TrackPeak = track.ReplayGain()
		switch {
		case e.Album != nil:
			album := mp3gain.Change{Steps: e.Steps, Loudness: math.Inf(-1), Peak: e.Album.Peak}
			if e.Album.Loudness != nil {
				album.Loudness = *e.Album.Loudness
			}
			t.Gain.AlbumGain, t.Gain.AlbumPeak = album.ReplayGain()
		case e.Steps != 0:
			// the album's values were measured before this change, and no
			// longer say anything about it
			t.Gain.AlbumGain, t.Gain.AlbumPeak = "", ""
		}
		t.Gain.Steps += e.Steps
	}
	if t.Equal(before) {
		return nil
	}

	var transform func([]byte) error
	if steps := e.Steps; steps != 0 {
		transform = func(audio []byte) error { return applyGain(audio, steps) }
	}
	defer forget(src)
	if src.Path != "" {
		// the file is rewritten beside itself and renamed over what was read,
		// which some systems refuse while it is still open
		if err := closeOnce(); err != nil {
			return err
		}
		if transform == nil {
			return mp3tags.Write(src.Path, t, keep)
		}
		return mp3tags.WriteAudio(src.Path, t, transform, keep)
	}
	return replace(src, func(w io.Writer) error {
		var err error
		if transform == nil {
			err = mp3tags.WriteTo(w, r, src.Size, t, keep)
		} else {
			err = mp3tags.WriteAudioTo(w, r, src.Size, t, transform, keep)
		}
		// done with the original before the new file takes its name
		if closed := closeOnce(); err == nil {
			err = closed
		}
		return err
	})
}

func applyGain(audio []byte, steps int) error {
	err := mp3gain.Apply(audio, steps)
	switch {
	case errors.Is(err, mp3gain.ErrOutOfRange):
		return ErrOutOfRange
	case errors.Is(err, mp3gain.ErrNoFrames), errors.Is(err, mp3gain.ErrFreeFormat):
		return ErrNotMP3
	}
	return err
}

// errStopped is what the writing of a file is stopped with when Put gave up
// before it read all of it.
var errStopped = errors.New("the upload of the file stopped")

// replace streams the new file, which write produces, into Put.
func replace(src Source, write func(w io.Writer) error) error {
	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := write(writer)
		_ = writer.CloseWithError(err)
		written <- err
	}()
	err := src.Put(reader)
	_ = reader.CloseWithError(errStopped)
	if failed := <-written; failed != nil {
		return failed
	}
	return err
}

// analysis is what a file was measured at, measured now if it was not yet.
func analysis(src Source) (mp3gain.Result, error) {
	if res, ok := cached(src); ok {
		return res, nil
	}
	r, closer, err := src.Open()
	if err != nil {
		return mp3gain.Result{}, err
	}
	defer func() { _ = closer.Close() }()
	return analysisFrom(src, r)
}

// analysisFrom is analysis for a file already open.
func analysisFrom(src Source, r io.ReaderAt) (mp3gain.Result, error) {
	if res, ok := cached(src); ok {
		return res, nil
	}
	if src.Size > MaxAudio {
		return mp3gain.Result{}, ErrTooLarge
	}
	audio, err := mp3tags.ReadAudioFrom(r, src.Size)
	if err != nil {
		return mp3gain.Result{}, err
	}
	decoding <- struct{}{}
	res, err := mp3gain.Analyze(audio)
	<-decoding
	switch {
	case errors.Is(err, mp3gain.ErrNoFrames), errors.Is(err, mp3gain.ErrFreeFormat):
		return mp3gain.Result{}, ErrNotMP3
	case err != nil:
		return mp3gain.Result{}, err
	}
	remember(src, res)
	return res, nil
}

// decoding holds a place for every file decoded at once: a decode keeps a
// core busy for as long as it takes.
var decoding = make(chan struct{}, max(1, runtime.NumCPU()))

// The analyses of the files measured last, by the file and its version, so
// that planning an album and saving it do not decode its files again.
const cacheSize = 256

var cache = struct {
	sync.Mutex
	order *list.List
	byKey map[string]*list.Element
}{order: list.New(), byKey: map[string]*list.Element{}}

type cacheEntry struct {
	key string
	res mp3gain.Result
}

func versionOf(src Source) string {
	return src.Key + "\x00" + strconv.FormatInt(src.Size, 10) + "\x00" + strconv.FormatInt(src.ModTime.UnixNano(), 10)
}

func cached(src Source) (mp3gain.Result, bool) {
	cache.Lock()
	defer cache.Unlock()
	el, ok := cache.byKey[versionOf(src)]
	if !ok {
		return mp3gain.Result{}, false
	}
	cache.order.MoveToFront(el)
	return el.Value.(*cacheEntry).res, true
}

func remember(src Source, res mp3gain.Result) {
	key := versionOf(src)
	cache.Lock()
	defer cache.Unlock()
	if el, ok := cache.byKey[key]; ok {
		el.Value.(*cacheEntry).res = res
		cache.order.MoveToFront(el)
		return
	}
	cache.byKey[key] = cache.order.PushFront(&cacheEntry{key: key, res: res})
	for cache.order.Len() > cacheSize {
		oldest := cache.order.Back()
		cache.order.Remove(oldest)
		delete(cache.byKey, oldest.Value.(*cacheEntry).key)
	}
}

// forget drops the analysis of a version of a file that is being replaced.
func forget(src Source) {
	key := versionOf(src)
	cache.Lock()
	defer cache.Unlock()
	if el, ok := cache.byKey[key]; ok {
		cache.order.Remove(el)
		delete(cache.byKey, key)
	}
}

// locks are the files being saved, each with how many saves wait for it.
var locks = struct {
	sync.Mutex
	held map[string]*fileLock
}{held: map[string]*fileLock{}}

type fileLock struct {
	sync.Mutex
	waiting int
}

// lock waits until no other save writes the file, and holds it until unlock
// is called.
func lock(key string) (unlock func()) {
	locks.Lock()
	l := locks.held[key]
	if l == nil {
		l = &fileLock{}
		locks.held[key] = l
	}
	l.waiting++
	locks.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		locks.Lock()
		l.waiting--
		if l.waiting == 0 {
			delete(locks.held, key)
		}
		locks.Unlock()
	}
}
