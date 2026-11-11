package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// The functions below are the os calls that touch files, each with a log in
// front: they do what the os function of the same name does and return what
// it returns, and at debug they write one "file <op>" record that names the
// line that called them, with the path, what else the call knows for free and
// the error when it failed. A call site reads as the os call it replaces:
//
//	info, err := logging.Stat(c.log, target.Path)
//
// Above debug the record costs one level check.

func traced(log *slog.Logger) bool {
	return log != nil && log.Enabled(context.Background(), slog.LevelDebug)
}

func took(started time.Time) slog.Attr {
	return slog.Duration("took", time.Since(started).Round(time.Microsecond))
}

func infoAttrs(info fs.FileInfo) []slog.Attr {
	if info == nil {
		return nil
	}
	return []slog.Attr{slog.Int64("size", info.Size()), slog.String("mode", info.Mode().String())}
}

// Stat is os.Stat, traced.
func Stat(log *slog.Logger, name string) (fs.FileInfo, error) {
	info, err := os.Stat(name)
	if traced(log) {
		fileOp(log, 1, "stat", name, err, infoAttrs(info)...)
	}
	return info, err
}

// Lstat is os.Lstat, traced.
func Lstat(log *slog.Logger, name string) (fs.FileInfo, error) {
	info, err := os.Lstat(name)
	if traced(log) {
		fileOp(log, 1, "lstat", name, err, infoAttrs(info)...)
	}
	return info, err
}

// Open is os.Open, traced.
func Open(log *slog.Logger, name string) (*os.File, error) {
	file, err := os.Open(name)
	if traced(log) {
		fileOp(log, 1, "open", name, err)
	}
	return file, err
}

// OpenFile is os.OpenFile, traced.
func OpenFile(log *slog.Logger, name string, flag int, perm fs.FileMode) (*os.File, error) {
	file, err := os.OpenFile(name, flag, perm)
	if traced(log) {
		fileOp(log, 1, "open", name, err, slog.String("flags", openFlags(flag)), slog.String("perm", perm.String()))
	}
	return file, err
}

// Create is os.Create, traced.
func Create(log *slog.Logger, name string) (*os.File, error) {
	file, err := os.Create(name)
	if traced(log) {
		fileOp(log, 1, "create", name, err)
	}
	return file, err
}

// CreateTemp is os.CreateTemp, traced; the record names the file made.
func CreateTemp(log *slog.Logger, dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if traced(log) {
		name := filepath.Join(dir, pattern)
		if file != nil {
			name = file.Name()
		}
		fileOp(log, 1, "create temp", name, err)
	}
	return file, err
}

// MkdirTemp is os.MkdirTemp, traced; the record names the folder made.
func MkdirTemp(log *slog.Logger, dir, pattern string) (string, error) {
	name, err := os.MkdirTemp(dir, pattern)
	if traced(log) {
		shown := name
		if err != nil {
			shown = filepath.Join(dir, pattern)
		}
		fileOp(log, 1, "mkdir temp", shown, err)
	}
	return name, err
}

// Mkdir is os.Mkdir, traced.
func Mkdir(log *slog.Logger, name string, perm fs.FileMode) error {
	err := os.Mkdir(name, perm)
	if traced(log) {
		fileOp(log, 1, "mkdir", name, err, slog.String("perm", perm.String()))
	}
	return err
}

// MkdirAll is os.MkdirAll, traced.
func MkdirAll(log *slog.Logger, name string, perm fs.FileMode) error {
	err := os.MkdirAll(name, perm)
	if traced(log) {
		fileOp(log, 1, "mkdir all", name, err, slog.String("perm", perm.String()))
	}
	return err
}

// Remove is os.Remove, traced.
func Remove(log *slog.Logger, name string) error {
	err := os.Remove(name)
	if traced(log) {
		fileOp(log, 1, "remove", name, err)
	}
	return err
}

// RemoveAll is os.RemoveAll, traced.
func RemoveAll(log *slog.Logger, name string) error {
	started := time.Now()
	err := os.RemoveAll(name)
	if traced(log) {
		fileOp(log, 1, "remove all", name, err, took(started))
	}
	return err
}

// Rename is os.Rename, traced.
func Rename(log *slog.Logger, oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if traced(log) {
		fileOp(log, 1, "rename", oldpath, err, slog.String("to", newpath))
	}
	return err
}

// Symlink is os.Symlink, traced: path is the link and target what it names.
func Symlink(log *slog.Logger, oldname, newname string) error {
	err := os.Symlink(oldname, newname)
	if traced(log) {
		fileOp(log, 1, "symlink", newname, err, slog.String("target", oldname))
	}
	return err
}

