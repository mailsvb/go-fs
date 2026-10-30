package httpd

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// What is deleted over the http server, by the listing, by a WebDAV client or
// by S3, is moved to general.baseFolderTrash rather than removed, at the path
// it had in the served folder, so that an administrator can restore it from
// the Trash page. FTP, SFTP and TFTP delete for good, as they always did. With
// the folder empty there is no trash, and the http server deletes for good as
// well.
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
// trashMu, which is also what keeps the index whole.

// trashIndexName is the index in the trash folder. A file of this name at the
// top of the served folder would take its place, so that one file is deleted
// for good rather than moved.
const trashIndexName = ".go-fs-trash.json"

// trashIndex is when each path that was deleted was deleted.
type trashIndex map[string]time.Time

// trashOn reports whether deleting moves to the trash.
func trashOn(set *settings) bool {
	return set.cfg.TrashFolder != ""
}

// trashPath is where a path of the served folder is kept in the trash.
func trashPath(folder, virtual string) string {
	return filepath.Join(folder, filepath.FromSlash(virtual))
}

// isTrashIndex reports whether virtual names the index rather than something
// that was deleted.
func isTrashIndex(virtual string) bool {
	return virtual == "/"+trashIndexName
}

// readTrashIndex reads the index. One that is missing or cannot be read is
// empty: what it would have said falls back to the modification times.
func readTrashIndex(folder string) trashIndex {
	index := trashIndex{}
	body, err := os.ReadFile(filepath.Join(folder, trashIndexName))
	if err != nil {
		return index
	}
	_ = json.Unmarshal(body, &index)
	return index
}

// writeTrashIndex replaces the index in one rename, so that a crash leaves the
// old one or the new one and never half of either.
func writeTrashIndex(folder string, index trashIndex) error {
	body, err := json.Marshal(index)
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp(folder, trashIndexName+".*")
	if err != nil {
		return err
	}
	if _, err := staged.Write(body); err != nil {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
		return err
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(staged.Name())
		return err
	}
	if err := os.Rename(staged.Name(), filepath.Join(folder, trashIndexName)); err != nil {
		_ = os.Remove(staged.Name())
		return err
	}
	return nil
}

// under reports whether virtual is at or below folder.
func under(virtual, folder string) bool {
	return virtual == folder || folder == "/" || strings.HasPrefix(virtual, folder+"/")
}

// drop forgets virtual and everything below it.
func (index trashIndex) drop(virtual string) {
	for key := range index {
		if under(key, virtual) {
			delete(index, key)
		}
	}
}

// hasBelow reports whether a path below virtual has an entry of its own.
func (index trashIndex) hasBelow(virtual string) bool {
	for key := range index {
		if key != virtual && under(key, virtual) {
			return true
		}
	}
	return false
}

// deleted is when virtual was deleted: its own entry or the deepest one above
// it, and the zero time when there is none.
func (index trashIndex) deleted(virtual string) time.Time {
	for at := virtual; ; at = path.Dir(at) {
		if when, ok := index[at]; ok {
			return when
		}
		if at == "/" {
			return time.Time{}
		}
	}
}

// discard removes what target names, with everything in it when all is set:
// into the trash while there is one, for good otherwise. It reports which.
func (s *Server) discard(set *settings, target vfs.Target, all bool) (trashed bool, err error) {
	if !trashOn(set) || isTrashIndex(target.Virtual) {
		if all {
			return false, os.RemoveAll(target.Path)
		}
		return false, os.Remove(target.Path)
	}
	return true, s.moveToTrash(set, target)
}

// moveToTrash moves target into the trash, replacing what the trash held for
// its path.
func (s *Server) moveToTrash(set *settings, target vfs.Target) error {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	folder := set.cfg.TrashFolder
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	index := readTrashIndex(folder)
	if err := trashFolders(folder, path.Dir(target.Virtual), index); err != nil {
		return err
	}
	if err := placeInTrash(target.Path, trashPath(folder, target.Virtual), target.Virtual, index); err != nil {
		return err
	}
	index[target.Virtual] = time.Now()
	return writeTrashIndex(folder, index)
}

// trashFolders makes the folders above a path in the trash. One of them that
// is a file is from an earlier delete, and the newer delete replaces it.
func trashFolders(folder, virtual string, index trashIndex) error {
	walked := "/"
	for _, part := range strings.Split(strings.Trim(virtual, "/"), "/") {
		if part == "" {
			continue
		}
		walked = path.Join(walked, part)
		at := trashPath(folder, walked)
		info, err := os.Lstat(at)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			if err := os.RemoveAll(at); err != nil {
				return err
			}
			index.drop(walked)
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.Mkdir(at, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// placeInTrash moves src to dst, which is its place in the trash. A folder
// that is there already is merged with it; anything else that is there is
// replaced.
func placeInTrash(src, dst, virtual string, index trashIndex) error {
	from, err := os.Lstat(src)
	if err != nil {
		return err
	}
	there, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		index.drop(virtual)
		return moveTree(src, dst)
	}
	if err != nil {
		return err
	}
	if !from.IsDir() || !there.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		index.drop(virtual)
		return moveTree(src, dst)
	}

	incoming, err := os.ReadDir(src)
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
	kept, err := os.ReadDir(dst)
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
		if when := index.deleted(child); !when.IsZero() {
			index[child] = when
		}
	}
	for _, item := range incoming {
		if err := placeInTrash(filepath.Join(src, item.Name()), filepath.Join(dst, item.Name()),
			path.Join(virtual, item.Name()), index); err != nil {
			return err
		}
	}
	_ = os.Chtimes(dst, from.ModTime(), from.ModTime())
	return os.Remove(src)
}

