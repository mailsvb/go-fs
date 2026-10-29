//go:build !light && !no_webdav

package httpd

import (
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// WebDAV is served on the paths of the served folder themselves, so that
// Explorer and Finder mount http://host:8080/ as a drive. It shares four verbs
// with what the page and scripts send, and gives two of them another meaning:
// to a WebDAV client DELETE removes a folder with everything in it, and MOVE
// moves across folders and onto a name that is taken. Those two keep the
// meaning they always had for everyone else, which is why a request has to be
// told apart before it is answered; see davRequest.
//
// Whatever the meaning, the rights are the ones every other request is held
// to, checked for every path a request touches: a recursive DELETE removes no
// more than the DELETEs of each file and folder in it could. Telling the
// clients apart decides what a verb means, never what an account may do.

// davBodyLimit caps the XML a WebDAV request other than PUT may carry. A
// PROPFIND or a LOCK is a few hundred bytes; the cap only keeps one from being
// a way to make the server read without end.
const davBodyLimit = 1 << 20

// davState is what WebDAV keeps from one request to the next: the locks its
// clients hold. It lives on the Server rather than in the settings, so a
// reload does not drop a lock Finder is relying on.
type davState struct {
	locks webdav.LockSystem
}

func newDAVState() davState {
	return davState{locks: webdav.NewMemLS()}
}

// davOnly reports whether a method is one only WebDAV has, which is WebDAV
// whoever sends it.
func davOnly(method string) bool {
	switch method {
	case methodPropfind, methodProppatch, methodLock, methodUnlock, methodCopy, http.MethodOptions:
		return true
	}
	return false
}

// davClient reports whether a request comes from a client that mounts the
// folder. It is known by its User-Agent, which http.webdavClients lists, or by
// a header that only WebDAV defines: Depth, or If with the lock it holds.
func davClient(set *settings, r *http.Request) bool {
	if r.Header.Get("Depth") != "" || r.Header.Get("If") != "" {
		return true
	}
	agent := strings.ToLower(r.UserAgent())
	if agent == "" {
		return false
	}
	for _, pattern := range set.cfg.WebDAVClients {
		if pattern = strings.ToLower(strings.TrimSpace(pattern)); pattern != "" && strings.Contains(agent, pattern) {
			return true
		}
	}
	return false
}

// davRequest reports whether a request is answered as WebDAV.
//
// GET, HEAD and MKCOL never are: they mean the same to every client, and the
// handlers that answer them already do what WebDAV asks of them. PUT, DELETE
// and MOVE are when a WebDAV client sends them. PUT is among them because the
// page's PUT refuses a body that names a type other than octet-stream, and
// Office names the type of what it saves.
func davRequest(set *settings, r *http.Request) bool {
	if !set.cfg.WebDAV {
		return false
	}
	if davOnly(r.Method) {
		return true
	}
	switch r.Method {
	case http.MethodPut, http.MethodDelete, methodMove:
		return davClient(set, r)
	}
	return false
}

// davBasic reports whether a challenge also offers Basic. Explorer's WebDAV
// client does not answer the Digest this server offers everyone, and sends
// Basic only over TLS, so Basic is offered to a WebDAV request that came over
// TLS, and to nothing that would send the password in the clear.
func davBasic(set *settings, r *http.Request) bool {
	return set.cfg.WebDAV && (davOnly(r.Method) || davClient(set, r)) && secureRequest(set, r)
}

// serveDAV answers a WebDAV request, once ServeHTTP has said who it is from.
func (s *Server) serveDAV(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) {
	switch r.Method {
	case methodPropfind, http.MethodOptions:
	default:
		if !s.sameSite(set, w, r) {
			return
		}
	}
	if !s.davChecked(set, w, r, target, user) {
		return
	}

	if r.Method == http.MethodPut {
		// the limits of handlePut, for the same reasons: a body that says it
		// is too large is turned away unread, and a slow one is cut off only
		// when it stalls
		if set.cfg.MaxUploadSize > 0 && r.ContentLength > set.cfg.MaxUploadSize {
			s.tooLarge(set, w, target)
			return
		}
		if set.cfg.ReadTimeout > 0 {
			r.Body = &idleUploadBody{
				ReadCloser: r.Body,
				controller: http.NewResponseController(w),
				idle:       seconds(set.cfg.ReadTimeout),
			}
		}
		if set.cfg.MaxUploadSize > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, set.cfg.MaxUploadSize)
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, davBodyLimit)
	}
	body := &davBody{ReadCloser: r.Body}
	r.Body = body

	handler := &webdav.Handler{
		FileSystem: &davFS{s: s, set: set, user: user, r: r, body: body},
		LockSystem: s.dav.locks,
		Logger: func(r *http.Request, err error) {
			if err != nil {
				s.log.Debug("webdav request failed", "user", nameOf(user), "method", r.Method,
					"path", r.URL.Path, "address", clientAddress(set, r), "error", err)
			}
		},
	}
	handler.ServeHTTP(w, r)
}