// Readlink is os.Readlink, traced.
func Readlink(log *slog.Logger, name string) (string, error) {
	target, err := os.Readlink(name)
	if traced(log) {
		fileOp(log, 1, "readlink", name, err, slog.String("target", target))
	}
	return target, err
}

// ReadDir is os.ReadDir, traced with the number of entries.
func ReadDir(log *slog.Logger, name string) ([]os.DirEntry, error) {
	started := time.Now()
	entries, err := os.ReadDir(name)
	if traced(log) {
		fileOp(log, 1, "readdir", name, err, slog.Int("entries", len(entries)), took(started))
	}
	return entries, err
}

// ReadFile is os.ReadFile, traced with the bytes read.
func ReadFile(log *slog.Logger, name string) ([]byte, error) {
	data, err := os.ReadFile(name)
	if traced(log) {
		fileOp(log, 1, "read", name, err, slog.Int("bytes", len(data)))
	}
	return data, err
}

// WriteFile is os.WriteFile, traced with the bytes written.
func WriteFile(log *slog.Logger, name string, data []byte, perm fs.FileMode) error {
	err := os.WriteFile(name, data, perm)
	if traced(log) {
		fileOp(log, 1, "write", name, err, slog.Int("bytes", len(data)), slog.String("perm", perm.String()))
	}
	return err
}

// Chmod is os.Chmod, traced.
func Chmod(log *slog.Logger, name string, mode fs.FileMode) error {
	err := os.Chmod(name, mode)
	if traced(log) {
		fileOp(log, 1, "chmod", name, err, slog.String("mode", mode.String()))
	}
	return err
}

// Chtimes is os.Chtimes, traced.
func Chtimes(log *slog.Logger, name string, atime, mtime time.Time) error {
	err := os.Chtimes(name, atime, mtime)
	if traced(log) {
		fileOp(log, 1, "chtimes", name, err, slog.Time("mtime", mtime))
	}
	return err
}

// Truncate is os.Truncate, traced.
func Truncate(log *slog.Logger, name string, size int64) error {
	err := os.Truncate(name, size)
	if traced(log) {
		fileOp(log, 1, "truncate", name, err, slog.Int64("size", size))
	}
	return err
}

// WalkDir is filepath.WalkDir, traced as one record for the whole walk,
// with the number of entries visited: a record per entry would be a listing
// of the tree, which the walk's own records give where they matter.
func WalkDir(log *slog.Logger, root string, fn fs.WalkDirFunc) error {
	if !traced(log) {
		return filepath.WalkDir(root, fn)
	}
	started := time.Now()
	visited := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		visited++
		return fn(path, d, err)
	})
	fileOp(log, 1, "walk", root, err, slog.Int("entries", visited), took(started))
	return err
}

// openFlags names the flags of an open as the os package spells them.
func openFlags(flag int) string {
	var names []string
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		names = append(names, "WRONLY")
	case os.O_RDWR:
		names = append(names, "RDWR")
	default:
		names = append(names, "RDONLY")
	}
	for _, f := range []struct {
		bit  int
		name string
	}{{os.O_APPEND, "APPEND"}, {os.O_CREATE, "CREATE"}, {os.O_EXCL, "EXCL"}, {os.O_SYNC, "SYNC"}, {os.O_TRUNC, "TRUNC"}} {
		if flag&f.bit != 0 {
			names = append(names, f.name)
		}
	}
	out := names[0]
	for _, n := range names[1:] {
		out += "|" + n
	}
	return out
}

// Summary records a file that was read by something that cannot record it
// itself, such as the configuration, which is read before there is a logger:
// "file <op>" with its size, when it was changed and the start of its
// SHA-256, which tells two versions of it apart in the log.
func Summary(log *slog.Logger, op, name string) {
	if !traced(log) {
		return
	}
	data, err := os.ReadFile(name)
	info, statErr := os.Stat(name)
	if err == nil {
		err = statErr
	}
	attrs := []slog.Attr{slog.Int("bytes", len(data))}
	if info != nil {
		attrs = append(attrs, slog.Time("modified", info.ModTime()), slog.String("mode", info.Mode().String()))
	}
	if err == nil {
		sum := sha256.Sum256(data)
		attrs = append(attrs, slog.String("sha256", hex.EncodeToString(sum[:6])))
	}
	fileOp(log, 1, op, name, err, attrs...)
}
