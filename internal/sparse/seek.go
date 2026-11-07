//go:build linux || darwin

package sparse

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Extents reports which of the n bytes at off hold data and which are holes,
// by SEEK_DATA and SEEK_HOLE. A filesystem that knows nothing of holes
// reports everything as data.
func Extents(f *os.File, off, n int64) ([]Extent, error) {
	if n <= 0 {
		return nil, nil
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	end := off + n
	var extents []Extent
	var seekErr error
	controlErr := raw.Control(func(fd uintptr) {
		for pos := off; pos < end; {
			data, err := unix.Seek(int(fd), pos, unix.SEEK_DATA)
			switch {
			case errors.Is(err, unix.ENXIO):
				// nothing but hole from here to the end of the file
				extents = append(extents, Extent{Offset: pos, Length: end - pos, Hole: true})
				return
			case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
				// no hole reporting here: all of it is data
				extents = append(extents[:0], Extent{Offset: off, Length: n})
				return
			case err != nil:
				seekErr = err
				return
			}
			if data > pos {
				stop := min(data, end)
				extents = append(extents, Extent{Offset: pos, Length: stop - pos, Hole: true})
				pos = stop
				continue
			}
			hole, err := unix.Seek(int(fd), pos, unix.SEEK_HOLE)
			if err != nil {
				seekErr = err
				return
			}
			// SEEK_HOLE never stays put on data, but a filesystem that
			// misreports must not make this loop forever
			if hole <= pos {
				hole = end
			}
			stop := min(hole, end)
			extents = append(extents, Extent{Offset: pos, Length: stop - pos})
			pos = stop
		}
	})
	if controlErr != nil {
		return nil, controlErr
	}
	if seekErr != nil {
		return nil, &os.PathError{Op: "seek", Path: f.Name(), Err: seekErr}
	}
	return merge(extents), nil
}

// Allocated is how many bytes of the filesystem the file at path takes.
func Allocated(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Blocks) * 512, nil
	}
	return info.Size(), nil
}
