// Package disks keeps the disk images of the Disks page: the folder
// general.baseFolderDisks, the images directly in it, and who has which of
// them in use. The NBD server serves the images to the VMs that run on them,
// the Disks page makes, resizes, renames, clones, uploads and downloads them,
// and both run in one process: this package is where they meet, so that a
// disk is never resized under a VM, and never attached read-write by two.
//
// An image is a sparse raw file, named with letters, digits, dots, dashes
// and underscores. A file whose name starts with a dot is work in progress,
// an upload or a clone, and is neither listed nor served.
package disks

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/sparse"
	"github.com/mailsvb/go-fs/internal/vfs"
)

// The errors a caller tells apart. A refused lock is a *BusyError, which
// errors.Is matches against ErrBusy.
var (
	ErrName           = errors.New("is not a valid disk name: use letters, digits, dots, dashes and underscores, starting with a letter or a digit")
	ErrNotFound       = fs.ErrNotExist
	ErrExists   error = taken{}
	ErrBusy           = errors.New("is in use")
	ErrShrink         = errors.New("is larger than the size asked for, and shrinking cuts off what the guest wrote there")
	ErrSize           = errors.New("is not a size go-fs makes a disk of")
)

// taken is the error of a name another disk has, which errors.Is also
// matches against fs.ErrExist.
type taken struct{}

func (taken) Error() string        { return "is the name of another disk" }
func (taken) Is(target error) bool { return target == fs.ErrExist }

// MaxNameLength is the longest name a disk may have.
const MaxNameLength = 200

// MaxSize is the largest disk the Disks page makes, 1 PiB: far beyond what a
// filesystem holds in one file today, and still far from what overflows.
const MaxSize = 1 << 50

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// reservedNames are what Windows takes for a device whatever the extension,
// so that a name valid on one platform is valid on every one.
var reservedNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

// ValidName reports whether name may be the name of a disk.
func ValidName(name string) error {
	if len(name) > MaxNameLength || !namePattern.MatchString(name) || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%q %w", name, ErrName)
	}
	stem := strings.ToLower(name)
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	if reservedNames[stem] || (len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) &&
		stem[3] >= '0' && stem[3] <= '9') {
		return fmt.Errorf("%q %w", name, ErrName)
	}
	return nil
}

// ParseSize reads a size as the Disks page takes it: a number of bytes, or a
// number with K, M, G, T or P for powers of 1024, with or without the B or
// iB that follows, such as 100G, 1.5T or 512MiB. It is rounded up to a whole
// 512-byte sector, which is what a guest addresses a disk in.
func ParseSize(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	upper := strings.ToUpper(trimmed)
	upper = strings.TrimSuffix(upper, "IB")
	upper = strings.TrimSuffix(upper, "B")
	multiplier := 1.0
	if n := len(upper); n > 0 {
		if at := strings.IndexByte("KMGTP", upper[n-1]); at >= 0 {
			multiplier = float64(int64(1) << (10 * (at + 1)))
			upper = upper[:n-1]
		}
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(upper), 64)
	if err != nil || number <= 0 || number*multiplier > MaxSize {
		return 0, fmt.Errorf("%q %w; give it as, say, 100G", trimmed, ErrSize)
	}
	return roundUp(int64(number * multiplier)), nil
}

func roundUp(size int64) int64 {
	return (size + 511) / 512 * 512
}

// Folder is the folder of the disk images.
type Folder struct {
	base  string
	folds bool
	reg   *registry
	// log takes the trace of what is done to the images on disk
	log *slog.Logger
}

// folders hands every caller that opens one folder the same registry, which
// is what lets the NBD server and the Disks page see each other's locks.
var (
	foldersMu sync.Mutex
	folders   = map[string]*registry{}
)

// Open opens the folder of the disk images, which has to exist. log takes
// the trace of the files touched, nil for none.
func Open(folder string, log *slog.Logger) (*Folder, error) {
	root, err := vfs.New(folder)
	if err != nil {
		return nil, err
	}
	base := root.Base()
	foldersMu.Lock()
	defer foldersMu.Unlock()
	reg := folders[base]
	if reg == nil {
		reg = newRegistry()
		folders[base] = reg
	}
	return &Folder{base: base, folds: root.CaseInsensitive(), reg: reg, log: logging.OrDiscard(log)}, nil
}

// Base is the folder on disk.
func (f *Folder) Base() string { return f.base }

