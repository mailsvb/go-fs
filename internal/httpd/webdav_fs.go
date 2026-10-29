//go:build !light && !no_webdav

package httpd

import (
	"context"
	"encoding/xml"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/net/webdav"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// davFS is the served folder as the WebDAV package sees it, for one request.
// It is the boundary the rights are kept at: every path is resolved through
// the root, which keeps it inside the served folder, and checked against the
// account before anything is read or changed. davChecked has refused most of
// what would fail here before the package started; this is what holds if it
// missed something.
//
// A path the account may not read is not there, as a path outside the served
// folder is not: it is left out of a listing and answered 404 when asked for.
// The errors carry os.ErrNotExist and os.ErrPermission inside a PathError,
// which is what the package needs to skip such a path in a listing rather
// than give up on the whole of it.
type davFS struct {
	s    *Server
	set  *settings
	user *account
	r    *http.Request
	body *davBody
}

func (d *davFS) resolve(op, name string) (vfs.Target, error) {
	target := d.s.root.Resolve("/", name)
	if !target.Valid {
		return target, &os.PathError{Op: op, Path: name, Err: os.ErrNotExist}
	}
	return target, nil
}

// readable reports whether the account may see a path at all.
func (d *davFS) readable(virtual string) bool {
	return d.s.permits(d.set, d.user, http.MethodGet, virtual, actRead)
}

func (d *davFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	target, err := d.resolve("stat", name)
	if err != nil {
		return nil, err
	}
	if !d.readable(target.Virtual) {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	info, err := os.Stat(target.Path)
	if err != nil {
		return nil, err
	}
	return davInfo{info}, nil
}

// OpenFile opens a file to read, or to write what replaces it. The WebDAV
// package asks for the second with O_CREATE and O_TRUNC, for a PUT and for
// each file a COPY makes. A PROPPATCH asks for O_RDWR alone, to patch the
// properties, which are taken and not kept, so that is a read too.
func (d *davFS) OpenFile(_ context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	target, err := d.resolve("open", name)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 {
		return d.create(target)
	}
	if !d.readable(target.Virtual) {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	file, err := os.Open(target.Path)
	if err != nil {
		return nil, err
	}
	return &davFile{File: file, dav: d, target: target}, nil
}

// create opens what a file is written to: a file in the staging folder,
// which becomes the file when it is closed, so that nothing half written is
// ever seen at the name.
func (d *davFS) create(target vfs.Target) (webdav.File, error) {
	denied := &os.PathError{Op: "create", Path: target.Virtual, Err: os.ErrPermission}
	if target.IsRoot() {
		return nil, denied
	}
	info, err := os.Stat(target.Path)
	exists := err == nil
	if exists && !info.Mode().IsRegular() {
		return nil, denied
	}
	if !d.s.permits(d.set, d.user, http.MethodPut, target.Virtual, davWriteAction(target.Path)) {
		return nil, denied
	}
	// a file is not given the folders above it, as a PUT of the page's
	// would: RFC 4918 9.7.1 answers 409 for a folder that is not there
	if _, err := os.Stat(filepath.Dir(target.Path)); err != nil {
		return nil, err
	}

	staging := stagingFolder(d.set)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}
	staged, err := os.CreateTemp(staging, "webdav-*")
	if err != nil {
		return nil, err
	}
	// a temporary file is the owner's alone, and this one is about to be a
	// file of the served folder like an upload
	_ = staged.Chmod(0o644)
	return &davUpload{File: staged, dav: d, target: target, replace: exists, started: time.Now()}, nil
}

func (d *davFS) Mkdir(_ context.Context, name string, _ os.FileMode) error {
	target, err := d.resolve("mkdir", name)
	if err != nil {
		return err
	}
	if target.IsRoot() {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrExist}
	}
	if !d.s.permits(d.set, d.user, methodMkcol, target.Virtual, actMkdir) {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrPermission}
	}
	if err := os.Mkdir(target.Path, 0o755); err != nil {
		return err
	}
	d.s.log.Info("webdav mkdir", "user", nameOf(d.user), "folder", target.Virtual,
		"method", d.r.Method, "address", clientAddress(d.set, d.r))
	return nil
}

func (d *davFS) RemoveAll(_ context.Context, name string) error {
	target, err := d.resolve("remove", name)
	if err != nil {
		return err
	}
	if target.IsRoot() {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrPermission}
	}
	info, err := os.Lstat(target.Path)
	if err != nil {
		return err
	}
	if err := d.s.davRemovable(d.set, d.user, target); err != nil {
		return err
	}
	if err := os.RemoveAll(target.Path); err != nil {
		return err
	}
	d.s.log.Info("webdav delete", "user", nameOf(d.user), "path", target.Virtual,
		"folder", info.IsDir(), "method", d.r.Method, "address", clientAddress(d.set, d.r))
	return nil
}

