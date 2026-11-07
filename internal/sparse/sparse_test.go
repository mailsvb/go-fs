package sparse

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const mib = 1 << 20

// image is a file of size bytes made the way the Disks page makes one: marked
// sparse and extended by Truncate, so that all of it is a hole.
func image(t *testing.T, size int64) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "disk.raw"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := SetSparse(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return f
}

func pattern(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func readAt(t *testing.T, f *os.File, off int64, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	return b
}

// covers reports whether the extents tile [off, off+n) without a gap.
func covers(extents []Extent, off, n int64) bool {
	pos := off
	for _, e := range extents {
		if e.Offset != pos || e.Length <= 0 {
			return false
		}
		pos += e.Length
	}
	return pos == off+n
}

// dataAt reports whether the extents call the byte at off data.
func dataAt(extents []Extent, off int64) bool {
	for _, e := range extents {
		if off >= e.Offset && off < e.Offset+e.Length {
			return !e.Hole
		}
	}
	return false
}

// reportsHoles is whether the filesystem of the test keeps holes and says
// where they are: a file with one byte in the middle, synced, has to come
// back as hole, data and hole. APFS reports the holes of a truncated file
// until the first sync and allocates all of it then.
func reportsHoles(t *testing.T) bool {
	t.Helper()
	f := image(t, 4*mib)
	if _, err := f.WriteAt([]byte{1}, 2*mib); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	extents, err := Extents(f, 0, 4*mib)
	if err != nil {
		t.Fatal(err)
	}
	return len(extents) == 3 && extents[0].Hole && !extents[1].Hole && extents[2].Hole
}

func TestExtentsTileTheRangeAndFindTheData(t *testing.T) {
	f := image(t, 16*mib)
	if _, err := f.WriteAt(pattern(64*1024, 0xab), 5*mib); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]int64{{0, 16 * mib}, {5 * mib, 4096}, {mib, 3 * mib}, {5*mib + 4096, 10 * mib}} {
		extents, err := Extents(f, r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		if !covers(extents, r[0], r[1]) {
			t.Errorf("Extents(%d, %d) = %+v, which does not tile the range", r[0], r[1], extents)
		}
		if r[0] <= 5*mib && 5*mib < r[0]+r[1] && !dataAt(extents, 5*mib) {
			t.Errorf("Extents(%d, %d) = %+v calls the written data a hole", r[0], r[1], extents)
		}
	}
	if !reportsHoles(t) {
		t.Skipf("the filesystem of %s reports no holes", t.TempDir())
	}
	extents, _ := Extents(f, 0, 16*mib)
	if dataAt(extents, 0) || dataAt(extents, 15*mib) {
		t.Errorf("the untouched start and end are reported as data: %+v", extents)
	}
}

func TestPunchHoleReadsAsZerosAndKeepsTheSize(t *testing.T) {
	f := image(t, 8*mib)
	if _, err := f.WriteAt(pattern(4*mib, 0xcd), 2*mib); err != nil {
		t.Fatal(err)
	}
	err := PunchHole(f, 3*mib, mib)
	if !HolePunching {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("PunchHole on %s = %v, want ErrUnsupported", runtime.GOOS, err)
		}
		return
	}
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("the filesystem of %s punches no holes", t.TempDir())
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, f, 3*mib, mib); !IsZero(got) {
		t.Error("the punched range does not read as zeros")
	}
	if got := readAt(t, f, 2*mib, mib); !bytes.Equal(got, pattern(mib, 0xcd)) {
		t.Error("the data before the hole changed")
	}
	if got := readAt(t, f, 4*mib, mib); !bytes.Equal(got, pattern(mib, 0xcd)) {
		t.Error("the data after the hole changed")
	}
	if info, _ := f.Stat(); info.Size() != 8*mib {
		t.Errorf("the size is %d after a punch", info.Size())
	}
	if reportsHoles(t) {
		extents, _ := Extents(f, 0, 8*mib)
		if dataAt(extents, 3*mib+mib/2) {
			t.Errorf("the punched range is still reported as data: %+v", extents)
		}
	}
}

