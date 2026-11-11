//go:build !light && !no_nbd

package nbd

import (
	"errors"
	"io/fs"
	"log/slog"
	"math/bits"
	"os"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
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

// fileImage is an image on disk. The calls below the reads and the writes
// are traced: the hole punched, the range zeroed, the extents asked for and
// the sync, each of which is a call into the filesystem of its own.
type fileImage struct {
	*os.File
	log *slog.Logger
}

func (f fileImage) PunchHole(off, n int64) error {
	err := sparse.PunchHole(f.File, off, n)
	logging.Syscall(f.log, "punch hole", err, slog.Int64("offset", off), slog.Int64("length", n))
	return err
}

func (f fileImage) ZeroRange(off, n int64) error {
	err := sparse.ZeroRange(f.File, off, n)
	logging.Syscall(f.log, "zero range", err, slog.Int64("offset", off), slog.Int64("length", n))
	return err
}

func (f fileImage) Extents(off, n int64) ([]sparse.Extent, error) {
	extents, err := sparse.Extents(f.File, off, n)
	logging.Syscall(f.log, "seek data", err, slog.Int64("offset", off), slog.Int64("length", n),
		slog.Int("extents", len(extents)))
	return extents, err
}

func (f fileImage) Sync() error {
	started := time.Now()
	err := f.File.Sync()
	logging.Syscall(f.log, "fsync", err, slog.Duration("took", time.Since(started).Round(time.Millisecond)))
	return err
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
