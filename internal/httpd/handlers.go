package httpd

import (
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go-fs/internal/vfs"
)

// handleGet serves a file as a download and a folder as the browsable listing.
func (s *Server) handleGet(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target) {
	info, err := os.Stat(target.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if info.IsDir() {
		entries, err := readDirectory(target.Path)
		if err != nil {
			s.log.Error("http cannot read the folder", "path", target.Virtual, "error", err)
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(listingPage(vfs.AsFolder(target.Virtual), entries))
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
	w.Header().Set("Content-Disposition", "attachment; filename="+quoted(name))
	w.Header().Set("Content-Type", typeOf(name).Media)
	// ServeContent adds Content-Length and, unlike the Node implementation,
	// answers a range request, so a large download can be resumed
	http.ServeContent(w, r, name, info.ModTime(), file)

	s.log.Info("http download", "file", target.Virtual, "bytes", info.Size(),
		"address", addressOf(r))
}

// handlePut stores an uploaded file. A body of octet-stream is the file; a
// multipart body carries it in a part. Folders above it are created, and a
// target that already exists is refused, as in the Node implementation.
func (s *Server) handlePut(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target) {
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

	body := io.Reader(r.Body)
	if set.cfg.MaxUploadSize > 0 {
		body = http.MaxBytesReader(w, r.Body, set.cfg.MaxUploadSize)
	}

	if multipart {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
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

	written, err := s.store(target.Path, body)
	if err != nil {
		_ = os.Remove(target.Path)
		s.log.Error("http upload failed", "file", target.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}

	s.log.Info("http upload", "file", target.Virtual, "bytes", written, "address", addressOf(r))
	w.WriteHeader(http.StatusOK)
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
func (s *Server) handleDelete(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target) {
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

	s.log.Info("http delete", "path", target.Virtual, "address", addressOf(r))
	w.WriteHeader(http.StatusOK)
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

// quoted wraps a filename for a header, escaping what would end the value.
func quoted(name string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(name) + `"`
}
