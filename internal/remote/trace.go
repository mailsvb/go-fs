package remote

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
)

// The trace of a host, at debug: "remote login" when one is logged in to,
// and "remote <op>" for each operation on it, with the path, how long it
// took and the error. The logger is the one ctx carries, which is the
// request's for what a page does and the job's or the sync's otherwise; a
// ctx without one traces nothing. What a file holds is never traced, only
// how much of it there was.

// logOf is the logger ctx carries.
func logOf(ctx context.Context) *slog.Logger {
	return logging.From(ctx, logging.Discard)
}

// traced is fsys with each of its operations traced to log; layer tells the
// host itself ("host") from the vault of Cryptomator on it ("vault"), whose
// paths are the plaintext ones.
func traced(fsys FS, log *slog.Logger, l Login, layer string) FS {
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		return fsys
	}
	t := tracedFS{FS: fsys, log: log.With("remote", l.Where(""), "layer", layer)}
	if _, ok := fsys.(Chtimer); ok {
		return tracedChtimerFS{t}
	}
	return t
}

type tracedFS struct {
	FS
	log *slog.Logger
}

func (t tracedFS) op(op, p string, started time.Time, err error, attrs ...slog.Attr) {
	all := append([]slog.Attr{slog.String("path", p)}, attrs...)
	all = append(all, slog.Duration("took", time.Since(started).Round(time.Millisecond)))
	if err != nil {
		all = append(all, slog.Any("error", err))
	}
	t.log.LogAttrs(context.Background(), slog.LevelDebug, "remote "+op, all...)
}

func (t tracedFS) Stat(p string) (fs.FileInfo, error) {
	started := time.Now()
	info, err := t.FS.Stat(p)
	t.op("stat", p, started, err, infoAttrs(info)...)
	return info, err
}

func (t tracedFS) Lstat(p string) (fs.FileInfo, error) {
	started := time.Now()
	info, err := t.FS.Lstat(p)
	t.op("lstat", p, started, err, infoAttrs(info)...)
	return info, err
}

func (t tracedFS) ReadDir(p string) ([]fs.FileInfo, error) {
	started := time.Now()
	entries, err := t.FS.ReadDir(p)
	t.op("readdir", p, started, err, slog.Int("entries", len(entries)))
	return entries, err
}

func (t tracedFS) Open(p string) (io.ReadSeekCloser, error) {
	started := time.Now()
	file, err := t.FS.Open(p)
	t.op("open", p, started, err)
	if err != nil {
		return nil, err
	}
	return &tracedFile{ReadSeekCloser: file, fs: t, path: p, opened: time.Now()}, nil
}

func (t tracedFS) Put(p string, body io.Reader, size int64) (int64, error) {
	started := time.Now()
	n, err := t.FS.Put(p, body, size)
	t.op("put", p, started, err, slog.Int64("bytes", n), slog.Int64("size", size))
	return n, err
}

func (t tracedFS) Mkdir(p string) error {
	started := time.Now()
	err := t.FS.Mkdir(p)
	t.op("mkdir", p, started, err)
	return err
}

func (t tracedFS) Rename(from, to string) error {
	started := time.Now()
	err := t.FS.Rename(from, to)
	t.op("rename", from, started, err, slog.String("to", to))
	return err
}

func (t tracedFS) Remove(p string) error {
	started := time.Now()
	err := t.FS.Remove(p)
	t.op("remove", p, started, err)
	return err
}

func (t tracedFS) RemoveDir(p string) error {
	started := time.Now()
	err := t.FS.RemoveDir(p)
	t.op("rmdir", p, started, err)
	return err
}

func (t tracedFS) Close() error {
	started := time.Now()
	err := t.FS.Close()
	t.op("logout", "", started, err)
	return err
}

// tracedChtimerFS is tracedFS for a host that can set modification times.
type tracedChtimerFS struct{ tracedFS }

func (t tracedChtimerFS) Chtimes(p string, mtime time.Time) error {
	started := time.Now()
	err := t.FS.(Chtimer).Chtimes(p, mtime)
	t.op("chtimes", p, started, err, slog.Time("mtime", mtime))
	return err
}

// tracedFile records, as it is closed, how much of a file was read.
type tracedFile struct {
	io.ReadSeekCloser
	fs     tracedFS
	path   string
	opened time.Time
	read   int64
}

func (f *tracedFile) Read(p []byte) (int, error) {
	n, err := f.ReadSeekCloser.Read(p)
	f.read += int64(n)
	return n, err
}

func (f *tracedFile) Close() error {
	err := f.ReadSeekCloser.Close()
	f.fs.op("close", f.path, f.opened, err, slog.Int64("bytesRead", f.read))
	return err
}

func infoAttrs(info fs.FileInfo) []slog.Attr {
	if info == nil {
		return nil
	}
	return []slog.Attr{slog.Int64("size", info.Size()), slog.Bool("dir", info.IsDir())}
}

// traceLogin records a login, with the login as its LogValue shows it,
// which says whether there is a password and never what it is.
func traceLogin(ctx context.Context, what string, l Login, started time.Time, err error) {
	log := logOf(ctx)
	if !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{slog.Any("login", l), slog.Duration("took", time.Since(started).Round(time.Millisecond))}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	log.LogAttrs(ctx, slog.LevelDebug, "remote "+what, attrs...)
}
