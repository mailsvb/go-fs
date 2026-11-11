// Package trash keeps general.baseFolderTrash: what is deleted over the http
// server, or by a sync, is moved there rather than removed, at the path it had
// in the served folder, so that an administrator can restore it from the
// Trash page.
//
// Deleting a path again replaces what the trash held for it: a file replaces
// a file, and a folder is merged into the folder that is there, its files
// replacing the ones of the same name.
//
// A file keeps its own modification time in the trash, so when it was deleted
// is kept beside it, in an index at the top of the trash: the deepest entry
// at or above a path is when that path was deleted. Something in the trash
// that no entry covers, which is only what was put there by hand, counts from
// its modification time instead. Every change to the trash is made under
// Lock, which is also what keeps the index whole; it is one for the whole
// process, since the http server and the syncs move into the same trash.
package trash

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
)

// mu serializes every change to the trash and its index.
var mu sync.Mutex

// Lock takes the trash, for a change to it or a read of its index that has
// to see it whole. Move takes it itself.
func Lock() { mu.Lock() }

// Unlock gives the trash back.
func Unlock() { mu.Unlock() }

// IndexName is the index in the trash folder. A file of this name at the
// top of the served folder would take its place, so that one file is deleted
// for good rather than moved.
const IndexName = ".go-fs-trash.json"

// Index is when each path that was deleted was deleted.
type Index map[string]time.Time

// Path is where a path of the served folder is kept in the trash.
func Path(folder, virtual string) string {
	return filepath.Join(folder, filepath.FromSlash(virtual))
}

// IsIndex reports whether virtual names the index rather than something
// that was deleted.
func IsIndex(virtual string) bool {
	return virtual == "/"+IndexName
}

// ReadIndex reads the index. One that is missing or cannot be read is
// empty: what it would have said falls back to the modification times.
func ReadIndex(log *slog.Logger, folder string) Index {
	index := Index{}
	body, err := logging.ReadFile(log, filepath.Join(folder, IndexName))
	if err != nil {
		return index
	}
	_ = json.Unmarshal(body, &index)
	return index
}

// WriteIndex replaces the index in one rename, so that a crash leaves the
// old one or the new one and never half of either.
func WriteIndex(log *slog.Logger, folder string, index Index) error {
	body, err := json.Marshal(index)
	if err != nil {
		return err
	}
	staged, err := logging.CreateTemp(log, folder, IndexName+".*")
	if err != nil {
		return err
	}
	if _, err := staged.Write(body); err != nil {
		_ = staged.Close()
		_ = logging.Remove(log, staged.Name())
		return err
	}
	if err := staged.Close(); err != nil {
		_ = logging.Remove(log, staged.Name())
		return err
	}
	if err := logging.Rename(log, staged.Name(), filepath.Join(folder, IndexName)); err != nil {
		_ = logging.Remove(log, staged.Name())
		return err
	}
	return nil
}

// under reports whether virtual is at or below folder.
func under(virtual, folder string) bool {
	return virtual == folder || folder == "/" || strings.HasPrefix(virtual, folder+"/")
}

// Drop forgets virtual and everything below it.
func (index Index) Drop(virtual string) {
	for key := range index {
		if under(key, virtual) {
			delete(index, key)
		}
	}
}

// HasBelow reports whether a path below virtual has an entry of its own.
func (index Index) HasBelow(virtual string) bool {
	for key := range index {
		if key != virtual && under(key, virtual) {
			return true
		}
	}
	return false
}

// Deleted is when virtual was deleted: its own entry or the deepest one above
// it, and the zero time when there is none.
func (index Index) Deleted(virtual string) time.Time {
	for at := virtual; ; at = path.Dir(at) {
		if when, ok := index[at]; ok {
			return when
		}
		if at == "/" {
			return time.Time{}
		}
	}
}

// Move moves src, which is at virtual in the served folder, into the trash
// in folder, replacing what the trash held for its path.
func Move(log *slog.Logger, folder, src, virtual string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := logging.MkdirAll(log, folder, 0o700); err != nil {
		return err
	}
	index := ReadIndex(log, folder)
	if err := trashFolders(log, folder, path.Dir(virtual), index); err != nil {
		return err
	}
	if err := placeInTrash(log, src, Path(folder, virtual), virtual, index); err != nil {
		return err
	}
	index[virtual] = time.Now()
	return WriteIndex(log, folder, index)
}