// key is what two spellings of one name have in common on this folder's
// filesystem. Names are ASCII, so lowering it is all folding takes.
func (f *Folder) key(name string) string {
	if f.folds {
		return strings.ToLower(name)
	}
	return name
}

// path is where the disk name is. The name has been validated.
func (f *Folder) path(name string) string { return filepath.Join(f.base, name) }

// stagingPath is where work on the disk name is kept until it is done.
func (f *Folder) stagingPath(kind, name string) string {
	return filepath.Join(f.base, "."+kind+"-"+name)
}

// Disk is what List reports of one image.
type Disk struct {
	Name      string
	Size      int64
	Allocated int64
	Modified  time.Time
	// Sessions are the NBD connections that have it attached.
	Sessions []*Session
	// Busy names what holds it exclusively right now, a resize or a clone
	// into it; empty when nothing does.
	Busy string
	// Holds counts the downloads and clones reading it.
	Holds int
}

// List reports the images in the folder, by name.
func (f *Folder) List() ([]Disk, error) {
	entries, err := logging.ReadDir(f.log, f.base)
	if err != nil {
		return nil, err
	}
	var disks []Disk
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !entry.Type().IsRegular() || ValidName(name) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		allocated, err := sparse.Allocated(f.path(name))
		logging.Syscall(f.log, "allocated", err, slog.String("path", f.path(name)), slog.Int64("allocated", allocated))
		if err != nil {
			allocated = info.Size()
		}
		disk := Disk{Name: name, Size: info.Size(), Allocated: allocated, Modified: info.ModTime()}
		disk.Sessions, disk.Busy, disk.Holds = f.reg.state(f.key(name))
		disks = append(disks, disk)
	}
	sort.Slice(disks, func(i, j int) bool { return strings.ToLower(disks[i].Name) < strings.ToLower(disks[j].Name) })
	return disks, nil
}

// Stat reports the image name.
func (f *Folder) Stat(name string) (os.FileInfo, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	info, err := logging.Lstat(f.log, f.path(name))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file: %w", name, ErrNotFound)
	}
	return info, nil
}

// Create makes the image name of size bytes, as a sparse file that takes no
// space until a guest writes to it.
func (f *Folder) Create(name string, size int64) (err error) {
	if err := ValidName(name); err != nil {
		return err
	}
	if size <= 0 || size > MaxSize {
		return fmt.Errorf("%d bytes %w", size, ErrSize)
	}
	lease, err := f.reg.acquire(f.key(name), name, Exclusive, nil, "being created")
	if err != nil {
		return err
	}
	defer lease.Release()
	file, err := logging.OpenFile(f.log, f.path(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%q %w", name, ErrExists)
	}
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = logging.Remove(f.log, f.path(name))
		}
	}()
	if err := f.setSparse(file); err != nil {
		return err
	}
	if err := file.Truncate(roundUp(size)); err != nil {
		return err
	}
	return file.Sync()
}

// Resize changes the size of the image name, which nothing may have in use.
// Making it smaller is refused unless shrink says to: it cuts off whatever
// the guest wrote at the end of the disk, which may be a partition.
func (f *Folder) Resize(name string, size int64, shrink bool) (err error) {
	if _, err := f.Stat(name); err != nil {
		return err
	}
	if size <= 0 || size > MaxSize {
		return fmt.Errorf("%d bytes %w", size, ErrSize)
	}
	size = roundUp(size)
	lease, err := f.reg.acquire(f.key(name), name, Exclusive, nil, "being resized")
	if err != nil {
		return err
	}
	defer lease.Release()
	file, err := logging.OpenFile(f.log, f.path(name), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if size < info.Size() && !shrink {
		return fmt.Errorf("%q %w", name, ErrShrink)
	}
	if err := f.setSparse(file); err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		return err
	}
	return file.Sync()
}

