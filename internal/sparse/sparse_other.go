//go:build !linux && !darwin && !windows

package sparse

import "os"

// HolePunching reports whether PunchHole can work on this platform at all.
const HolePunching = false

// SetSparse marks a file as one that may have holes.
func SetSparse(*os.File) error { return nil }

// PunchHole is unsupported here.
func PunchHole(*os.File, int64, int64) error { return ErrUnsupported }

// ZeroRange makes n bytes at off read as zeros.
func ZeroRange(f *os.File, off, n int64) error { return WriteZeros(f, off, n) }

// Extents reports everything as data: there is no way to ask here.
func Extents(_ *os.File, off, n int64) ([]Extent, error) {
	if n <= 0 {
		return nil, nil
	}
	return []Extent{{Offset: off, Length: n}}, nil
}

// Allocated is how many bytes of the filesystem the file at path takes, as
// far as can be told here: its size.
func Allocated(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func cloneFile(string, string) error { return ErrUnsupported }