// Rename moves a file or a folder. The WebDAV package removes a destination
// it may overwrite before it asks, so a destination that is still there is
// one the account cannot see, and it is refused rather than replaced unseen.
func (d *davFS) Rename(_ context.Context, oldName, newName string) error {
	from, err := d.resolve("rename", oldName)
	if err != nil {
		return err
	}
	to, err := d.resolve("rename", newName)
	if err != nil {
		return err
	}
	if from.IsRoot() || to.IsRoot() {
		return &os.PathError{Op: "rename", Path: oldName, Err: os.ErrPermission}
	}
	if _, err := os.Lstat(to.Path); err == nil {
		return &os.PathError{Op: "rename", Path: newName, Err: os.ErrExist}
	}
	if err := d.s.davMovable(d.set, d.user, from, to); err != nil {
		return err
	}
	if err := os.Rename(from.Path, to.Path); err != nil {
		return err
	}
	d.s.log.Info("webdav move", "user", nameOf(d.user), "from", from.Virtual, "to", to.Virtual,
		"address", clientAddress(d.set, d.r))
	return nil
}

// davInfo is a file's information with the content type taken from its
// name. Without it the WebDAV package opens every file of a listing to sniff
// the type from what is in it.
type davInfo struct {
	os.FileInfo
}

func (i davInfo) ContentType(context.Context) (string, error) {
	if kind := mime.TypeByExtension(filepath.Ext(i.Name())); kind != "" {
		return kind, nil
	}
	return "application/octet-stream", nil
}

// davFile is a file or a folder opened to read.
type davFile struct {
	*os.File
	dav    *davFS
	target vfs.Target
}

func (f *davFile) Stat() (os.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return davInfo{info}, nil
}

// Readdir lists a folder without what the account may not see, and without
// a link that leads out of the served folder.
func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	infos, err := f.File.Readdir(count)
	visible := make([]os.FileInfo, 0, len(infos))
	for _, info := range infos {
		child := f.dav.s.root.Resolve(f.target.Virtual, info.Name())
		if !child.Valid || !f.dav.readable(child.Virtual) {
			continue
		}
		visible = append(visible, davInfo{info})
	}
	return visible, err
}

func (f *davFile) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: f.target.Virtual, Err: os.ErrPermission}
}

// DeadProps and Patch take the properties a client sets and keep none of
// them. Explorer sets its Win32 times and attributes after every file it
// writes, and takes a refusal for a failed copy; nothing here has anywhere to
// keep them, and the times it would set are the ones the file already has.
func (f *davFile) DeadProps() (map[xml.Name]webdav.Property, error) {
	return nil, nil
}

func (f *davFile) Patch(patches []webdav.Proppatch) ([]webdav.Propstat, error) {
	accepted := webdav.Propstat{Status: http.StatusOK}
	for _, patch := range patches {
		for _, prop := range patch.Props {
			accepted.Props = append(accepted.Props, webdav.Property{XMLName: prop.XMLName})
		}
	}
	return []webdav.Propstat{accepted}, nil
}

// davUpload is a file being written, staged until it is closed.
type davUpload struct {
	*os.File
	dav     *davFS
	target  vfs.Target
	replace bool
	started time.Time
}

// Close puts the staged file at its name, or throws it away when the body it
// was written from did not arrive whole. Whether the name may be replaced was
// decided when the file was opened; it is checked again under the lock that
// the other uploads to the name take, so a name taken since is not
// overwritten by a create.
func (u *davUpload) Close() error {
	staged := u.File.Name()
	defer func() { _ = os.Remove(staged) }()
	info, statErr := u.File.Stat()
	if err := u.File.Close(); err != nil {
		return err
	}
	if statErr != nil {
		return statErr
	}
	d := u.dav
	if d.body != nil && d.body.failed != nil {
		d.s.log.Info("webdav upload failed", "user", nameOf(d.user), "file", u.target.Virtual,
			"bytes", info.Size(), "address", clientAddress(d.set, d.r),
			"took", time.Since(u.started).Round(time.Millisecond), "error", d.body.failed)
		return d.body.failed
	}

	lock := d.s.uploadLock(u.target.Path)
	lock.Lock()
	defer lock.Unlock()
	if existing, err := os.Stat(u.target.Path); err == nil && (!u.replace || !existing.Mode().IsRegular()) {
		return &os.PathError{Op: "create", Path: u.target.Virtual, Err: os.ErrExist}
	}
	if err := finalizeUpload(staged, u.target.Path, u.replace); err != nil {
		return err
	}
	d.s.log.Info("webdav upload", "user", nameOf(d.user), "file", u.target.Virtual,
		"bytes", info.Size(), "replaced", u.replace, "method", d.r.Method,
		"address", clientAddress(d.set, d.r), "took", time.Since(u.started).Round(time.Millisecond))
	return nil
}
