package sparse

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// HolePunching reports whether PunchHole can work on this platform at all.
// The filesystem may still refuse, which PunchHole reports as ErrUnsupported.
const HolePunching = true

// SetSparse marks a file as one that may have holes. Every Linux filesystem
// that can have holes has them without being asked.
func SetSparse(*os.File) error { return nil }

// PunchHole gives the space of n bytes at off back to the filesystem; the
// range reads as zeros afterwards and the size of the file stays.
func PunchHole(f *os.File, off, n int64) error {
	return fallocate(f, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
}

// ZeroRange makes n bytes at off read as zeros, keeping them allocated.
func ZeroRange(f *os.File, off, n int64) error {
	err := fallocate(f, unix.FALLOC_FL_ZERO_RANGE|unix.FALLOC_FL_KEEP_SIZE, off, n)
	if errors.Is(err, ErrUnsupported) {
		return WriteZeros(f, off, n)
	}
	return err
}

func fallocate(f *os.File, mode uint32, off, n int64) error {
	if n <= 0 {
		return nil
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		callErr = unix.Fallocate(int(fd), mode, off, n)
	}); err != nil {
		return err
	}
	if errors.Is(callErr, unix.EOPNOTSUPP) || errors.Is(callErr, unix.ENOSYS) {
		return ErrUnsupported
	}
	if callErr != nil {
		return &os.PathError{Op: "fallocate", Path: f.Name(), Err: callErr}
	}
	return nil
}

// cloneFile makes dst share the blocks of src, which btrfs and XFS can do.
func cloneFile(dst, src string) (err error) {
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
		return ErrUnsupported
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
	inRaw, err := in.SyscallConn()
	if err != nil {
		return err
	}
	outRaw, err := out.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	controlErr := inRaw.Control(func(srcFd uintptr) {
		if err := outRaw.Control(func(dstFd uintptr) {
			callErr = unix.IoctlFileClone(int(dstFd), int(srcFd))
		}); err != nil {
			callErr = err
		}
	})
	if controlErr != nil {
		return controlErr
	}
	switch {
	case callErr == nil:
		return nil
	case errors.Is(callErr, unix.EOPNOTSUPP), errors.Is(callErr, unix.EXDEV),
		errors.Is(callErr, unix.EINVAL), errors.Is(callErr, unix.ENOTTY), errors.Is(callErr, unix.ENOSYS):
		return ErrUnsupported
	default:
		return &os.PathError{Op: "clone", Path: dst, Err: callErr}
	}
}