// davChecked decides whether the account may do what a WebDAV request asks,
// before anything is done, and answers the request when it may not. The file
// system checks every path again as it goes; this is where a refusal gets a
// 403 rather than whatever status the WebDAV package maps an error to, and
// where a MOVE or a COPY that removes what it overwrites is checked whole,
// since a refusal halfway would come after the removal.
func (s *Server) davChecked(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) bool {
	refuse := func(path string) bool {
		s.log.Debug("webdav request not allowed for the account",
			"user", nameOf(user), "method", r.Method, "path", path)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	switch r.Method {
	case http.MethodOptions:
		return true
	case methodPropfind:
		// RFC 4918 9.1 lets a server refuse an infinite depth, which a
		// request without a Depth asks for; Explorer and Finder send 0 or 1,
		// and anything deeper walks the whole tree for one request
		if depth := r.Header.Get("Depth"); depth != "0" && depth != "1" {
			s.log.Debug("webdav propfind refused, the depth is infinite", "path", target.Virtual)
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+
				`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
			return false
		}
		return s.permits(set, user, http.MethodGet, target.Virtual, actRead) || refuse(target.Virtual)
	case methodProppatch, methodUnlock:
		// a PROPPATCH is taken and nothing of it is kept, and an UNLOCK only
		// gives back what a LOCK took, so neither needs more than seeing
		return s.permits(set, user, http.MethodGet, target.Virtual, actRead) || refuse(target.Virtual)
	case http.MethodPut, methodLock:
		// a LOCK of a name that is free creates an empty file there, which is
		// how Finder and Office claim a file they are about to write
		if target.IsRoot() {
			return refuse(target.Virtual)
		}
		return s.permits(set, user, http.MethodPut, target.Virtual, davWriteAction(user, target.Path)) ||
			refuse(target.Virtual)
	case http.MethodDelete:
		if target.IsRoot() {
			return refuse(target.Virtual)
		}
		if _, err := os.Lstat(target.Path); err != nil {
			// what is not there is the WebDAV package's to answer
			return true
		}
		if err := s.davRemovable(set, user, target); err != nil {
			return refuse(target.Virtual)
		}
		return true
	case methodMove, methodCopy:
		return s.davCheckedTransfer(set, w, r, target, user, refuse)
	}
	return true
}

// davCheckedTransfer is davChecked for MOVE and COPY, which name a second
// path in their Destination.
func (s *Server) davCheckedTransfer(set *settings, w http.ResponseWriter, r *http.Request,
	target vfs.Target, user *account, refuse func(string) bool) bool {
	destination, ok := s.davDestination(r)
	if !ok {
		s.log.Debug("webdav destination refused", "method", r.Method, "from", target.Virtual,
			"destination", r.Header.Get("Destination"))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return false
	}
	if target.IsRoot() || destination.IsRoot() {
		return refuse(destination.Virtual)
	}
	source, err := os.Lstat(target.Path)
	if err != nil {
		// what is not there is the WebDAV package's to answer
		return true
	}

	fold := s.root.CaseInsensitive()
	if davOverlap(fold, target.Virtual, destination.Virtual) {
		// the WebDAV package removes what a transfer overwrites before it
		// starts, so a destination that is the source, or holds it, or lies
		// in it, would take the source with it. Renaming a file to another
		// spelling of its own name on a filesystem that ignores case is the
		// one such request that means something, and it is done here.
		if r.Method == methodMove && fold && target.Virtual != destination.Virtual &&
			strings.EqualFold(target.Virtual, destination.Virtual) {
			s.davRenameCase(set, w, r, target, destination, user)
			return false
		}
		s.log.Debug("webdav transfer onto itself refused", "method", r.Method,
			"from", target.Virtual, "to", destination.Virtual)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}

	var overwrite bool
	if r.Method == methodMove {
		// the WebDAV package's reading of the header, which is the one that
		// decides what it does
		overwrite = r.Header.Get("Overwrite") == "T"
		if err := s.davMovable(set, user, target, destination); err != nil {
			return refuse(target.Virtual)
		}
	} else {
		overwrite = r.Header.Get("Overwrite") != "F"
		if !s.permits(set, user, http.MethodGet, target.Virtual, actRead) {
			return refuse(target.Virtual)
		}
		if err := s.davCopyable(set, user, target, destination, source.IsDir()); err != nil {
			return refuse(destination.Virtual)
		}
	}
	if _, err := os.Lstat(destination.Path); err == nil && overwrite {
		if err := s.davRemovable(set, user, destination); err != nil {
			return refuse(destination.Virtual)
		}
	}
	return true
}

// davDestination resolves the Destination header of a MOVE or a COPY, which
// has to name a path of this server.
func (s *Server) davDestination(r *http.Request) (vfs.Target, bool) {
	header := r.Header.Get("Destination")
	if header == "" {
		return vfs.Target{}, false
	}
	parsed, err := url.Parse(header)
	if err != nil || parsed.Path == "" || (parsed.Host != "" && parsed.Host != r.Host) {
		return vfs.Target{}, false
	}
	destination := s.root.Resolve("/", parsed.Path)
	return destination, destination.Valid
}

// davOverlap reports whether two paths are one, or one lies inside the other.
func davOverlap(fold bool, a, b string) bool {
	if fold {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	return a == b || strings.HasPrefix(a, vfs.AsFolder(b)) || strings.HasPrefix(b, vfs.AsFolder(a))
}

// davRenameCase gives a file or a folder another spelling of its own name.
func (s *Server) davRenameCase(set *settings, w http.ResponseWriter, r *http.Request,
	from, to vfs.Target, user *account) {
	release, ok := s.davHold(set, w, from.Virtual)
	if !ok {
		return
	}
	defer release()
	if err := os.Rename(from.Path, to.Path); err != nil {
		s.log.Error("webdav move failed", "from", from.Virtual, "to", to.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	s.log.Info("webdav move", "user", nameOf(user), "from", from.Virtual, "to", to.Virtual,
		"address", clientAddress(set, r))
	w.WriteHeader(http.StatusCreated)
}

// davWriteAction is the right writing a file needs: creating it where the
// name is free, replacing it where it is taken.
//
// An empty file counts as a free name for an account. Explorer and Finder
// both create a file with an empty PUT, and lock it, before they send what is
// in it, so an account that may create files and not replace them could
// otherwise copy nothing but empty files onto the drive. A request without an
// account is held to the promise every public PUT keeps, that it never
// replaces what is there.
func davWriteAction(user *account, osPath string) action {
	info, err := os.Stat(osPath)
	if err != nil || (user != nil && info.Mode().IsRegular() && info.Size() == 0) {
		return actCreate
	}
	return actOverwrite
}

// davWalk calls visit with the virtual path of target and of everything in it.
func davWalk(target vfs.Target, visit func(virtual string, folder bool) error) error {
	return filepath.WalkDir(target.Path, func(osPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(target.Path, osPath)
		if err != nil {
			return err
		}
		return visit(path.Join(target.Virtual, filepath.ToSlash(rel)), entry.IsDir())
	})
}

// davRemovable reports whether the account may remove target and everything
// in it, which is what a recursive DELETE needs: the right to delete each
// file and each folder, at a path it may reach.
func (s *Server) davRemovable(set *settings, user *account, target vfs.Target) error {
	return davWalk(target, func(virtual string, folder bool) error {
		act := actDeleteFile
		if folder {
			act = actDeleteFolder
		}
		if !s.permits(set, user, http.MethodDelete, virtual, act) {
			return &os.PathError{Op: "remove", Path: virtual, Err: os.ErrPermission}
		}
		return nil
	})
}

// davMovable reports whether the account may move from to to: rename each
// thing in it, both at the path it leaves and at the one it lands on.
func (s *Server) davMovable(set *settings, user *account, from, to vfs.Target) error {
	return davWalk(from, func(virtual string, _ bool) error {
		landing := path.Join(to.Virtual, strings.TrimPrefix(virtual, from.Virtual))
		if !s.permits(set, user, methodMove, virtual, actRename) ||
			!s.permits(set, user, methodMove, landing, actRename) {
			return &os.PathError{Op: "rename", Path: virtual, Err: os.ErrPermission}
		}
		return nil
	})
}

// davCopyable reports whether the account may create each copy a COPY of
// from makes under to. What it cannot see is left out of the copy rather
// than refused, as it is left out of a listing.
func (s *Server) davCopyable(set *settings, user *account, from, to vfs.Target, folder bool) error {
	if !folder {
		if !s.permits(set, user, http.MethodPut, to.Virtual, actCreate) {
			return &os.PathError{Op: "copy", Path: to.Virtual, Err: os.ErrPermission}
		}
		return nil
	}
	return davWalk(from, func(virtual string, folder bool) error {
		if !s.permits(set, user, http.MethodGet, virtual, actRead) {
			if folder {
				return filepath.SkipDir
			}
			return nil
		}
		landing := path.Join(to.Virtual, strings.TrimPrefix(virtual, from.Virtual))
		method, act := http.MethodPut, actCreate
		if folder {
			method, act = methodMkcol, actMkdir
		}
		if !s.permits(set, user, method, landing, act) {
			return &os.PathError{Op: "copy", Path: landing, Err: os.ErrPermission}
		}
		return nil
	})
}

// davHold claims paths a request of the page or of a script is about to
// change, against the locks WebDAV clients hold, until release is called. A
// path one of them has locked is answered 423, so a file Finder has open is
// not written under it. It is the claim the WebDAV package makes for a
// request of its own that holds no lock.
func (s *Server) davHold(set *settings, w http.ResponseWriter, virtuals ...string) (release func(), ok bool) {
	if !set.cfg.WebDAV {
		return func() {}, true
	}
	now := time.Now()
	tokens := make([]string, 0, len(virtuals))
	release = func() {
		for _, token := range tokens {
			_ = s.dav.locks.Unlock(now, token)
		}
	}
	for _, virtual := range virtuals {
		token, err := s.dav.locks.Create(now, webdav.LockDetails{Root: virtual, Duration: -1, ZeroDepth: true})
		if err != nil {
			release()
			if err == webdav.ErrLocked {
				s.log.Debug("http request refused, a webdav client holds a lock", "path", virtual)
				http.Error(w, "Locked", webdav.StatusLocked)
				return nil, false
			}
			s.log.Error("http cannot check the webdav locks", "path", virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return nil, false
		}
		tokens = append(tokens, token)
	}
	return release, true
}

// davBody remembers that reading the request body failed. The WebDAV package
// closes the file a PUT writes whether or not the body arrived whole, so
// closing is where an upload is committed, and this is how it knows not to.
type davBody struct {
	io.ReadCloser
	failed error
}

func (b *davBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.failed == nil {
		b.failed = err
	}
	return n, err
}