// moveTree moves a file or a folder. Across filesystems, where a rename
// fails, it is copied and the original removed.
func moveTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

// copyTree copies a file, a link or a folder with everything in it, keeping
// the modes and the modification times.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(link, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		items, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := copyTree(filepath.Join(src, item.Name()), filepath.Join(dst, item.Name())); err != nil {
				return err
			}
		}
	case info.Mode().IsRegular():
		if err := copyFile(src, dst, false); err != nil {
			return err
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
	default:
		return &os.PathError{Op: "copy", Path: src, Err: errors.ErrUnsupported}
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// errNotInTrash is a path the trash holds nothing at.
var errNotInTrash = errors.New("not in the trash")

// trashItem resolves a path of the trash to the folder, and refuses one that
// is not there or is the index.
func trashItem(folder, virtual string) (string, error) {
	if virtual == "/" || isTrashIndex(virtual) {
		return "", errNotInTrash
	}
	at := trashPath(folder, virtual)
	if _, err := os.Lstat(at); err != nil {
		return "", errNotInTrash
	}
	return at, nil
}

// restoreFromTrash moves what the trash holds at virtual back to that path of
// the served folder, making the folders above it that are gone. A folder is
// merged into the one that is there. What is in the way is replaced with
// overwrite and stays in the trash without it, which is what skipped counts.
func (s *Server) restoreFromTrash(set *settings, virtual string, overwrite bool) (restored, skipped int, err error) {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	folder := set.cfg.TrashFolder
	src, err := trashItem(folder, virtual)
	if err != nil {
		return 0, 0, err
	}
	target := s.root.Resolve("/", virtual)
	if !target.Valid || target.IsRoot() {
		return 0, 0, errNotInTrash
	}
	index := readTrashIndex(folder)
	defer func() {
		pruneTrash(folder, virtual, index)
		if werr := writeTrashIndex(folder, index); err == nil {
			err = werr
		}
	}()

	if ok, err := s.liveFolders(path.Dir(virtual), overwrite); err != nil {
		return 0, 0, err
	} else if !ok {
		return 0, countItems(src), nil
	}
	restored, skipped, err = restoreTree(src, target.Path, virtual, overwrite, index)
	return restored, skipped, err
}

// liveFolders makes the folders above a path of the served folder. One of them
// that is a file is in the way: it is replaced with overwrite, and without it
// nothing below can be restored.
func (s *Server) liveFolders(virtual string, overwrite bool) (bool, error) {
	walked := "/"
	for _, part := range strings.Split(strings.Trim(virtual, "/"), "/") {
		if part == "" {
			continue
		}
		walked = path.Join(walked, part)
		target := s.root.Resolve("/", walked)
		if !target.Valid {
			return false, errNotInTrash
		}
		info, err := os.Stat(target.Path)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			if !overwrite {
				return false, nil
			}
			if err := os.Remove(target.Path); err != nil {
				return false, err
			}
		case !errors.Is(err, os.ErrNotExist):
			return false, err
		}
		if err := os.Mkdir(target.Path, 0o755); err != nil {
			return false, err
		}
	}
	return true, nil
}

// restoreTree moves src back to dst, folder by folder where both are one.
func restoreTree(src, dst, virtual string, overwrite bool, index trashIndex) (restored, skipped int, err error) {
	from, err := os.Lstat(src)
	if err != nil {
		return 0, 0, err
	}
	there, err := os.Lstat(dst)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return 0, 0, err
	case from.IsDir() && there.IsDir():
		items, err := os.ReadDir(src)
		if err != nil {
			return 0, 0, err
		}
		for _, item := range items {
			done, left, err := restoreTree(filepath.Join(src, item.Name()), filepath.Join(dst, item.Name()),
				path.Join(virtual, item.Name()), overwrite, index)
			restored, skipped = restored+done, skipped+left
			if err != nil {
				return restored, skipped, err
			}
		}
		if len(items) == 0 {
			// an empty folder whose place is taken by a folder is restored
			// by being there
			restored++
		}
		if os.Remove(src) == nil {
			index.drop(virtual)
		}
		return restored, skipped, nil
	case !overwrite:
		return 0, countItems(src), nil
	default:
		if err := os.RemoveAll(dst); err != nil {
			return 0, 0, err
		}
	}
	count := countItems(src)
	if err := moveTree(src, dst); err != nil {
		return 0, 0, err
	}
	index.drop(virtual)
	return count, 0, nil
}

