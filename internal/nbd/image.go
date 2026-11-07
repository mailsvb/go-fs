//go:build !light && !no_nbd

package nbd

import (
	"errors"
	"io/fs"
	"math/bits"
	"os"
	"sync"

	"github.com/mailsvb/go-fs/internal/sparse"
)

// image is what a session reads and writes: the attached file, or, in a
// test, something that fails the way a full disk does.
type image interface {
	ReadAt(b []byte, off int64) (int, error)
	WriteAt(b []byte, off int64) (int, error)
	Sync() error
	Close() error
	PunchHole(off, n int64) error
	ZeroRange(off, n int64) error
	Extents(off, n int64) ([]sparse.Extent, error)
}

// fileImage is an image on disk.
type fileImage struct{ *os.File }

func (f fileImage) PunchHole(off, n int64) error { return sparse.PunchHole(f.File, off, n) }
func (f fileImage) ZeroRange(off, n int64) error { return sparse.ZeroRange(f.File, off, n) }
func (f fileImage) Extents(off, n int64) ([]sparse.Extent, error) {
	return sparse.Extents(f.File, off, n)
}

// errno is the NBD error a failed request reports. A full filesystem is told
// apart from every other failure, since it is what lets QEMU pause the VM
// (werror=stop) rather than hand its guest an I/O error.
func errno(err error) uint32 {
	switch {
	case err == nil:
		return 0
	case diskFull(err):
		return errNoSpace
	case errors.Is(err, fs.ErrPermission):
		return errPerm
	default:
		return errIO
	}
}

// buffers are the buffers of requests, by size class: a power of two from
// 4 KiB to maxBlock, so that the steady stream of a VM's requests allocates
// nothing.
var buffers [sizeClasses]sync.Pool

// sizeClasses are the powers of two from 4 KiB (2^12) to maxBlock (2^25).
const sizeClasses = 14

func sizeClass(n int) int {
	if n <= 4096 {
		return 0
	}
	return bits.Len(uint(n-1)) - 12
}

func getBuffer(n int) []byte {
	class := sizeClass(n)
	if p, ok := buffers[class].Get().(*[]byte); ok {
		return (*p)[:n]
	}
	return make([]byte, n, 4096<<class)
}

func putBuffer(b []byte) {
	if cap(b) < 4096 || cap(b) > maxBlock || cap(b)&(cap(b)-1) != 0 {
		return
	}
	b = b[:cap(b)]
	buffers[sizeClass(cap(b))].Put(&b)
}

// budget bounds the bytes a connection has in flight.
type budget struct {
	mu     sync.Mutex
	cond   *sync.Cond
	free   int64
	total  int64
	closed bool
}

func newBudget(total int64) *budget {
	b := &budget{free: total, total: total}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire waits until n bytes are free, and reports false when the budget
// was closed meanwhile.
func (b *budget) acquire(n int64) bool {
	n = min(n, b.total)
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.free < n && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return false
	}
	b.free -= n
	return true
}

func (b *budget) release(n int64) {
	n = min(n, b.total)
	b.mu.Lock()
	b.free += n
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *budget) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}
