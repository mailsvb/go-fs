//go:build !light && !no_mp3tags

package mp3edit

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

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
	_, info.Measured = cached(src)
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
	// measured is what the audio was measured at before this save, which
	// the file keeps under its new version, shifted by the change
	var measured mp3gain.Result
	var known bool
	if e.Steps != 0 || e.ReplayGain || e.Album != nil {
		res, err := analysisFrom(src, r)
		if err != nil {
			return err
		}
		measured, known = res, true
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
	} else {
		measured, known = cached(src)
	}
	if t.Equal(before) {
		return nil
	}

	var transform func([]byte) error
	if steps := e.Steps; steps != 0 {
		transform = func(audio []byte) error { return applyGain(audio, steps) }
	}
	err = write(src, r, t, transform, closeOnce)
	forget(src)
	if err == nil && known {
		carry(src, measured, e.Steps)
	}
	return err
}

// write replaces the file with its new tags, and its audio changed by
// transform where that is not nil.
func write(src Source, r io.ReaderAt, t mp3tags.Tags, transform func([]byte) error, closeOnce func() error) error {
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
// that planning an album and saving it do not decode its files again. Behind
// the memory, the persistent data file keeps them, so that they outlast the
// process and a file is measured once for as long as it stays as it is.
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

// keepFor is how long the persistent data file keeps an analysis after it was
// last used; one of a file that changed is no longer used and runs out.
const keepFor = 180 * 24 * time.Hour

// storedVersion is the version of the records of the persistent data file:
// one of another version is not used, and its file is measured again.
const storedVersion = 1

var persisted struct {
	sync.RWMutex
	bucket Bucket
	logger *slog.Logger
}

// now is the clock the records of the persistent data file run out by.
var now = time.Now

// Persist keeps analyses in a bucket of the persistent data file beyond the
// memory of the process; nil keeps them in memory only.
func Persist(b Bucket, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	persisted.Lock()
	defer persisted.Unlock()
	persisted.bucket, persisted.logger = b, logger
}

func persistence() (Bucket, *slog.Logger) {
	persisted.RLock()
	defer persisted.RUnlock()
	return persisted.bucket, persisted.logger
}

// stored is an analysis as the persistent data file keeps it: the loudness
// nil for silence, which JSON has no number for, and the energies of the
// blocks as float32 in little-endian order, which keeps a minute of audio in
// 2.4 KB and the loudness of an album within far less than a step.
type stored struct {
	V          int       `json:"v"`
	Frames     int       `json:"frames"`
	SampleRate int       `json:"sampleRate"`
	Mono       bool      `json:"mono,omitempty"`
	MinGain    int       `json:"minGain"`
	MaxGain    int       `json:"maxGain"`
	Loudness   *float64  `json:"loudness"`
	Peak       float64   `json:"peak"`
	Blocks     []byte    `json:"blocks"`
	Until      time.Time `json:"until"`
}

func toStored(res mp3gain.Result, until time.Time) stored {
	s := stored{V: storedVersion, Frames: res.Frames, SampleRate: res.SampleRate, Mono: res.Mono,
		MinGain: res.MinGain, MaxGain: res.MaxGain, Peak: res.Peak, Until: until,
		Blocks: make([]byte, 4*len(res.Blocks))}
	if !math.IsInf(res.Loudness, -1) {
		loudness := res.Loudness
		s.Loudness = &loudness
	}
	for i, b := range res.Blocks {
		binary.LittleEndian.PutUint32(s.Blocks[4*i:], math.Float32bits(float32(b)))
	}
	return s
}

// result is the analysis a record keeps, false for one of another version.
func (s stored) result() (mp3gain.Result, bool) {
	if s.V != storedVersion || s.Frames <= 0 || len(s.Blocks)%4 != 0 {
		return mp3gain.Result{}, false
	}
	res := mp3gain.Result{
		Stats: mp3gain.Stats{Frames: s.Frames, SampleRate: s.SampleRate, Mono: s.Mono,
			MinGain: s.MinGain, MaxGain: s.MaxGain},
		Loudness: math.Inf(-1), Peak: s.Peak, Blocks: make([]float64, len(s.Blocks)/4),
	}
	if s.Loudness != nil {
		res.Loudness = *s.Loudness
	}
	for i := range res.Blocks {
		res.Blocks[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(s.Blocks[4*i:])))
	}
	return res, true
}

