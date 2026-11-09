package httpd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/mp3edit"
	"github.com/mailsvb/go-fs/internal/vfs"
)

// The listing edits the tags of an MP3 file and changes its volume, one file
// from its menu or every MP3 of the folder from the header, where the account
// may replace the file (see rights.MP3Tags). The editor asks with a marker on
// the file, of the served tree or of a stored server:
//
//	<file>?go-fs=mp3tags&mp3tags=<part>
//	/?go-fs=remote&server=<name>&path=<file>&mp3tags=<part>
//
// GET of the part tags reads the fields, the cover and the volume information,
// GET of cover the cover image; POST of tags saves an edit, POST of analyze
// measures the file's loudness, and POST of album, on the folder, plans one
// change for the files it names.

const (
	actionMP3Tags = "mp3tags"
	// mp3PartParam names what of the editor a request is for.
	mp3PartParam = "mp3tags"

	mp3PartTags    = "tags"
	mp3PartCover   = "cover"
	mp3PartAnalyze = "analyze"
	mp3PartAlbum   = "album"

	// maxMP3Body is the largest edit taken: a cover of mp3edit.MaxCover in
	// base64, and the fields.
	maxMP3Body = mp3edit.MaxCover/3*4 + 1<<20
)

// mp3tagsAction reports a request of the editor on the served tree.
func mp3tagsAction(r *http.Request) bool {
	return r.URL.Query().Get(sessionParam) == actionMP3Tags
}

// mp3Part is the part of the editor a request asks for, and whether that
// part answers its method; "" for one that does not exist.
func mp3Part(r *http.Request) (part string, allowed bool) {
	part = r.URL.Query().Get(mp3PartParam)
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch part {
	case mp3PartTags:
		return part, read || r.Method == http.MethodPost
	case mp3PartCover:
		return part, read
	case mp3PartAnalyze, mp3PartAlbum:
		return part, r.Method == http.MethodPost
	}
	return "", false
}

// mp3Methods are the methods each part answers.
var mp3Methods = map[string]string{
	mp3PartTags:    "GET, HEAD, POST",
	mp3PartCover:   "GET, HEAD",
	mp3PartAnalyze: "POST",
	mp3PartAlbum:   "POST",
}

// mp3Target is what the editor works on, wherever it is.
type mp3Target struct {
	// label is the file or folder as the log names it.
	label   string
	user    string
	address string
	isDir   bool
	// source is the file, for every part but album.
	source mp3edit.Source
	// member is a file of the folder, for album.
	member func(name string) (mp3edit.Source, error)
	// hold claims the file against the locks of WebDAV clients while it is
	// saved.
	hold func(w http.ResponseWriter) (release func(), ok bool)
}

