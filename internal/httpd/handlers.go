package httpd

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go-fs/internal/vfs"
)

// handleGet serves a file as a download and a folder as the browsable listing.
func (s *Server) handleGet(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, cred credential) {
	user := cred.user
	info, err := os.Stat(target.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if info.IsDir() {
		// The links in a listing are relative, so a folder has to be reached
		// with a trailing slash for them to point inside it: without this,
		// /photos would link to /sub/ instead of /photos/sub/.
		if !strings.HasSuffix(r.URL.Path, "/") {
			redirect := *r.URL
			redirect.Path += "/"
			http.Redirect(w, r, redirect.RequestURI(), http.StatusMovedPermanently)
			return
		}
		entries, err := readDirectory(target.Path)
		if err != nil {
			s.log.Error("http cannot read the folder", "path", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
		nonce, err := pageNonce()
		if err != nil {
			s.log.Error("http cannot render the listing", "path", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
		page, err := listingPage(vfs.AsFolder(target.Virtual), entries,
			parseSort(r.URL.Query()), s.rightsFor(set, user, target.Virtual),
			sessionViewFor(set, r, cred), nonce)
		if err != nil {
			s.log.Error("http cannot render the listing", "path", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Content-Security-Policy", contentPolicy(nonce))
		// the page says who is signed in, so a shared cache must not hand one
		// browser's copy to another
		w.Header().Set("Vary", "Cookie")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(page)
		}
		return
	}
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(target.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()

	name := filepath.Base(target.Path)
	w.Header().Set("Content-Disposition", disposition(name))
	w.Header().Set("Content-Type", typeOf(name).Media)
	// ServeContent adds Content-Length and, unlike the Node implementation,
	// answers a range request, so a large download can be resumed
	http.ServeContent(w, r, name, info.ModTime(), file)

	s.log.Info("http download", "user", nameOf(user), "file", target.Virtual,
		"bytes", info.Size(), "address", addressOf(r))
}

// handlePut stores an uploaded file. A body of octet-stream is the file; a
// multipart body carries it in a part. Folders above it are created, and a
// target that already exists is refused, as in the Node implementation.
func (s *Server) handlePut(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) {
	if _, err := os.Stat(target.Path); err == nil {
		// the original falls through to its not-found handler here
		http.NotFound(w, r)
		return
	}

	contentType := r.Header.Get("Content-Type")
	binary := strings.Contains(contentType, "application/octet-stream")
	multipart := strings.Contains(contentType, "multipart")
	if !binary && !multipart {
		http.NotFound(w, r)
		return
	}

	if err := os.MkdirAll(filepath.Dir(target.Path), 0o755); err != nil {
		s.log.Error("http cannot create the folder", "path", target.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}

	// maxUploadSize is a limit on the file, so the raw body is allowed the
	// wrapping a multipart request puts around it — otherwise the same file
	// would be accepted sent as octet-stream and refused sent as a form.
	//
	// The cap goes on the request body itself rather than on a reader derived
	// from it, because the multipart parser below reads r.Body and would
	// otherwise take a body of any size; what actually reaches the file is
	// counted again further down. A client that declares its length is turned
	// away before any of it is read.
	var limited *limitedBody
	if set.cfg.MaxUploadSize > 0 {
		bodyLimit := set.cfg.MaxUploadSize
		if multipart {
			bodyLimit += multipartEnvelope
		}
		if r.ContentLength > bodyLimit {
			s.tooLarge(set, w, target)
			return
		}
		limited = &limitedBody{ReadCloser: http.MaxBytesReader(w, r.Body, bodyLimit)}
		r.Body = limited
	}
	body := io.Reader(r.Body)

	if multipart {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			// a body cut short by the limit reaches the parser as a truncated
			// one, which it reports as malformed; what the client actually did
			// is send too much, and that is what it is told
			if tooLarge(err) || limited.hit() {
				s.tooLarge(set, w, target)
				return
			}
			s.log.Debug("http cannot read the upload", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		part, _, err := r.FormFile("file")
		if err != nil {
			// the field is called file by the original, but any single file
			// part is taken rather than failing on the name
			part, err = firstFilePart(r)
			if err != nil {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
		}
		defer func() { _ = part.Close() }()
		body = part
	}

	if set.cfg.MaxUploadSize > 0 {
		// one byte past the limit is enough to know it was passed
		body = io.LimitReader(body, set.cfg.MaxUploadSize+1)
	}
	written, err := s.store(target.Path, body)
	if err == nil && set.cfg.MaxUploadSize > 0 && written > set.cfg.MaxUploadSize {
		_ = os.Remove(target.Path)
		s.tooLarge(set, w, target)
		return
	}
	if err != nil {
		// nothing half written is left behind, whatever went wrong
		_ = os.Remove(target.Path)
		if tooLarge(err) || limited.hit() {
			s.tooLarge(set, w, target)
			return
		}
		s.log.Error("http upload failed", "file", target.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}

	s.log.Info("http upload", "user", nameOf(user), "file", target.Virtual,
		"bytes", written, "address", addressOf(r))
	w.WriteHeader(http.StatusOK)
}

// disposition builds the Content-Disposition of a download.
//
// The plain filename is quoted, which is what every client reads, and it is
// kept to printable ASCII so that the header says what it means whatever the
// file is called. A name that needed changing to fit there is carried beside it
// in the RFC 5987 form, which is how a client that understands it gets the
// real name back.
func disposition(name string) string {
	plain := make([]rune, 0, len(name))
	exact := true
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7F:
			// a control character has no business in a header
			exact = false
		case r > 0x7F:
			exact = false
			plain = append(plain, '_')
		default:
			plain = append(plain, r)
		}
	}
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(string(plain))
	header := `attachment; filename="` + quoted + `"`
	if exact {
		return header
	}
	return header + "; filename*=UTF-8''" + url.PathEscape(name)
}

// multipartEnvelope is how much of a multipart body is not the file: the
// boundaries and the part headers. It is generous — a single file part needs a
// few hundred bytes — because it is an allowance, not a limit of its own.
const multipartEnvelope = 8 << 10

// tooLarge reports the error MaxBytesReader produces, whether it surfaced from
// the multipart parser or from the copy.
func tooLarge(err error) bool {
	var limit *http.MaxBytesError
	return errors.As(err, &limit)
}

// limitedBody remembers that the limit was reached, because the reader that
// hits it is not always the one that reports it: the multipart parser buffers,
// so it sees a body that stops mid-header and calls that malformed rather than
// passing on the error underneath.
type limitedBody struct {
	io.ReadCloser
	exceeded bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if tooLarge(err) {
		b.exceeded = true
	}
	return n, err
}

// hit is safe on a nil receiver, which is what an upload with no limit has.
func (b *limitedBody) hit() bool {
	return b != nil && b.exceeded
}

// tooLarge answers an upload above http.maxUploadSize with the status that
// says so, rather than reporting a server error for what the client did.
func (s *Server) tooLarge(set *settings, w http.ResponseWriter, target vfs.Target) {
	s.log.Debug("http upload exceeds the maximum size",
		"file", target.Virtual, "maxUploadSize", set.cfg.MaxUploadSize)
	http.Error(w, fmt.Sprintf("the upload is larger than the maximum of %d bytes",
		set.cfg.MaxUploadSize), http.StatusRequestEntityTooLarge)
}

// store writes a body to its final name.
func (s *Server) store(osPath string, body io.Reader) (int64, error) {
	file, err := os.OpenFile(osPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(file, body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return written, err
}

// firstFilePart takes whatever file part the request carries, whatever the
// field is called.
func firstFilePart(r *http.Request) (multipartFile, error) {
	for _, parts := range r.MultipartForm.File {
		for _, header := range parts {
			return header.Open()
		}
	}
	return nil, http.ErrMissingFile
}

type multipartFile interface {
	io.ReadCloser
	io.ReaderAt
	io.Seeker
}

// handleDelete removes a file, or a folder when it is empty.
func (s *Server) handleDelete(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) {
	if target.IsRoot() {
		// removing the served folder would take every path with it
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(target.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case info.Mode().IsRegular():
		if err := os.Remove(target.Path); err != nil {
			s.log.Error("http delete failed", "file", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
	case info.IsDir():
		entries, err := os.ReadDir(target.Path)
		if err != nil || len(entries) > 0 {
			// a folder with anything in it is not removed, as in the original
			http.NotFound(w, r)
			return
		}
		if err := os.Remove(target.Path); err != nil {
			s.log.Error("http delete failed", "folder", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}

	s.log.Info("http delete", "user", nameOf(user), "path", target.Virtual,
		"address", addressOf(r))
	w.WriteHeader(http.StatusOK)
}

// handleMkcol creates a folder, which is the one thing PUT cannot do: it makes
// the folders above a file, so there is no way to ask it for an empty one.
func (s *Server) handleMkcol(w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) {
	if r.ContentLength != 0 {
		// RFC 4918: a body here describes something this server does not know
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}
	if target.IsRoot() {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := os.Stat(target.Path); err == nil {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// deliberately not MkdirAll: a typo in the folder above should be an error
	// rather than a tree nobody asked for
	if err := os.Mkdir(target.Path, 0o755); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "Conflict", http.StatusConflict)
			return
		}
		s.log.Error("http mkdir failed", "folder", target.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}

	s.log.Info("http mkdir", "user", nameOf(user), "folder", target.Virtual,
		"address", addressOf(r))
	w.WriteHeader(http.StatusCreated)
}

// handleMove renames a file or a folder in place.
func (s *Server) handleMove(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) {
	if target.IsRoot() {
		// renaming the served folder would take every path with it
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(target.Path); err != nil {
		http.NotFound(w, r)
		return
	}

	destination, ok := s.destinationOf(set, w, r, target, user)
	if !ok {
		return
	}
	if _, err := os.Stat(destination.Path); err == nil {
		// the same refusal PUT makes for a name that is taken, with the status
		// that says which of the two paths was the problem — a bare 404 here
		// cannot be told apart from a source that is not there
		http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
		return
	}

	if err := os.Rename(target.Path, destination.Path); err != nil {
		s.log.Error("http rename failed", "from", target.Virtual,
			"to", destination.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}

	s.log.Info("http rename", "user", nameOf(user), "from", target.Virtual,
		"to", destination.Virtual, "address", addressOf(r))
	w.WriteHeader(http.StatusNoContent)
}

// destinationOf resolves the Destination header of a MOVE.
//
// The header may be an absolute URL or a path, and only the path is read. The
// result has to name something in the same folder: what this offers is a
// rename, and accepting a destination anywhere else would quietly make it a
// move API with a reach nothing here checks for.
func (s *Server) destinationOf(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, user *account) (vfs.Target, bool) {
	header := r.Header.Get("Destination")
	if header == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return vfs.Target{}, false
	}
	parsed, err := url.Parse(header)
	if err != nil || parsed.Path == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return vfs.Target{}, false
	}

	destination := s.root.Resolve("/", parsed.Path)
	if !destination.Valid || destination.IsRoot() {
		s.log.Debug("http rename destination refused", "destination", parsed.Path)
		http.NotFound(w, r)
		return vfs.Target{}, false
	}
	if folderOf(destination.Virtual) != folderOf(target.Virtual) {
		s.log.Debug("http rename crosses folders", "from", target.Virtual,
			"to", destination.Virtual)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return vfs.Target{}, false
	}
	// where the name lands is checked as well as where it came from, so a
	// rename cannot carry a file out of the scope of the account doing it
	if !s.permits(set, user, methodMove, destination.Virtual) {
		s.log.Debug("http rename destination not allowed for the account",
			"user", nameOf(user), "path", destination.Virtual)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return vfs.Target{}, false
	}
	return destination, true
}

// folderOf is the folder a virtual path sits in.
func folderOf(virtual string) string {
	return path.Dir(strings.TrimSuffix(virtual, "/"))
}

// pageNonce is the one-off value that lets the listing's own style and script
// run under a policy that allows nothing else.
//
// The alphabet is the URL-safe one, which CSP accepts and which survives being
// written into an attribute: standard base64 has a "+" in it, and html/template
// writes that as &#43;, leaving the page and the header naming values that only
// match once a parser has decoded one of them.
func pageNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// contentPolicy closes the listing page off to everything it does not carry
// itself. connect-src has to stay open to this origin: the buttons on the page
// reach the very server that sent it.
//
// form-action is deliberately not set, because the dialogs submit to
// method="dialog", which some browsers check against it even though it never
// leaves the page.
func contentPolicy(nonce string) string {
	return "default-src 'none'; " +
		"style-src 'nonce-" + nonce + "'; " +
		"script-src 'nonce-" + nonce + "'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"base-uri 'none'"
}

// handleDirectoryReader answers the legacy listing endpoint: a form field dir,
// resolved against the folder the request path is in, answered as links.
func (s *Server) handleDirectoryReader(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target) {
	if err := r.ParseForm(); err != nil {
		http.NotFound(w, r)
		return
	}
	folder := r.FormValue("dir")
	if folder == "" {
		s.log.Debug("http directory reader called without a dir")
		http.NotFound(w, r)
		return
	}

	// the request path names the reader script, so the listing is relative to
	// the folder that script sits in
	base := path.Dir(target.Virtual)
	listed := s.root.Resolve(base, folder)
	if !listed.Valid {
		s.log.Debug("http directory reader path refused", "dir", folder)
		http.NotFound(w, r)
		return
	}
	if info, err := os.Stat(listed.Path); err != nil || !info.IsDir() {
		s.log.Debug("http directory reader folder does not exist", "dir", listed.Virtual)
		http.NotFound(w, r)
		return
	}

	entries, err := readDirectory(listed.Path)
	if err != nil {
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(readerPage(folder, entries))
}
