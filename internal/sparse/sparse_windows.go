package sparse

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HolePunching reports whether PunchHole can work on this platform at all.
// The filesystem may still refuse, which PunchHole reports as ErrUnsupported.
const HolePunching = true

// SetSparse marks a file as one that may have holes, which NTFS and ReFS
// need to be told before a range of zeros takes no space.
func SetSparse(f *os.File) error {
	err := ioctl(f, windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, nil)
	if errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		// FAT and the like: a file there simply has no holes
		return nil
	}
	return err
}

// fileZeroDataInformation is FILE_ZERO_DATA_INFORMATION.
type fileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

// PunchHole gives the space of n bytes at off back to the filesystem; the
// range reads as zeros afterwards and the size of the file stays. On a file
// that is not marked sparse the range is zeroed but stays allocated.
func PunchHole(f *os.File, off, n int64) error {
	if n <= 0 {
		return nil
	}
	in := fileZeroDataInformation{FileOffset: off, BeyondFinalZero: off + n}
	err := ioctl(f, windows.FSCTL_SET_ZERO_DATA, (*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)), nil, 0, nil)
	if errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return ErrUnsupported
	}
	return err
}

// ZeroRange makes n bytes at off read as zeros, keeping them allocated.
func ZeroRange(f *os.File, off, n int64) error { return WriteZeros(f, off, n) }

// fileAllocatedRangeBuffer is FILE_ALLOCATED_RANGE_BUFFER.
type fileAllocatedRangeBuffer struct {
	FileOffset int64
	Length     int64
}

// Extents reports which of the n bytes at off hold data and which are holes,
// by FSCTL_QUERY_ALLOCATED_RANGES. A file that is not sparse is all data.
func Extents(f *os.File, off, n int64) ([]Extent, error) {
	if n <= 0 {
		return nil, nil
	}
	end := off + n
	var extents []Extent
	ranges := make([]fileAllocatedRangeBuffer, 256)
	pos := off
	for pos < end {
		in := fileAllocatedRangeBuffer{FileOffset: pos, Length: end - pos}
		var returned uint32
		err := ioctl(f, windows.FSCTL_QUERY_ALLOCATED_RANGES,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			(*byte)(unsafe.Pointer(&ranges[0])), uint32(len(ranges))*uint32(unsafe.Sizeof(ranges[0])), &returned)
		more := errors.Is(err, windows.ERROR_MORE_DATA)
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			return []Extent{{Offset: off, Length: n}}, nil
		}
		if err != nil && !more {
			return nil, err
		}
		count := int(returned / uint32(unsafe.Sizeof(ranges[0])))
		for _, r := range ranges[:count] {
			start := max(r.FileOffset, pos)
			stop := min(r.FileOffset+r.Length, end)
			if start > pos {
				extents = append(extents, Extent{Offset: pos, Length: start - pos, Hole: true})
			}
			if stop > start {
				extents = append(extents, Extent{Offset: start, Length: stop - start})
				pos = stop
			}
		}
		if !more || count == 0 {
			break
		}
	}
	if pos < end {
		extents = append(extents, Extent{Offset: pos, Length: end - pos, Hole: true})
	}
	return merge(extents), nil
}

var procGetCompressedFileSizeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// Allocated is how many bytes of the filesystem the file at path takes.
func Allocated(path string) (int64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSizeW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != windows.ERROR_SUCCESS {
		return 0, &os.PathError{Op: "GetCompressedFileSize", Path: path, Err: callErr}
	}
	return int64(high)<<32 | int64(uint32(low)), nil
}

func ioctl(f *os.File, code uint32, in *byte, inSize uint32, out *byte, outSize uint32, returned *uint32) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		var ignored uint32
		if returned == nil {
			returned = &ignored
		}
		callErr = windows.DeviceIoControl(windows.Handle(fd), code, in, inSize, out, outSize, returned, nil)
	}); err != nil {
		return err
	}
	return callErr
}

// cloneFile is unsupported: ReFS block cloning takes more ceremony than a
// disk copy is worth.
func cloneFile(string, string) error { return ErrUnsupported }
