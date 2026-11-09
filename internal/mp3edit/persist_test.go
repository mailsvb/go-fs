//go:build !light && !no_mp3tags

package mp3edit

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/mailsvb/go-tag/mp3gain"
)

// bucket is a persistent data file in memory, which keeps records as JSON
// as the real one does.
type bucket struct {
	records map[string][]byte
	until   map[string]time.Time
	puts    int
}

func (b *bucket) Get(key string, v any) (bool, error) {
	data, ok := b.records[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(data, v)
}

func (b *bucket) PutUntil(key string, v any, expires time.Time) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.records[key] = data
	b.until[key] = expires
	b.puts++
	return nil
}

func (b *bucket) Delete(key string) error {
	delete(b.records, key)
	delete(b.until, key)
	return nil
}

// persistIn keeps the analyses of a test in a bucket of its own, with the
// memory of the process empty, and keeps them in memory only again after it.
func persistIn(t *testing.T) *bucket {
	t.Helper()
	b := &bucket{records: map[string][]byte{}, until: map[string]time.Time{}}
	clearMemory()
	Persist(b, nil)
	t.Cleanup(func() {
		Persist(nil, nil)
		clearMemory()
		now = time.Now
	})
	return b
}

func clearMemory() {
	cache.Lock()
	defer cache.Unlock()
	cache.order.Init()
	clear(cache.byKey)
}