func TestZeroRangeReadsAsZeros(t *testing.T) {
	f := image(t, 4*mib)
	if _, err := f.WriteAt(pattern(2*mib, 0x11), mib); err != nil {
		t.Fatal(err)
	}
	if err := ZeroRange(f, mib+100, 300000); err != nil {
		t.Fatal(err)
	}
	got := readAt(t, f, mib, 2*mib)
	want := pattern(2*mib, 0x11)
	copy(want[100:100+300000], make([]byte, 300000))
	if !bytes.Equal(got, want) {
		t.Error("ZeroRange zeroed the wrong bytes")
	}
}

func TestWriteAtSparseLeavesZeroBlocksOut(t *testing.T) {
	f := image(t, 4*mib)
	// an unaligned start, a run of data, a long run of zeros and data again
	data := append(append(pattern(10000, 0x5a), make([]byte, 2*mib)...), pattern(5000, 0xa5)...)
	written, err := WriteAtSparse(f, data, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, f, 1000, len(data)); !bytes.Equal(got, data) {
		t.Fatal("the file does not read back what was written")
	}
	if written >= int64(len(data))/2 {
		t.Errorf("%d of %d bytes were written, the zeros were not left out", written, len(data))
	}
	if reportsHoles(t) {
		extents, _ := Extents(f, 0, 4*mib)
		if dataAt(extents, 1000+10000+mib) {
			t.Errorf("the zeros in the middle are reported as data: %+v", extents)
		}
	}
}

func TestIsZero(t *testing.T) {
	if !IsZero(nil) || !IsZero(make([]byte, 3*mib+7)) {
		t.Error("zeros are not zero")
	}
	b := make([]byte, 3*mib+7)
	b[len(b)-1] = 1
	if IsZero(b) {
		t.Error("a one at the very end is missed")
	}
}

func TestCopyFileCopiesContentAndRefusesAnExistingTarget(t *testing.T) {
	f := image(t, 8*mib)
	if _, err := f.WriteAt(pattern(mib, 0x77), 6*mib); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(pattern(10, 0x01), 0); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.raw")
	var last [2]int64
	if err := CopyFile(context.Background(), dst, f.Name(), func(done, total int64) { last = [2]int64{done, total} }); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(f.Name())
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the copy differs from the original")
	}
	if last != [2]int64{8 * mib, 8 * mib} {
		t.Errorf("the last progress was %v", last)
	}
	if err := CopyFile(context.Background(), dst, f.Name(), nil); !errors.Is(err, os.ErrExist) {
		t.Errorf("a copy onto an existing file = %v, want ErrExist", err)
	}
}

// The copy that reads the extents, which is the one a filesystem that cannot
// share blocks gets, copies what it should, keeps the holes and stops when
// asked, leaving nothing behind.
func TestTheExtentCopyKeepsTheContentAndTheHoles(t *testing.T) {
	f := image(t, 8*mib)
	data := append(pattern(mib, 0x44), make([]byte, 2*mib)...)
	if _, err := f.WriteAt(data, 2*mib); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.raw")
	if err := copyExtents(context.Background(), dst, f.Name(), nil); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(f.Name())
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatal("the copy differs from the original")
	}
	if !reportsHoles(t) {
		return
	}
	copied, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	extents, _ := Extents(copied, 0, 8*mib)
	// the zeros the original holds as data are a hole of the copy
	if dataAt(extents, 0) || dataAt(extents, 3*mib+mib/2) || !dataAt(extents, 2*mib) {
		t.Errorf("the copy has the extents %+v", extents)
	}
}

func TestTheExtentCopyIsCancelledCleanly(t *testing.T) {
	f := image(t, 8*mib)
	if _, err := f.WriteAt(pattern(4*mib, 0x33), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "copy.raw")
	err := copyExtents(ctx, dst, f.Name(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a cancelled copy left %s behind", dst)
	}
}

func TestAllocatedIsLessThanTheSizeOfASparseFile(t *testing.T) {
	f := image(t, 64*mib)
	allocated, err := Allocated(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !reportsHoles(t) {
		t.Skip("the filesystem reports no holes")
	}
	if allocated >= 64*mib {
		t.Errorf("an empty 64 MiB image takes %d bytes", allocated)
	}
}
