package sftp

import (
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"sync/atomic"

	"github.com/pkg/sftp"

	"go-fs/internal/vfs"
)

// session is the state one authenticated client has while its SFTP subsystem
// runs: which account it is, and where its transfers are logged. The account
// itself is looked up per request rather than held, see account below.
type session struct {
	server *Server
	name   string
	log    *slog.Logger
}

// account resolves the account this session belongs to against the
// configuration as it stands right now.
//
// It is deliberately not held from the handshake: an account is checked again
// on every request, so a right taken away by a reload applies to the next
// request the client makes, and an account that was removed can do nothing at
// all rather than keeping what it had until it disconnects.
func (s *session) account() (*account, error) {
	user := s.server.settings().users[s.name]
	if user == nil {
		s.log.Info("sftp account is no longer configured, refusing the request")
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	return user, nil
}

func (s *Server) handlers(name string, log *slog.Logger) sftp.Handlers {
	sess := &session{server: s, name: name, log: log}
	return sftp.Handlers{
		FileGet:  sess,
		FilePut:  sess,
		FileCmd:  sess,
		FileList: sess,
	}
}

// resolve maps a client path onto the filesystem. Client paths are absolute,
// so they are resolved against the root of the virtual filesystem; the same
// vfs the FTP and TFTP servers use rejects ".." and symbolic links that leave
// the base folder.
func (s *session) resolve(user *account, clientPath string) (vfs.Target, error) {
	target := user.root.Resolve("/", clientPath)
	if !target.Valid {
		s.log.Debug("sftp path refused", "path", clientPath)
		return target, sftp.ErrSSHFxPermissionDenied
	}
	return target, nil
}

// Fileread serves a read, which needs the retrieve right.
func (s *session) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	user, err := s.account()
	if err != nil {
		return nil, err
	}
	if !user.perms.FileRetrieve {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	target, err := s.resolve(user, r.Filepath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(target.Path)
	if err != nil {
		return nil, err
	}
	return &countingReader{file: file, session: s, virtual: target.Virtual}, nil
}

// Filewrite serves a write. A new file needs the create right and an existing
// one the overwrite right, the same split the FTP STOR command makes.
func (s *session) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	user, err := s.account()
	if err != nil {
		return nil, err
	}
	target, err := s.resolve(user, r.Filepath)
	if err != nil {
		return nil, err
	}

	_, statErr := os.Stat(target.Path)
	exists := statErr == nil
	if exists && !user.perms.FileOverwrite {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	if !exists && !user.perms.FileCreate {
		return nil, sftp.ErrSSHFxPermissionDenied
	}

	// The library hands us an io.WriterAt and the client counts its own
	// offsets from zero, so an append is served by shifting those offsets past
	// what the file already holds. O_APPEND itself cannot be used: the
	// standard library refuses WriteAt on a file opened with it.
	var base int64
	flags := os.O_WRONLY | os.O_CREATE
	pflags := r.Pflags()
	if pflags.Trunc {
		flags |= os.O_TRUNC
	}
	if pflags.Excl {
		flags |= os.O_EXCL
	}
	if pflags.Append && exists {
		info, err := os.Stat(target.Path)
		if err != nil {
			return nil, err
		}
		base = info.Size()
	}

	file, err := os.OpenFile(target.Path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	return &countingWriter{file: file, session: s, virtual: target.Virtual, base: base}, nil
}

// Filecmd serves everything that changes the folder rather than a file's
// content. Each method needs the right that matches what it does, and a rename
// needs both, because it creates one name and removes another.
func (s *session) Filecmd(r *sftp.Request) error {
	user, err := s.account()
	if err != nil {
		return err
	}
	target, err := s.resolve(user, r.Filepath)
	if err != nil {
		return err
	}

	switch r.Method {
	case "Remove":
		if !user.perms.FileDelete || target.IsRoot() {
			return sftp.ErrSSHFxPermissionDenied
		}
		info, err := os.Stat(target.Path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			// removing a folder is Rmdir, which has its own right
			return sftp.ErrSSHFxFailure
		}
		return os.Remove(target.Path)

	case "Rmdir":
		// the base folder itself is not the client's to remove: it would take
		// the served tree with it and leave every path invalid
		if !user.perms.FolderDelete || target.IsRoot() {
			return sftp.ErrSSHFxPermissionDenied
		}
		info, err := os.Stat(target.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return sftp.ErrSSHFxFailure
		}
		// rmdir removes an empty folder, here as everywhere else: a tree of
		// files is only deleted by an account that may delete files, one by one
		return os.Remove(target.Path)

	case "Mkdir":
		if !user.perms.FolderCreate {
			return sftp.ErrSSHFxPermissionDenied
		}
		if _, err := os.Stat(target.Path); err == nil {
			return sftp.ErrSSHFxFailure
		}
		return os.MkdirAll(target.Path, 0o755)

	case "Rename", "PosixRename":
		if !user.perms.FileCreate || !user.perms.FileDelete || target.IsRoot() {
			return sftp.ErrSSHFxPermissionDenied
		}
		destination, err := s.resolve(user, r.Target)
		if err != nil {
			return err
		}
		if destination.IsRoot() {
			return sftp.ErrSSHFxPermissionDenied
		}
		if r.Method == "Rename" {
			// plain rename must not clobber, POSIX rename may
			if _, err := os.Stat(destination.Path); err == nil {
				return sftp.ErrSSHFxFailure
			}
		}
		return os.Rename(target.Path, destination.Path)

	case "Setstat", "Fsetstat":
		// changing mode, times or size is a change to the file, so it needs
		// the same right as MFMT and SITE CHMOD do over FTP
		if !user.perms.FileOverwrite || target.IsRoot() {
			return sftp.ErrSSHFxPermissionDenied
		}
		return s.setstat(target, r)

	case "Symlink", "Link":
		// links are never created: they are the one thing that could point out
		// of the base folder, and nothing needs them here
		return sftp.ErrSSHFxOpUnsupported
	}
	return sftp.ErrSSHFxOpUnsupported
}

// setstat applies the attributes a SETSTAT request carries.
func (s *session) setstat(target vfs.Target, r *sftp.Request) error {
	attrs := r.Attributes()
	flags := r.AttrFlags()
	if flags.Size {
		if err := os.Truncate(target.Path, int64(attrs.Size)); err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := os.Chmod(target.Path, attrs.FileMode().Perm()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := os.Chtimes(target.Path, attrs.AccessTime(), attrs.ModTime()); err != nil {
			return err
		}
	}
	// ownership is not ours to change, and a request to do so is not an error
	return nil
}

// Filelist serves listing and stat, which every account may do, exactly as
// LIST needs no right over FTP.
func (s *session) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	user, err := s.account()
	if err != nil {
		return nil, err
	}
	target, err := s.resolve(user, r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		entries, err := os.ReadDir(target.Path)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				// a file that vanished between the read and the stat is
				// skipped rather than failing the whole listing
				continue
			}
			infos = append(infos, info)
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
		return listerAt(infos), nil

	case "Stat":
		info, err := os.Stat(target.Path)
		if err != nil {
			return nil, err
		}
		return listerAt{info}, nil

	case "Readlink":
		// the client is told where a link points only if the target stays
		// inside the base folder
		destination, err := os.Readlink(target.Path)
		if err != nil {
			return nil, err
		}
		// a relative target is relative to the folder the link sits in; an
		// absolute one is already a path in the served tree
		resolved := user.root.Resolve(path.Dir(target.Virtual), destination)
		if !resolved.Valid {
			return nil, sftp.ErrSSHFxPermissionDenied
		}
		info, err := os.Stat(resolved.Path)
		if err != nil {
			return nil, err
		}
		return listerAt{renamed{FileInfo: info, name: resolved.Virtual}}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

// listerAt serves a directory listing in the chunks the client asks for.
type listerAt []os.FileInfo

func (l listerAt) ListAt(target []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(target, l[offset:])
	if n < len(target) {
		return n, io.EOF
	}
	return n, nil
}

// renamed reports a different name for a FileInfo, which Readlink needs.
type renamed struct {
	os.FileInfo
	name string
}

func (r renamed) Name() string { return r.name }

// countingReader and countingWriter carry a transfer's byte count so that the
// log record can report it. The request server closes handles that implement
// io.Closer, which is where the record is written.
type countingReader struct {
	file    *os.File
	session *session
	virtual string
	bytes   atomic.Int64
}

func (c *countingReader) ReadAt(b []byte, offset int64) (int, error) {
	n, err := c.file.ReadAt(b, offset)
	c.bytes.Add(int64(n))
	return n, err
}

func (c *countingReader) Close() error {
	err := c.file.Close()
	c.session.log.Info("sftp download", "file", c.virtual, "bytes", c.bytes.Load())
	return err
}

type countingWriter struct {
	file    *os.File
	session *session
	virtual string
	// base is what an append starts from, zero for every other open
	base  int64
	bytes atomic.Int64
}

func (c *countingWriter) WriteAt(b []byte, offset int64) (int, error) {
	n, err := c.file.WriteAt(b, c.base+offset)
	c.bytes.Add(int64(n))
	return n, err
}

func (c *countingWriter) Close() error {
	err := c.file.Close()
	c.session.log.Info("sftp upload", "file", c.virtual, "bytes", c.bytes.Load())
	return err
}
