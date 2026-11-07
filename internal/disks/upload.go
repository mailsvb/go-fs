package disks

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/mailsvb/go-fs/internal/sparse"
)

// An upload arrives in chunks, the convention the file listing uploads by:
// each names its place with start, end and total, and has to start where
// what is staged ends, which is the only record of how far the upload got.
// A chunk that does not is refused with an *OutOfSyncError that says where it
// would have fitted, so a client can pick up from there.
//
// The staging file is extended to the end of every chunk, and a block of
// zeros in a chunk is never written, so an upload of a raw image is as sparse
// as the image is empty.

// ErrShortChunk is a chunk whose body ended before the end it named.
var ErrShortChunk = errors.New("the chunk ended before the end it named")

// OutOfSyncError is a chunk that does not continue what is staged.
type OutOfSyncError struct{ Have int64 }

func (e *OutOfSyncError) Error() string {
	return fmt.Sprintf("expected the chunk to start at %d", e.Have)
}

// Chunk is one piece of an upload: bytes start to end of total, end
// inclusive.
type Chunk struct {
	Start, End, Total int64
}

// Last reports whether the chunk ends the upload.
func (c Chunk) Last() bool { return c.End+1 == c.Total }

// WriteChunk stores one chunk of the upload of the image name. When it
// reports the upload complete, FinishUpload puts it in place.
func (f *Folder) WriteChunk(name string, chunk Chunk, body io.Reader) (complete bool, err error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	if chunk.Start < 0 || chunk.Start > chunk.End || chunk.End >= chunk.Total || chunk.Total > MaxSize {
		return false, fmt.Errorf("bytes %d-%d/%d %w", chunk.Start, chunk.End, chunk.Total, ErrSize)
	}
	key := f.key(name)
	lock := f.reg.uploadLock(key)
	if !lock.TryLock() {
		return false, &BusyError{Name: name, Reason: "is being uploaded by another request right now"}
	}
	defer lock.Unlock()
	f.reg.begin("upload", key)
	defer f.reg.end("upload", key)

	staging := f.stagingPath("upload", name)
	if chunk.Start != 0 {
		info, err := os.Stat(staging)
		switch {
		case err == nil && info.Size() == chunk.Start:
			// continues the upload
		case err == nil && info.Size() == chunk.End+1:
			// this very chunk landed already, and the answer was lost
			return chunk.Last(), nil
		case err == nil:
			return false, &OutOfSyncError{Have: info.Size()}
		case errors.Is(err, fs.ErrNotExist):
			return false, &OutOfSyncError{Have: 0}
		default:
			return false, err
		}
	}

	file, err := os.OpenFile(staging, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if chunk.Start == 0 {
		// a client that starts over from byte zero may, whatever an
		// abandoned attempt left staged
		if err := file.Truncate(0); err != nil {
			return false, err
		}
		if err := sparse.SetSparse(file); err != nil {
			return false, err
		}
	}
	size := chunk.End - chunk.Start + 1
	undo := func(err error) (bool, error) {
		_ = file.Truncate(chunk.Start)
		return false, err
	}
	buffer := make([]byte, min(size, 1<<20))
	var received int64
	for received < size {
		n, readErr := io.ReadFull(body, buffer[:min(int64(len(buffer)), size-received)])
		if n > 0 {
			if _, err := sparse.WriteAtSparse(file, buffer[:n], chunk.Start+received); err != nil {
				return undo(err)
			}
			received += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return undo(ErrShortChunk)
			}
			return undo(readErr)
		}
	}
	// a chunk that ends in zeros wrote nothing there: the size says how far
	// the upload got all the same
	if err := file.Truncate(chunk.End + 1); err != nil {
		return undo(err)
	}
	if chunk.Last() {
		if err := file.Sync(); err != nil {
			return undo(err)
		}
	}
	return chunk.Last(), nil
}

// FinishUpload puts the complete upload of the image name of total bytes in
// place. An image of that name is replaced only when replace says so, and
// only while nothing has it in use.
func (f *Folder) FinishUpload(name string, total int64, replace bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	staging := f.stagingPath("upload", name)
	info, err := os.Stat(staging)
	if err != nil {
		return err
	}
	if info.Size() != total {
		return fmt.Errorf("the upload of %q holds %d bytes, not %d", name, info.Size(), total)
	}
	lease, err := f.reg.acquire(f.key(name), name, Exclusive, nil, "being replaced by an upload")
	if err != nil {
		return err
	}
	defer lease.Release()
	if existing, err := os.Lstat(f.path(name)); err == nil {
		if !replace || !existing.Mode().IsRegular() {
			return fmt.Errorf("%q %w", name, ErrExists)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(staging, f.path(name))
}

// Exists reports whether there is an image called name.
func (f *Folder) Exists(name string) bool {
	_, err := f.Stat(name)
	return err == nil
}