// trashFolders makes the folders above a path in the trash. One of them that
// is a file is from an earlier delete, and the newer delete replaces it.
func trashFolders(log *slog.Logger, folder, virtual string, index Index) error {
	walked := "/"
	for _, part := range strings.Split(strings.Trim(virtual, "/"), "/") {
		if part == "" {
			continue
		}
		walked = path.Join(walked, part)
		at := Path(folder, walked)
		info, err := logging.Lstat(log, at)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			if err := logging.RemoveAll(log, at); err != nil {
				return err
			}
			index.Drop(walked)
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := logging.Mkdir(log, at, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// placeInTrash moves src to dst, which is its place in the trash. A folder
// that is there already is merged with it; anything else that is there is
// replaced.
func placeInTrash(log *slog.Logger, src, dst, virtual string, index Index) error {
	from, err := logging.Lstat(log, src)
	if err != nil {
		return err
	}
	there, err := logging.Lstat(log, dst)
	if errors.Is(err, os.ErrNotExist) {
		index.Drop(virtual)
		return MoveTree(log, src, dst)
	}
	if err != nil {
		return err
	}
	if !from.IsDir() || !there.IsDir() {
		if err := logging.RemoveAll(log, dst); err != nil {
			return err
		}
		index.Drop(virtual)
		return MoveTree(log, src, dst)
	}

	incoming, err := logging.ReadDir(log, src)
	if err != nil {
		return err
	}
	coming := make(map[string]bool, len(incoming))
	for _, item := range incoming {
		coming[item.Name()] = true
	}
	// what the trash held and this delete leaves alone keeps the date it
	// was deleted on, rather than taking the one this delete gives the
	// folder
	kept, err := logging.ReadDir(log, dst)
	if err != nil {
		return err
	}
	for _, item := range kept {
		child := path.Join(virtual, item.Name())
		if coming[item.Name()] {
			continue
		}
		if _, own := index[child]; own {
			continue
		}
		if when := index.Deleted(child); !when.IsZero() {
			index[child] = when
		}
	}
	for _, item := range incoming {
		if err := placeInTrash(log, filepath.Join(src, item.Name()), filepath.Join(dst, item.Name()),
			path.Join(virtual, item.Name()), index); err != nil {
			return err
		}
	}
	_ = logging.Chtimes(log, dst, from.ModTime(), from.ModTime())
	return logging.Remove(log, src)
}

// MoveTree moves a file or a folder. Across filesystems, where a rename
// fails, it is copied and the original removed.
func MoveTree(log *slog.Logger, src, dst string) error {
	if err := logging.Rename(log, src, dst); err == nil {
		return nil
	}
	if err := copyTree(log, src, dst); err != nil {
		_ = logging.RemoveAll(log, dst)
		return err
	}
	return logging.RemoveAll(log, src)
}

// copyTree copies a file, a link or a folder with everything in it, keeping
// the modes and the modification times.
func copyTree(log *slog.Logger, src, dst string) error {
	info, err := logging.Lstat(log, src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := logging.Readlink(log, src)
		if err != nil {
			return err
		}
		return logging.Symlink(log, link, dst)
	case info.IsDir():
		if err := logging.Mkdir(log, dst, info.Mode().Perm()); err != nil {
			return err
		}
		items, err := logging.ReadDir(log, src)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := copyTree(log, filepath.Join(src, item.Name()), filepath.Join(dst, item.Name())); err != nil {
				return err
			}
		}
	case info.Mode().IsRegular():
		if err := copyFile(log, src, dst); err != nil {
			return err
		}
		if err := logging.Chmod(log, dst, info.Mode().Perm()); err != nil {
			return err
		}
	default:
		return &os.PathError{Op: "copy", Path: src, Err: errors.ErrUnsupported}
	}
	return logging.Chtimes(log, dst, info.ModTime(), info.ModTime())
}

// copyFile copies src onto dst, which must not exist yet.
func copyFile(log *slog.Logger, src, dst string) error {
	in, err := logging.Open(log, src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := logging.OpenFile(log, dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = logging.Remove(log, dst)
		return err
	}
	return out.Close()
}
