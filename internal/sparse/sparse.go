// Package sparse is what the Disks page and the NBD server need of a file
// beyond reading and writing it: to keep a disk image sparse, so that a 100 GB
// disk takes only the space its guest has written, to give space back when the
// guest trims, and to say which parts of an image hold data at all.
//
// Linux and Windows punch holes; elsewhere a punch is unsupported and a range
// that has to read as zeros is written with zeros. Linux and macOS report
// holes with SEEK_DATA and SEEK_HOLE, Windows with FSCTL_QUERY_ALLOCATED_RANGES;
// elsewhere the whole file is reported as data, which is never wrong, only
// less useful.
package sparse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrUnsupported is what PunchHole and a clone return where the platform or
// the filesystem cannot do it.
var ErrUnsupported = errors.ErrUnsupported

// Extent is a run of a file that is all data or all hole. A hole reads as
// zeros and takes no space; data may hold zeros too, it is simply allocated.
type Extent struct {
	Offset int64
	Length int64
	Hole   bool
}

// zeros is what WriteZeros writes from, so that it allocates nothing per call.
var zeros = make([]byte, 1<<20)

// WriteZeros writes n zero bytes at off, which is the way to make a range read
// as zeros where nothing better is available.
func WriteZeros(f *os.File, off, n int64) error {
	for n > 0 {
		chunk := min(n, int64(len(zeros)))
		if _, err := f.WriteAt(zeros[:chunk], off); err != nil {
			return err
		}
		off += chunk
		n -= chunk
	}
	return nil
}

// IsZero reports whether b holds nothing but zero bytes.
func IsZero(b []byte) bool {
	for len(b) > 0 {
		chunk := min(len(b), len(zeros))
		if !bytes.Equal(b[:chunk], zeros[:chunk]) {
			return false
		}
		b = b[chunk:]
	}
	return true
}

// BlockSize is the granularity zero detection works at: a block of this
// size that holds nothing but zeros is left as a hole rather than written.
const BlockSize = 4096

// WriteAtSparse writes b at off, but leaves out every BlockSize-aligned block
// that is all zeros, so that a file written into a hole stays a hole where
// the data is zeros. The file has to read as zeros at off already, which a
// file that was extended by Truncate does. It reports how many bytes it
// actually wrote.
func WriteAtSparse(f *os.File, b []byte, off int64) (written int64, err error) {
	for len(b) > 0 {
		// up to the next block boundary, so that every block but the first
		// and the last is aligned
		n := BlockSize - int(off%BlockSize)
		n = min(n, len(b))
		if !IsZero(b[:n]) {
			// write a run of non-zero blocks in one call
			end := n
			for end < len(b) {
				next := min(end+BlockSize, len(b))
				if IsZero(b[end:next]) {
					break
				}
				end = next
			}
			if _, err := f.WriteAt(b[:end], off); err != nil {
				return written, err
			}
			written += int64(end)
			n = end
		}
		b = b[n:]
		off += int64(n)
	}
	return written, nil
}

// merge joins neighbouring extents of the same kind and drops empty ones.
func merge(extents []Extent) []Extent {
	out := extents[:0]
	for _, e := range extents {
		if e.Length <= 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Hole == e.Hole && out[n-1].Offset+out[n-1].Length == e.Offset {
			out[n-1].Length += e.Length
			continue
		}
		out = append(out, e)
	}
	return out
}

// CopyFile makes dst a copy of src, which has to be a regular file, and
// fails when dst exists. Where the filesystem can share the blocks of the two
// files, as btrfs, XFS and APFS can, the copy takes no time and no space.
// Otherwise only what src holds data in is read, and a block of zeros in it
// becomes a hole of dst, so the copy of a sparse image is as sparse. progress,
// when not nil, is told how many bytes of the size of src are done. A copy
// that fails or is cancelled is removed.
func CopyFile(ctx context.Context, dst, src string, progress func(done, total int64)) (err error) {
	switch err := cloneFile(dst, src); {
	case err == nil:
		if progress != nil {
			if info, statErr := os.Stat(dst); statErr == nil {
				progress(info.Size(), info.Size())
			}
		}
		return nil
	case !errors.Is(err, ErrUnsupported):
		return err
	}
	return copyExtents(ctx, dst, src, progress)
}

// copyExtents is CopyFile where no block can be shared: it reads the data of
// src and leaves its holes, and its blocks of zeros, out of dst.
func copyExtents(ctx context.Context, dst, src string, progress func(done, total int64)) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if err := SetSparse(out); err != nil {
		return err
	}
	size := info.Size()
	if err := out.Truncate(size); err != nil {
		return err
	}
	extents, err := Extents(in, 0, size)
	if err != nil {
		return err
	}
	buffer := make([]byte, 1<<20)
	var done int64
	for _, extent := range extents {
		if extent.Hole {
			done += extent.Length
			if progress != nil {
				progress(done, size)
			}
			continue
		}
		for off, end := extent.Offset, extent.Offset+extent.Length; off < end; {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := buffer[:min(int64(len(buffer)), end-off)]
			n, err := in.ReadAt(chunk, off)
			if err != nil && !(errors.Is(err, io.EOF) && n == len(chunk)) {
				return err
			}
			if _, err := WriteAtSparse(out, chunk, off); err != nil {
				return err
			}
			off += int64(n)
			done += int64(n)
			if progress != nil {
				progress(done, size)
			}
		}
	}
	return out.Sync()
}