// handleMP3Tags answers the editor on the served tree.
func (s *Server) handleMP3Tags(set *settings, w http.ResponseWriter, r *http.Request, target vfs.Target, cred credential) {
	w.Header().Set("Cache-Control", "no-store")
	part, allowed := mp3Part(r)
	if !mp3edit.Compiled || part == "" {
		http.NotFound(w, r)
		return
	}
	if !allowed {
		w.Header().Set("Allow", mp3Methods[part])
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !read && !s.sameSite(set, w, r) {
		return
	}
	user := cred.user
	// reading what the editor shows takes reading the file; everything else
	// is offered to who may replace it, and measuring keeps a core busy
	may := s.permits(set, user, http.MethodGet, target.Virtual, actRead)
	if !read {
		may = may && s.permits(set, user, http.MethodPut, target.Virtual, actOverwrite)
	}
	if !may {
		s.log.Debug("http mp3 tags not allowed for the account", "user", nameOf(user),
			"part", part, "path", target.Virtual)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Stat(target.Path)
	if err != nil || (part == mp3PartAlbum) != info.IsDir() ||
		(!info.IsDir() && (!info.Mode().IsRegular() || !mp3edit.IsMP3(info.Name()))) {
		http.NotFound(w, r)
		return
	}
	t := mp3Target{label: target.Virtual, user: nameOf(user), address: clientAddress(set, r), isDir: info.IsDir()}
	if t.isDir {
		folder := vfs.AsFolder(target.Virtual)
		t.member = func(name string) (mp3edit.Source, error) {
			member := s.root.Resolve(folder, name)
			if !member.Valid || strings.Contains(name, "/") || !mp3edit.IsMP3(name) ||
				!s.permits(set, user, http.MethodPut, member.Virtual, actOverwrite) {
				return mp3edit.Source{}, fs.ErrNotExist
			}
			found, err := os.Stat(member.Path)
			if err != nil || !found.Mode().IsRegular() {
				return mp3edit.Source{}, fs.ErrNotExist
			}
			return localMP3(member.Path, found), nil
		}
	} else {
		t.source = localMP3(target.Path, info)
		t.hold = func(w http.ResponseWriter) (func(), bool) { return s.davHold(set, w, target.Virtual) }
	}
	s.serveMP3(w, r, part, t)
}

// localMP3 is a file of the served tree.
func localMP3(file string, info fs.FileInfo) mp3edit.Source {
	return mp3edit.Source{
		Key:     "file\x00" + file,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Path:    file,
		Open: func() (io.ReaderAt, io.Closer, error) {
			f, err := os.Open(file)
			if err != nil {
				return nil, nil, err
			}
			return f, f, nil
		},
		Stat: func() (int64, time.Time, error) {
			return statOf(os.Stat(file))
		},
	}
}

// statOf is the size and the time of change of a file a stat found.
func statOf(info fs.FileInfo, err error) (int64, time.Time, error) {
	if err != nil {
		return 0, time.Time{}, err
	}
	return info.Size(), info.ModTime(), nil
}

// remoteMP3Tags answers the editor on a stored server, once it was logged in
// to: reading takes the server's downloads, the rest its uploads too and the
// account's right to replace a file.
func (s *Server) remoteMP3Tags(set *settings, w http.ResponseWriter, r *http.Request, q *remoteRequest) {
	w.Header().Set("Cache-Control", "no-store")
	part, allowed := mp3Part(r)
	if !mp3edit.Compiled || part == "" {
		http.NotFound(w, r)
		return
	}
	if !allowed {
		w.Header().Set("Allow", mp3Methods[part])
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	server := q.server.Rights()
	if !server.Download {
		s.remoteRefused(w, q, "download")
		return
	}
	if !read && !server.Upload {
		s.remoteRefused(w, q, "upload")
		return
	}
	if !read && !granted(q.user.perms, actOverwrite) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if part != mp3PartAlbum && (q.path == "" || q.path == "/") {
		http.NotFound(w, r)
		return
	}
	p, err := q.fs.Resolve(q.path)
	if err != nil {
		http.Error(w, remoteFolderFailure(sendRequest{Login: q.login}, q.path, err).Error(), http.StatusBadGateway)
		return
	}
	info, err := q.fs.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, remoteFolderFailure(sendRequest{Login: q.login}, p, err).Error(), http.StatusBadGateway)
		return
	case (part == mp3PartAlbum) != info.IsDir(),
		!info.IsDir() && (!info.Mode().IsRegular() || !mp3edit.IsMP3(p)):
		http.NotFound(w, r)
		return
	}
	t := mp3Target{label: q.logged(p), user: q.user.name, address: q.address, isDir: info.IsDir()}
	if t.isDir {
		t.member = func(name string) (mp3edit.Source, error) {
			if name == "" || strings.Contains(name, "/") || name == "." || name == ".." || !mp3edit.IsMP3(name) {
				return mp3edit.Source{}, fs.ErrNotExist
			}
			file := path.Join(p, name)
			found, err := q.fs.Stat(file)
			if err != nil {
				return mp3edit.Source{}, err
			}
			if !found.Mode().IsRegular() {
				return mp3edit.Source{}, fs.ErrNotExist
			}
			return remoteMP3(q, file, found), nil
		}
	} else {
		t.source = remoteMP3(q, p, info)
		t.hold = func(http.ResponseWriter) (func(), bool) { return func() {}, true }
	}
	s.serveMP3(w, r, part, t)
}

// remoteMP3 is a file of a stored server, which a save uploads whole.
func remoteMP3(q *remoteRequest, file string, info fs.FileInfo) mp3edit.Source {
	return mp3edit.Source{
		Key:     "server\x00" + q.server.Name + "\x00" + file,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Open: func() (io.ReaderAt, io.Closer, error) {
			f, err := q.fs.Open(file)
			if err != nil {
				return nil, nil, err
			}
			return readerAtOf(f), f, nil
		},
		Put: func(body io.Reader) error {
			_, err := q.fs.Put(file, body, -1)
			return err
		},
		Stat: func() (int64, time.Time, error) {
			return statOf(q.fs.Stat(file))
		},
	}
}

// readerAtOf reads a file at offsets: the file itself where it can, as the
// files of SFTP and SMB can, and by seeking it where it cannot.
func readerAtOf(f io.ReadSeeker) io.ReaderAt {
	if at, ok := f.(io.ReaderAt); ok {
		return at
	}
	return &seekingReaderAt{file: f}
}

type seekingReaderAt struct {
	mu   sync.Mutex
	file io.ReadSeeker
}

func (s *seekingReaderAt) ReadAt(b []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.file.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s.file, b)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

// mp3InfoJSON is what the editor reads of a file.
type mp3InfoJSON struct {
	mp3edit.Info
	// Modified is when the file was changed, which a save names in
	// If-Unmodified-Since.
	Modified string `json:"modified"`
}

// mp3EditJSON is a save.
type mp3EditJSON struct {
	Fields *mp3edit.Fields `json:"fields"`
	Cover  *struct {
		Remove bool `json:"remove"`
		// Data is a new image, in base64.
		Data string `json:"data"`
	} `json:"cover"`
	Gain struct {
		Steps      int                 `json:"steps"`
		ReplayGain bool                `json:"replayGain"`
		Album      *mp3edit.AlbumLevel `json:"album"`
	} `json:"gain"`
}

// mp3AlbumJSON names the files of an album, in the folder asked on.
type mp3AlbumJSON struct {
	Names []string `json:"names"`
}

// serveMP3 answers a part of the editor once the request was found allowed.
func (s *Server) serveMP3(w http.ResponseWriter, r *http.Request, part string, t mp3Target) {
	switch {
	case part == mp3PartTags && r.Method != http.MethodPost:
		info, err := mp3edit.Read(t.source)
		if err != nil {
			s.mp3Failure(w, t, "read", err)
			return
		}
		w.Header().Set("Last-Modified", t.source.ModTime.UTC().Format(http.TimeFormat))
		writeJSON(w, http.StatusOK, mp3InfoJSON{Info: info, Modified: t.source.ModTime.UTC().Format(http.TimeFormat)})
	case part == mp3PartCover:
		data, mime, err := mp3edit.CoverOf(t.source)
		if err != nil {
			s.mp3Failure(w, t, "read", err)
			return
		}
		if data == nil || !strings.HasPrefix(mime, "image/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", t.source.ModTime, bytes.NewReader(data))
	case part == mp3PartAnalyze:
		started := time.Now()
		analysis, err := mp3edit.Analyze(t.source)
		if err != nil {
			s.mp3Failure(w, t, "analyze", err)
			return
		}
		s.log.Debug("http mp3 analyzed", "user", t.user, "file", t.label, "steps", analysis.Track.Steps,
			"address", t.address, "took", time.Since(started).Round(time.Millisecond))
		writeJSON(w, http.StatusOK, analysis)
	case part == mp3PartAlbum:
		var asked mp3AlbumJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&asked); err != nil || len(asked.Names) == 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		sources := make([]mp3edit.Source, 0, len(asked.Names))
		for _, name := range asked.Names {
			src, err := t.member(name)
			if err != nil {
				s.mp3Failure(w, t, "read", err)
				return
			}
			sources = append(sources, src)
		}
		album, err := mp3edit.PlanAlbum(sources)
		if err != nil {
			s.mp3Failure(w, t, "analyze", err)
			return
		}
		writeJSON(w, http.StatusOK, album)
	default:
		s.saveMP3(w, r, t)
	}
}

