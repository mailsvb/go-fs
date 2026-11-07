package sparse

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// HolePunching reports whether PunchHole can work on this platform at all.
// macOS is where go-fs is developed, not where it is released for, so a
// trimmed range simply stays allocated here.
const HolePunching = false

// SetSparse marks a file as one that may have holes. APFS has them without
// being asked.
func SetSparse(*os.File) error { return nil }

// PunchHole is unsupported here.
func PunchHole(*os.File, int64, int64) error { return ErrUnsupported }

// ZeroRange makes n bytes at off read as zeros.
func ZeroRange(f *os.File, off, n int64) error { return WriteZeros(f, off, n) }

// cloneFile makes dst share the blocks of src, which APFS can do.
func cloneFile(dst, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrUnsupported
	}
	err = unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.ENOSYS):
		return ErrUnsupported
	default:
		return &os.PathError{Op: "clonefile", Path: dst, Err: err}
	}
}
