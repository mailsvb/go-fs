package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/trash"
)

// side is one of the two folders a sync keeps alike. Paths are from the top
// of the folder, with slashes, "" being the top.
type side interface {
	// list reads the folder and everything in it. Links, and the files a
	// write of go-fs leaves while it is not finished, are left out.
	list(ctx context.Context) (map[string]entry, error)
	open(rel string) (io.ReadCloser, error)
	// put writes a file whole, so that nothing is found under its name
	// before all of it is there, and gives it the modification time mod.
	put(rel string, body io.Reader, size int64, mod time.Time) error
	mkdir(rel string) error
	// remove removes a file, or a folder with nothing in it.
	remove(rel string, dir bool) error
}

// partial reports a file a write of go-fs leaves under a hidden name while
// it is not finished, here or on a remote host.
func partial(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".go-fs-") && strings.HasSuffix(name, ".part")
}

// localSide is a folder of general.baseFolderFiles.
type localSide struct {
	// log takes the trace of what the sync does on disk
	log *slog.Logger
	// root is the folder on disk, and virtual where it is in the served
	// folder, as the file listing and the trash name it.
	root    string
	virtual string
	// trash is general.baseFolderTrash, "" for none: what is removed goes
	// there when there is one.
	trash string
}

func (l localSide) path(rel string) string {
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

func (l localSide) list(ctx context.Context) (map[string]entry, error) {
	found := map[string]entry{}
	err := logging.WalkDir(l.log, l.root, func(at string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if at == l.root {
			return nil
		}
		if partial(d.Name()) || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(l.root, at)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			found[rel] = entry{Dir: true}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		found[rel] = entry{Size: info.Size(), Mod: info.ModTime()}
		return nil
	})
	return found, err
}

func (l localSide) open(rel string) (io.ReadCloser, error) { return logging.Open(l.log, l.path(rel)) }

func (l localSide) put(rel string, body io.Reader, _ int64, mod time.Time) (err error) {
	final := l.path(rel)
	out, err := logging.CreateTemp(l.log, filepath.Dir(final), "."+filepath.Base(final)+".go-fs-*.part")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = logging.Remove(l.log, out.Name())
		}
	}()
	if err := out.Chmod(0o644); err != nil {
		return err
	}
	if _, err := io.Copy(out, body); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := logging.Chtimes(l.log, out.Name(), mod, mod); err != nil {
		return err
	}
	return logging.Rename(l.log, out.Name(), final)
}

func (l localSide) mkdir(rel string) error { return logging.Mkdir(l.log, l.path(rel), 0o755) }

func (l localSide) remove(rel string, dir bool) error {
	if dir || l.trash == "" {
		return logging.Remove(l.log, l.path(rel))
	}
	return trash.Move(l.log, l.trash, l.path(rel), path.Join(l.virtual, rel))
}

// remoteSide is a folder of a stored server.
type remoteSide struct {
	fsys remote.FS
	// root is the folder on the server, absolute.
	root string
}

// openRemote logs in to the server and finds the folder.
func openRemote(ctx context.Context, l remote.Login, ssh config.SSH, folder string) (*remoteSide, error) {
	fsys, err := remote.Open(ctx, l, ssh)
	if err != nil {
		return nil, err
	}
	if _, ok := fsys.(remote.Chtimer); !ok {
		_ = fsys.Close()
		return nil, fmt.Errorf("%s cannot keep the modification time of a file", l.Shown())
	}
	root, err := fsys.Resolve(folder)
	if err == nil {
		var info fs.FileInfo
		if info, err = fsys.Stat(root); err == nil && !info.IsDir() {
			err = fmt.Errorf("%s is not a folder", root)
		}
	}
	if err != nil {
		_ = fsys.Close()
		return nil, fmt.Errorf("the folder %q of %s: %w", folder, l.Shown(), err)
	}
	return &remoteSide{fsys: fsys, root: root}, nil
}

func (r *remoteSide) path(rel string) string { return path.Join(r.root, rel) }

func (r *remoteSide) list(ctx context.Context) (map[string]entry, error) {
	found := map[string]entry{}
	var walk func(rel string) error
	walk = func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		infos, err := r.fsys.ReadDir(r.path(rel))
		if err != nil {
			return err
		}
		for _, info := range infos {
			if partial(info.Name()) || remote.IsLink(info) {
				continue
			}
			at := path.Join(rel, info.Name())
			switch {
			case info.IsDir():
				found[at] = entry{Dir: true}
				if err := walk(at); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				found[at] = entry{Size: info.Size(), Mod: info.ModTime()}
			}
		}
		return nil
	}
	return found, walk("")
}

func (r *remoteSide) open(rel string) (io.ReadCloser, error) { return r.fsys.Open(r.path(rel)) }

func (r *remoteSide) put(rel string, body io.Reader, size int64, mod time.Time) error {
	if _, err := r.fsys.Put(r.path(rel), body, size); err != nil {
		return err
	}
	return r.fsys.(remote.Chtimer).Chtimes(r.path(rel), mod)
}

func (r *remoteSide) mkdir(rel string) error { return r.fsys.Mkdir(r.path(rel)) }

func (r *remoteSide) remove(rel string, dir bool) error {
	if dir {
		return r.fsys.RemoveDir(r.path(rel))
	}
	return r.fsys.Remove(r.path(rel))
}

func (r *remoteSide) close() error { return r.fsys.Close() }

// errEmptied refuses to remove anything when one side is empty and the other
// is not: a folder that was emptied by mistake, or a disk that is not
// mounted, would otherwise empty the other side too.
var errEmptied = errors.New("is empty while the other side is not, so nothing is removed; " +
	"remove what is left by hand, or switch delete off for one run")