// saveMP3 writes an edit to a file that was not changed since the editor
// read it.
func (s *Server) saveMP3(w http.ResponseWriter, r *http.Request, t mp3Target) {
	var asked mp3EditJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMP3Body)).Decode(&asked); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, mp3edit.ErrCover.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	edit := mp3edit.Edit{Fields: asked.Fields, Steps: asked.Gain.Steps, ReplayGain: asked.Gain.ReplayGain,
		Album: asked.Gain.Album}
	if c := asked.Cover; c != nil {
		switch {
		case c.Data != "":
			data, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			edit.Cover = &mp3edit.NewCover{Data: data}
		case c.Remove:
			edit.RemoveCover = true
		}
	}
	if changedSince(r, t.source.ModTime) {
		s.log.Info("http mp3 tags refused, the file changed since it was read", "user", t.user,
			"file", t.label, "address", t.address)
		http.Error(w, "The file was changed since it was opened.", http.StatusPreconditionFailed)
		return
	}
	release, ok := t.hold(w)
	if !ok {
		return
	}
	defer release()
	started := time.Now()
	if err := mp3edit.Save(t.source, edit); err != nil {
		s.mp3Failure(w, t, "save", err)
		return
	}
	s.log.Info("http mp3 tags", "user", t.user, "file", t.label, "steps", edit.Steps,
		"address", t.address, "took", time.Since(started).Round(time.Millisecond))
	w.WriteHeader(http.StatusNoContent)
}

// mp3Failure answers what went wrong with a file of the editor.
func (s *Server) mp3Failure(w http.ResponseWriter, t mp3Target, what string, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, mp3edit.ErrOutOfRange):
		status = http.StatusConflict
	case errors.Is(err, mp3edit.ErrCover):
		status = http.StatusUnsupportedMediaType
	case errors.Is(err, mp3edit.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, mp3edit.ErrNotMP3):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	default:
		s.log.Warn("http mp3 "+what+" failed", "user", t.user, "file", t.label, "address", t.address,
			"error", err)
		http.Error(w, "The file could not be "+map[string]string{
			"read": "read", "analyze": "measured", "save": "saved"}[what]+": "+err.Error(), status)
		return
	}
	http.Error(w, err.Error(), status)
}