// Rename gives the image from the name to, which nothing may have in use and
// which must not be taken. A change of case alone is a rename too, on a
// filesystem that ignores case.
func (f *Folder) Rename(from, to string) error {
	if _, err := f.Stat(from); err != nil {
		return err
	}
	if err := ValidName(to); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	leases, err := f.exclusiveBoth(from, to, "being renamed")
	if err != nil {
		return err
	}
	defer leases.Release()
	if f.key(from) != f.key(to) {
		if _, err := logging.Lstat(f.log, f.path(to)); err == nil {
			return fmt.Errorf("%q %w", to, ErrExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return logging.Rename(f.log, f.path(from), f.path(to))
}

// Delete removes the image name for good, which nothing may have in use.
func (f *Folder) Delete(name string) error {
	if _, err := f.Stat(name); err != nil {
		return err
	}
	lease, err := f.reg.acquire(f.key(name), name, Exclusive, nil, "being deleted")
	if err != nil {
		return err
	}
	defer lease.Release()
	return logging.Remove(f.log, f.path(name))
}

// Attach opens the image name for an NBD session, read-write or read-only
// as session says, and takes the lock that goes with it: a second writer is
// refused, unless it is the same client back after losing its connection, in
// which case the connection it lost is closed and it takes over. The lease
// has to be released when the session ends.
func (f *Folder) Attach(name string, session *Session) (*os.File, *Lease, error) {
	if _, err := f.Stat(name); err != nil {
		return nil, nil, err
	}
	mode := Reader
	if session.Writable {
		mode = Writer
	}
	session.Disk = name
	lease, err := f.reg.acquire(f.key(name), name, mode, session, "")
	if err != nil {
		return nil, nil, err
	}
	flag := os.O_RDONLY
	if session.Writable {
		flag = os.O_RDWR
	}
	file, err := logging.OpenFile(f.log, f.path(name), flag, 0)
	if err != nil {
		lease.Release()
		return nil, nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		lease.Release()
		if err == nil {
			err = fmt.Errorf("%q is not a regular file: %w", name, ErrNotFound)
		}
		return nil, nil, err
	}
	if session.Writable {
		// an image made elsewhere is marked so that a trim gives space back
		if err := f.setSparse(file); err != nil {
			_ = file.Close()
			lease.Release()
			return nil, nil, err
		}
	}
	return file, lease, nil
}

// OpenRead opens the image name to be read whole, for a download: nothing
// may write to it meanwhile, so it is refused while the image is attached
// read-write, and an attempt to attach it read-write is refused until the
// lease is released.
func (f *Folder) OpenRead(name string) (*os.File, *Lease, error) {
	if _, err := f.Stat(name); err != nil {
		return nil, nil, err
	}
	lease, err := f.reg.acquire(f.key(name), name, Hold, nil, "")
	if err != nil {
		return nil, nil, err
	}
	file, err := logging.Open(f.log, f.path(name))
	if err != nil {
		lease.Release()
		return nil, nil, err
	}
	return file, lease, nil
}

// Sessions are the NBD sessions of the folder, in the order they began.
func (f *Folder) Sessions() []*Session { return f.reg.sessions() }

// Disconnect closes the NBD session id, reporting whether there was one
// still open: a session closed a moment ago is listed until its connection
// lets go of the image, and is not closed twice.
func (f *Folder) Disconnect(id string) bool {
	for _, session := range f.reg.sessions() {
		if session.ID == id {
			return session.close("it was disconnected on the Disks page")
		}
	}
	return false
}

// exclusiveBoth takes the exclusive lock of two names, or of one when they
// are one name on this filesystem.
func (f *Folder) exclusiveBoth(a, b, why string) (Leases, error) {
	first, err := f.reg.acquire(f.key(a), a, Exclusive, nil, why)
	if err != nil {
		return nil, err
	}
	if f.key(a) == f.key(b) {
		return Leases{first}, nil
	}
	second, err := f.reg.acquire(f.key(b), b, Exclusive, nil, why)
	if err != nil {
		first.Release()
		return nil, err
	}
	return Leases{first, second}, nil
}

// Sweep removes what uploads and clones left behind that has not been
// touched for maxAge, and that no upload or clone of this process is still
// writing.
func (f *Folder) Sweep(maxAge time.Duration) (removed []string) {
	entries, err := logging.ReadDir(f.log, f.base)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		kind, disk, ok := stagingName(name)
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		if f.reg.working(kind, f.key(disk)) {
			continue
		}
		if logging.Remove(f.log, filepath.Join(f.base, name)) == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// stagingName reads the name of a file work is kept in.
func stagingName(name string) (kind, disk string, ok bool) {
	for _, kind := range []string{"upload", "clone"} {
		if rest, found := strings.CutPrefix(name, "."+kind+"-"); found && ValidName(rest) == nil {
			return kind, rest, true
		}
	}
	return "", "", false
}

// setSparse marks file as sparse where the filesystem has to be told, with
// the call traced.
func (f *Folder) setSparse(file *os.File) error {
	err := sparse.SetSparse(file)
	logging.Syscall(f.log, "set sparse", err, slog.String("path", file.Name()))
	return err
}
