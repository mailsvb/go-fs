//go:build !light && !no_webdav

package httpd

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
	"golang.org/x/net/webdav"
)

// The WebDAV package reaches the served folder through the five methods of
// webdav.FileSystem, which are traced here around the code that serves them:
// one "file <op>" record each, at debug, with the path the client named, how
// long it took and the error the package is handed. A PROPFIND of a deep
// folder is many of these; the request's "http <" says which one it was.

func (d *davFS) trace(op, name string, started time.Time, err error, attrs ...slog.Attr) {
	log := d.s.logFor(d.r)
	if !log.Enabled(d.r.Context(), slog.LevelDebug) {
		return
	}
	attrs = append(attrs, slog.String("via", "webdav"),
		slog.Duration("took", time.Since(started).Round(time.Microsecond)))
	logging.FileOp(log, op, name, err, attrs...)
}

func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	started := time.Now()
	info, err := d.stat(ctx, name)
	d.trace("stat", name, started, err)
	return info, err
}

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	started := time.Now()
	file, err := d.openFile(ctx, name, flag, perm)
	d.trace("open", name, started, err, slog.Int("flag", flag))
	return file, err
}

func (d *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	started := time.Now()
	err := d.mkdir(ctx, name, perm)
	d.trace("mkdir", name, started, err)
	return err
}

func (d *davFS) RemoveAll(ctx context.Context, name string) error {
	started := time.Now()
	err := d.removeAll(ctx, name)
	d.trace("remove", name, started, err)
	return err
}

func (d *davFS) Rename(ctx context.Context, oldName, newName string) error {
	started := time.Now()
	err := d.rename(ctx, oldName, newName)
	d.trace("rename", oldName, started, err, slog.String("to", newName))
	return err
}