// audible is an MP3 that sounds, at -20.4 LUFS, which go-tag measures its
// own analysis by.
func audible(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/stereo44k.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// same reports whether two analyses found the same, within what keeping
// one in a record may change.
func same(a, b Analysis) bool {
	la, lb := a.Track.Loudness, b.Track.Loudness
	if (la == nil) != (lb == nil) || la != nil && math.Abs(*la-*lb) > 0.02 {
		return false
	}
	return math.Abs(a.Track.Peak-b.Track.Peak) < 0.001 &&
		a.MaxUp == b.MaxUp && a.MaxDown == b.MaxDown && a.Track.Steps == b.Track.Steps && a.Track.Limited == b.Track.Limited
}

// unreadable is a source whose file cannot be opened, so that what it
// answers comes from what was measured before.
func unreadable(src Source) Source {
	src.Open = func() (io.ReaderAt, io.Closer, error) {
		return nil, nil, errors.New("the file was read again")
	}
	return src
}

func TestARecordKeepsTheAnalysis(t *testing.T) {
	for _, res := range []mp3gain.Result{
		{Stats: mp3gain.Stats{Frames: 40, SampleRate: 44100, MinGain: 150, MaxGain: 150},
			Loudness: -20.25, Peak: 0.5, Blocks: []float64{0.001, 0.0123456789, 0}},
		{Stats: mp3gain.Stats{Frames: 40, SampleRate: 22050, Mono: true},
			Loudness: math.Inf(-1), Blocks: []float64{0, 0}},
	} {
		data, err := json.Marshal(toStored(res, time.Unix(1000, 0)))
		if err != nil {
			t.Fatal(err)
		}
		var s stored
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		got, ok := s.result()
		if !ok {
			t.Fatalf("the record of %+v is not used", res)
		}
		if got.Stats != res.Stats || got.Loudness != res.Loudness || got.Peak != res.Peak || len(got.Blocks) != len(res.Blocks) {
			t.Errorf("kept %+v, want %+v", got, res)
		}
		for i := range res.Blocks {
			if math.Abs(got.Blocks[i]-res.Blocks[i]) > 1e-7*res.Blocks[i] {
				t.Errorf("block %d = %v, want %v", i, got.Blocks[i], res.Blocks[i])
			}
		}
	}
	if _, ok := (stored{V: storedVersion + 1, Frames: 1}).result(); ok {
		t.Error("a record of another version is used")
	}
}

func TestAnAnalysisOutlastsTheMemory(t *testing.T) {
	b := persistIn(t)
	src := localSource(t, audible(t))
	first, err := Analyze(src)
	if err != nil {
		t.Fatal(err)
	}
	if first.Track.Loudness == nil {
		t.Fatal("the audible file measures as silence")
	}
	if len(b.records) != 1 {
		t.Fatalf("%d records kept, want 1", len(b.records))
	}

	clearMemory()
	again, err := Analyze(unreadable(src))
	if err != nil {
		t.Fatalf("measuring again decoded the file: %v", err)
	}
	if !same(again, first) {
		t.Errorf("measured again %+v, want %+v", again, first)
	}
	clearMemory()
	if _, err := PlanAlbum([]Source{unreadable(src)}); err != nil {
		t.Errorf("planning an album decoded the file: %v", err)
	}
	if info, err := Read(src); err != nil || !info.Measured {
		t.Errorf("Read = %+v, %v; want it measured", info, err)
	}
	other := localSource(t, tagged())
	if info, err := Read(other); err != nil || info.Measured {
		t.Errorf("Read of a file never measured = %+v, %v", info, err)
	}
}

func TestARecordInUseIsKeptLonger(t *testing.T) {
	b := persistIn(t)
	src := localSource(t, tagged())
	if _, err := Analyze(src); err != nil {
		t.Fatal(err)
	}
	key := versionOf(src)
	written := b.until[key]

	clearMemory()
	if _, ok := cached(src); !ok || b.until[key] != written {
		t.Fatalf("a fresh record was written again: %v → %v", written, b.until[key])
	}
	clearMemory()
	later := time.Now().Add(keepFor * 2 / 3)
	now = func() time.Time { return later }
	if _, ok := cached(src); !ok || !b.until[key].Equal(later.Add(keepFor)) {
		t.Errorf("a record used late runs out at %v, want %v", b.until[key], later.Add(keepFor))
	}
}

func TestASaveCarriesTheAnalysisOver(t *testing.T) {
	b := persistIn(t)
	src := localSource(t, audible(t))
	if _, err := Analyze(src); err != nil {
		t.Fatal(err)
	}
	if err := Save(src, Edit{Steps: -2, ReplayGain: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.records[versionOf(src)]; ok {
		t.Error("the analysis of the replaced version is still kept")
	}
	quieter := sourceOfPath(t, src.Path)
	if _, ok := b.records[versionOf(quieter)]; !ok {
		t.Fatal("the saved version has no analysis")
	}
	clearMemory()
	carried, err := Analyze(unreadable(quieter))
	if err != nil {
		t.Fatalf("the saved file was decoded again: %v", err)
	}

	// what the file measures at when it is decoded
	clearMemory()
	if err := b.Delete(versionOf(quieter)); err != nil {
		t.Fatal(err)
	}
	measured, err := Analyze(quieter)
	if err != nil {
		t.Fatal(err)
	}
	if measured.Track.Loudness == nil || !same(carried, measured) {
		t.Errorf("carried over %+v (%v), measured %+v (%v)", carried, deref(carried.Track.Loudness),
			measured, deref(measured.Track.Loudness))
	}
}

func TestASaveOfTheTagsOnlyKeepsTheAnalysis(t *testing.T) {
	persistIn(t)
	src := localSource(t, audible(t))
	first, err := Analyze(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(src, Edit{Fields: &Fields{Title: "New"}}); err != nil {
		t.Fatal(err)
	}
	clearMemory()
	again, err := Analyze(unreadable(sourceOfPath(t, src.Path)))
	if err != nil || !same(again, first) {
		t.Errorf("after a save of the tags: %+v, %v; want %+v", again, err, first)
	}
}

func TestASaveElsewhereCarriesTheAnalysisOver(t *testing.T) {
	persistIn(t)
	m := &memory{data: audible(t), modTime: time.Unix(1000, 0)}
	if _, err := Analyze(m.source()); err != nil {
		t.Fatal(err)
	}
	if err := Save(m.source(), Edit{Steps: -1}); err != nil {
		t.Fatal(err)
	}
	clearMemory()
	if _, err := Analyze(unreadable(m.source())); err != nil {
		t.Errorf("the saved file was decoded again: %v", err)
	}
}

func TestALouderVersionThatClipsIsNotCarriedOver(t *testing.T) {
	res := mp3gain.Result{Stats: mp3gain.Stats{Frames: 1, MinGain: 100, MaxGain: 200},
		Loudness: -10, Peak: 0.9, Blocks: []float64{0.1}}
	if _, ok := shifted(res, 2); ok {
		t.Error("a change that clips was carried over")
	}
	got, ok := shifted(res, -4)
	if !ok || got.MinGain != 96 || got.MaxGain != 196 || math.Abs(got.Peak-0.45) > 1e-12 ||
		math.Abs(got.Blocks[0]-0.025) > 1e-12 {
		t.Errorf("4 steps quieter = %+v, %v", got, ok)
	}
	silence := mp3gain.Result{Stats: mp3gain.Stats{Frames: 1}, Loudness: math.Inf(-1), Blocks: []float64{0}}
	if got, ok := shifted(silence, 3); !ok || got.MinGain != 0 || got.MaxGain != 0 || !math.IsInf(got.Loudness, -1) {
		t.Errorf("silence shifted = %+v, %v", got, ok)
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