func versionOf(src Source) string {
	return src.Key + "\x00" + strconv.FormatInt(src.Size, 10) + "\x00" + strconv.FormatInt(src.ModTime.UnixNano(), 10)
}

// nameOf is the key of a file as a log shows it.
func nameOf(src Source) string {
	return strings.ReplaceAll(src.Key, "\x00", " ")
}

// cached is what the file as it is was measured at, from the memory or the
// persistent data file, false where it was not measured.
func cached(src Source) (mp3gain.Result, bool) {
	key := versionOf(src)
	if res, ok := recalled(key); ok {
		return res, true
	}
	bucket, logger := persistence()
	if bucket == nil {
		return mp3gain.Result{}, false
	}
	var s stored
	found, err := bucket.Get(key, &s)
	if err != nil {
		logger.Warn("mp3edit: reading a kept analysis", "file", nameOf(src), "err", err)
		return mp3gain.Result{}, false
	}
	if !found {
		return mp3gain.Result{}, false
	}
	res, ok := s.result()
	if !ok {
		return mp3gain.Result{}, false
	}
	memorize(key, res)
	if s.Until.Sub(now()) < keepFor/2 {
		persist(src, key, res)
	}
	return res, true
}

// remember keeps what the file as it is was measured at.
func remember(src Source, res mp3gain.Result) {
	key := versionOf(src)
	memorize(key, res)
	persist(src, key, res)
}

func recalled(key string) (mp3gain.Result, bool) {
	cache.Lock()
	defer cache.Unlock()
	el, ok := cache.byKey[key]
	if !ok {
		return mp3gain.Result{}, false
	}
	cache.order.MoveToFront(el)
	return el.Value.(*cacheEntry).res, true
}

func memorize(key string, res mp3gain.Result) {
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

// persist writes an analysis to the persistent data file. A failure is
// logged and nothing else: the analysis holds all the same, and the file is
// measured again where it is not found next time.
func persist(src Source, key string, res mp3gain.Result) {
	bucket, logger := persistence()
	if bucket == nil {
		return
	}
	until := now().Add(keepFor)
	if err := bucket.PutUntil(key, toStored(res, until), until); err != nil {
		logger.Warn("mp3edit: keeping an analysis", "file", nameOf(src), "err", err)
	}
}

// forget drops the analysis of a version of a file that is being replaced.
func forget(src Source) {
	key := versionOf(src)
	cache.Lock()
	if el, ok := cache.byKey[key]; ok {
		cache.order.Remove(el)
		delete(cache.byKey, key)
	}
	cache.Unlock()
	bucket, logger := persistence()
	if bucket == nil {
		return
	}
	if err := bucket.Delete(key); err != nil {
		logger.Warn("mp3edit: dropping a kept analysis", "file", nameOf(src), "err", err)
	}
}

// carry keeps what a file was measured at before a save under the version
// the save left, changed by the steps the save changed its volume by, so that
// the file is not measured again. Where the new version is not known, the
// file is measured again next time.
func carry(src Source, res mp3gain.Result, steps int) {
	if src.Stat == nil {
		return
	}
	res, ok := shifted(res, steps)
	if !ok {
		return
	}
	size, modTime, err := src.Stat()
	if err != nil {
		return
	}
	next := src
	next.Size, next.ModTime = size, modTime
	remember(next, res)
}

// shifted is what audio measured at res measures at once its global gains
// are changed by steps: each step scales the samples by 2^(1/4), their
// energies by 2^(1/2). False where the louder audio would clip, which the
// decoder cuts and so changes what it measures.
func shifted(res mp3gain.Result, steps int) (mp3gain.Result, bool) {
	if steps == 0 {
		return res, true
	}
	peak := res.Peak * math.Pow(2, float64(steps)/4)
	if peak > 1 {
		return mp3gain.Result{}, false
	}
	energy := math.Pow(2, float64(steps)/2)
	blocks := make([]float64, len(res.Blocks))
	for i, b := range res.Blocks {
		blocks[i] = b * energy
	}
	stats := res.Stats
	if stats.MaxGain != 0 {
		// a gain of 0 is silence, which no change touches
		stats.MinGain += steps
		stats.MaxGain += steps
	}
	return mp3gain.Result{Stats: stats, Loudness: mp3gain.Loudness(blocks), Peak: peak, Blocks: blocks}, true
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