// countItems counts the files below a path, and an empty folder as one.
func countItems(at string) int {
	items, err := os.ReadDir(at)
	if err != nil {
		return 1
	}
	if len(items) == 0 {
		return 1
	}
	count := 0
	for _, item := range items {
		count += countItems(filepath.Join(at, item.Name()))
	}
	return count
}

// pruneTrash removes the folders of the trash that are left empty at and
// above virtual, and the entries of what is not there any more.
func pruneTrash(folder, virtual string, index trashIndex) {
	for at := virtual; at != "/" && at != "."; at = path.Dir(at) {
		items, err := os.ReadDir(trashPath(folder, at))
		if err == nil && len(items) > 0 {
			break
		}
		if err == nil && os.Remove(trashPath(folder, at)) != nil {
			break
		}
		index.drop(at)
	}
	for key := range index {
		if _, err := os.Lstat(trashPath(folder, key)); errors.Is(err, os.ErrNotExist) {
			delete(index, key)
		}
	}
}

// deleteFromTrash deletes what the trash holds at virtual for good.
func (s *Server) deleteFromTrash(set *settings, virtual string) error {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	folder := set.cfg.TrashFolder
	at, err := trashItem(folder, virtual)
	if err != nil {
		return err
	}
	index := readTrashIndex(folder)
	if err := os.RemoveAll(at); err != nil {
		return err
	}
	index.drop(virtual)
	pruneTrash(folder, path.Dir(virtual), index)
	return writeTrashIndex(folder, index)
}

// emptyTrash deletes everything in the trash for good, and reports how many
// files and empty folders that was.
func (s *Server) emptyTrash(set *settings) (int, error) {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	folder := set.cfg.TrashFolder
	items, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		at := filepath.Join(folder, item.Name())
		if item.Name() != trashIndexName {
			count += countItems(at)
		}
		if err := os.RemoveAll(at); err != nil {
			return count, err
		}
	}
	return count, nil
}

// purgeTrash deletes for good what has been in the trash for longer than
// general.trashRetentionDays. It is part of the hourly sweep.
func (s *Server) purgeTrash() {
	set := s.settings()
	if !trashOn(set) || set.cfg.TrashRetentionDays <= 0 {
		return
	}
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	folder := set.cfg.TrashFolder
	items, err := os.ReadDir(folder)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Debug("http cleanup cannot read the trash", "path", folder, "error", err)
		}
		return
	}
	index := readTrashIndex(folder)
	before := len(index)
	cutoff := time.Now().Add(-time.Duration(set.cfg.TrashRetentionDays) * 24 * time.Hour)
	purged := 0
	for _, item := range items {
		if item.Name() == trashIndexName || strings.HasPrefix(item.Name(), trashIndexName+".") {
			continue
		}
		s.purgeTree(folder, "/"+item.Name(), time.Time{}, cutoff, index, &purged)
	}
	if purged > 0 || len(index) != before {
		if err := writeTrashIndex(folder, index); err != nil {
			s.log.Error("http cleanup cannot write the trash index", "path", folder, "error", err)
		}
	}
}

// purgeTree removes what at virtual was deleted before cutoff, and reports
// whether nothing of it is left. inherited is when the folder above was
// deleted, zero for one that never was on its own.
func (s *Server) purgeTree(folder, virtual string, inherited, cutoff time.Time, index trashIndex, purged *int) bool {
	at := trashPath(folder, virtual)
	info, err := os.Lstat(at)
	if err != nil {
		index.drop(virtual)
		return true
	}
	when := inherited
	if own, ok := index[virtual]; ok {
		when = own
	}
	// a file, or a folder deleted as one, goes as a whole; a folder that
	// holds what was deleted later, or that is only there for what is in it,
	// is decided entry by entry
	if !info.IsDir() || (!when.IsZero() && !index.hasBelow(virtual)) {
		if when.IsZero() {
			when = info.ModTime()
		}
		if !when.Before(cutoff) {
			return false
		}
		if err := os.RemoveAll(at); err != nil {
			s.log.Error("trash purge failed", "path", virtual, "error", err)
			return false
		}
		index.drop(virtual)
		*purged++
		s.log.Info("trash purge", "path", virtual, "folder", info.IsDir(), "deleted", when.Format(time.RFC3339))
		return true
	}
	items, err := os.ReadDir(at)
	if err != nil {
		return false
	}
	left := false
	for _, item := range items {
		if !s.purgeTree(folder, path.Join(virtual, item.Name()), when, cutoff, index, purged) {
			left = true
		}
	}
	if left || (!when.IsZero() && !when.Before(cutoff)) {
		return false
	}
	if os.Remove(at) != nil {
		return false
	}
	index.drop(virtual)
	return true
}
