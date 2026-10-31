package httpd

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/trash"
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
// its modification time instead. The trash itself, its index and moving into
// it, is internal/trash; every change to it is made under trash.Lock.

// trashOn reports whether deleting moves to the trash.
func trashOn(set *settings) bool {
	return set.cfg.TrashFolder != ""
}

// discard removes what target names, with everything in it when all is set:
// into the trash while there is one, for good otherwise. It reports which.
func (s *Server) discard(set *settings, target vfs.Target, all bool) (trashed bool, err error) {
	if !trashOn(set) || trash.IsIndex(target.Virtual) {
		if all {
			return false, os.RemoveAll(target.Path)
		}
		return false, os.Remove(target.Path)
	}
	return true, trash.Move(set.cfg.TrashFolder, target.Path, target.Virtual)
}

// errNotInTrash is a path the trash holds nothing at.
var errNotInTrash = errors.New("not in the trash")

// trashItem resolves a path of the trash to the folder, and refuses one that
// is not there or is the index.
func trashItem(folder, virtual string) (string, error) {
	if virtual == "/" || trash.IsIndex(virtual) {
		return "", errNotInTrash
	}
	at := trash.Path(folder, virtual)
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
	trash.Lock()
	defer trash.Unlock()
	folder := set.cfg.TrashFolder
	src, err := trashItem(folder, virtual)
	if err != nil {
		return 0, 0, err
	}
	target := s.root.Resolve("/", virtual)
	if !target.Valid || target.IsRoot() {
		return 0, 0, errNotInTrash
	}
	index := trash.ReadIndex(folder)
	defer func() {
		pruneTrash(folder, virtual, index)
		if werr := trash.WriteIndex(folder, index); err == nil {
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
func restoreTree(src, dst, virtual string, overwrite bool, index trash.Index) (restored, skipped int, err error) {
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
			index.Drop(virtual)
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
	if err := trash.MoveTree(src, dst); err != nil {
		return 0, 0, err
	}
	index.Drop(virtual)
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
func pruneTrash(folder, virtual string, index trash.Index) {
	for at := virtual; at != "/" && at != "."; at = path.Dir(at) {
		items, err := os.ReadDir(trash.Path(folder, at))
		if err == nil && len(items) > 0 {
			break
		}
		if err == nil && os.Remove(trash.Path(folder, at)) != nil {
			break
		}
		index.Drop(at)
	}
	for key := range index {
		if _, err := os.Lstat(trash.Path(folder, key)); errors.Is(err, os.ErrNotExist) {
			delete(index, key)
		}
	}
}

// deleteFromTrash deletes what the trash holds at virtual for good.
func (s *Server) deleteFromTrash(set *settings, virtual string) error {
	trash.Lock()
	defer trash.Unlock()
	folder := set.cfg.TrashFolder
	at, err := trashItem(folder, virtual)
	if err != nil {
		return err
	}
	index := trash.ReadIndex(folder)
	if err := os.RemoveAll(at); err != nil {
		return err
	}
	index.Drop(virtual)
	pruneTrash(folder, path.Dir(virtual), index)
	return trash.WriteIndex(folder, index)
}

// emptyTrash deletes everything in the trash for good, and reports how many
// files and empty folders that was.
func (s *Server) emptyTrash(set *settings) (int, error) {
	trash.Lock()
	defer trash.Unlock()
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
		if item.Name() != trash.IndexName {
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
	trash.Lock()
	defer trash.Unlock()
	folder := set.cfg.TrashFolder
	items, err := os.ReadDir(folder)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Debug("http cleanup cannot read the trash", "path", folder, "error", err)
		}
		return
	}
	index := trash.ReadIndex(folder)
	before := len(index)
	cutoff := time.Now().Add(-time.Duration(set.cfg.TrashRetentionDays) * 24 * time.Hour)
	purged := 0
	for _, item := range items {
		if item.Name() == trash.IndexName || strings.HasPrefix(item.Name(), trash.IndexName+".") {
			continue
		}
		s.purgeTree(folder, "/"+item.Name(), time.Time{}, cutoff, index, &purged)
	}
	if purged > 0 || len(index) != before {
		if err := trash.WriteIndex(folder, index); err != nil {
			s.log.Error("http cleanup cannot write the trash index", "path", folder, "error", err)
		}
	}
}

// purgeTree removes what at virtual was deleted before cutoff, and reports
// whether nothing of it is left. inherited is when the folder above was
// deleted, zero for one that never was on its own.
func (s *Server) purgeTree(folder, virtual string, inherited, cutoff time.Time, index trash.Index, purged *int) bool {
	at := trash.Path(folder, virtual)
	info, err := os.Lstat(at)
	if err != nil {
		index.Drop(virtual)
		return true
	}
	when := inherited
	if own, ok := index[virtual]; ok {
		when = own
	}
	// a file, or a folder deleted as one, goes as a whole; a folder that
	// holds what was deleted later, or that is only there for what is in it,
	// is decided entry by entry
	if !info.IsDir() || (!when.IsZero() && !index.HasBelow(virtual)) {
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
		index.Drop(virtual)
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
	index.Drop(virtual)
	return true
}
