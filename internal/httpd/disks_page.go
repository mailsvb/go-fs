//go:build !light && !no_nbd

package httpd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/mailsvb/go-fs/internal/disks"
)

// The Disks page keeps the disk images of general.baseFolderDisks, which the
// NBD server serves to the VMs that run on them: it makes, resizes, renames,
// clones, uploads, downloads and deletes them, shows who has which attached,
// and disconnects a client. It is served under the go-fs marker like the
// Trash page, and decides for itself who may be there: an account that sets
// isAdmin, with a session. A disk is no file of the served folder, so neither
// the paths nor the rights of an account have anything to say about it.
//
// The page is rendered by its script from the state endpoint, which it asks
// again every few seconds for what changes on its own: the bytes a client
// moved, a clone's progress, a client that came or went.

// diskFolder is the folder of the Disks page.
type diskFolder = *disks.Folder

// openDisks opens the folder of the Disks page; empty is no Disks page.
func openDisks(folder string, log *slog.Logger) (diskFolder, error) {
	if folder == "" {
		return nil, nil
	}
	return disks.Open(folder, log)
}

const (
	actionDisks           = "disks"
	actionDisksState      = "disks-state"
	actionDisksCreate     = "disks-create"
	actionDisksResize     = "disks-resize"
	actionDisksRename     = "disks-rename"
	actionDisksClone      = "disks-clone"
	actionDisksDelete     = "disks-delete"
	actionDisksUpload     = "disks-upload"
	actionDisksDownload   = "disks-download"
	actionDisksDisconnect = "disks-disconnect"
	actionDisksJob        = "disks-job"
)

// disksPageMethods are the methods each endpoint of the page answers.
var disksPageMethods = map[string]string{
	actionDisks:           "GET, HEAD",
	actionDisksState:      "GET, HEAD",
	actionDisksCreate:     "POST",
	actionDisksResize:     "POST",
	actionDisksRename:     "POST",
	actionDisksClone:      "POST",
	actionDisksDelete:     "POST",
	actionDisksUpload:     "PUT",
	actionDisksDownload:   "GET, HEAD",
	actionDisksDisconnect: "POST",
	actionDisksJob:        "POST",
}

// disksPageAction reports which endpoint of the Disks page a request is for,
// or "" for any other request.
func disksPageAction(r *http.Request) string {
	action := r.URL.Query().Get(sessionParam)
	if _, ok := disksPageMethods[action]; ok {
		return action
	}
	return ""
}

// offersDisks reports whether the request behind cred may use the Disks
// page.
func offersDisks(set *settings, cred credential) bool {
	return set.disks != nil && cred.token && cred.user != nil && cred.user.isAdmin
}

// disksURL is the Disks page.
func disksURL() string {
	return "/?" + sessionParam + "=" + actionDisks
}

// handleDisks guards the Disks page and its endpoints and hands the request
// over.
func (s *Server) handleDisks(set *settings, w http.ResponseWriter, r *http.Request, action string) {
	if set.disks == nil {
		// switched off, the marker names nothing
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Cache-Control", "no-store")
	cred := s.identify(set, w, r)
	if !cred.token || cred.user == nil {
		s.logFor(r).Info("http disks refused, no session", "credentials", cred.method,
			"address", clientAddress(set, r))
		if action == actionDisks && browserRequest(r) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, loginURL(r.URL), http.StatusSeeOther)
			return
		}
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	if !cred.user.isAdmin {
		s.logFor(r).Warn("http disks refused, the account is not an admin",
			"user", cred.user.name, "address", clientAddress(set, r))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case action == actionDisks && read:
		s.disksPage(set, w, r, cred)
	case action == actionDisksState && read:
		writeJSON(w, http.StatusOK, disksState(set))
	case action == actionDisksDownload && read:
		s.diskDownload(set, w, r, cred.user)
	case action == actionDisksUpload && r.Method == http.MethodPut:
		if s.sameSite(set, w, r) {
			s.diskUpload(set, w, r, cred.user)
		}
	case disksPageMethods[action] == "POST" && r.Method == http.MethodPost:
		if s.sameSite(set, w, r) {
			s.diskChange(set, w, r, cred.user, action)
		}
	default:
		w.Header().Set("Allow", disksPageMethods[action])
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// --- the page ---------------------------------------------------------------

type disksData struct {
	Title   string
	State   disksStateJSON
	Session sessionView
	Nonce   string
	Style   template.CSS
	Script  template.JS
}

var disksTemplate = template.Must(template.ParseFS(assets, "assets/disks.html", "assets/partials.html"))

var disksScript = template.JS(mustRead("assets/disks.js"))

func (s *Server) disksPage(set *settings, w http.ResponseWriter, r *http.Request, cred credential) {
	nonce, err := pageNonce()
	if err != nil {
		s.logFor(r).Error("http cannot render the disks page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	who := s.sessionViewFor(set, r, cred)
	who.Page, who.Disks, who.Files = "Disks", "", "/"
	data := disksData{
		Title:   "Disks",
		State:   disksState(set),
		Session: who,
		Nonce:   nonce,
		Style:   listingStyle,
		Script:  disksScript,
	}
	var page bytes.Buffer
	if err := disksTemplate.Execute(&page, data); err != nil {
		s.logFor(r).Error("http cannot render the disks page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Content-Security-Policy", contentPolicy(nonce))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page.Bytes())
	}
}

// --- the state --------------------------------------------------------------

type disksStateJSON struct {
	Disks []diskJSON `json:"disks"`
	Jobs  []jobJSON  `json:"jobs"`
	// Folder is where the disks are, and NBDPort the port the NBD server
	// serves them on, 0 while it is off.
	Folder  string `json:"folder"`
	NBDPort int    `json:"nbdPort"`
	// ChunkSize is how large a piece of an upload may be.
	ChunkSize int64 `json:"chunkSize"`
	// Error is why the disks could not be listed.
	Error string `json:"error,omitempty"`
}

type diskJSON struct {
	Name      string        `json:"name"`
	Size      int64         `json:"size"`
	Allocated int64         `json:"allocated"`
	Modified  int64         `json:"modified"`
	Busy      string        `json:"busy,omitempty"`
	Holds     int           `json:"holds,omitempty"`
	Sessions  []sessionJSON `json:"sessions"`
}

type sessionJSON struct {
	ID       string `json:"id"`
	Account  string `json:"account"`
	Client   string `json:"client"`
	Writable bool   `json:"writable"`
	Since    int64  `json:"since"`
	Read     int64  `json:"read"`
	Written  int64  `json:"written"`
	Serial   string `json:"serial"`
	Expires  int64  `json:"expires"`
}

type jobJSON struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	Started int64  `json:"started"`
}

// disksChunk is the largest piece of a disk upload when http.maxChunkSize
// says nothing.
const disksChunk = 50 << 20

func disksState(set *settings) disksStateJSON {
	state := disksStateJSON{Folder: set.disks.Base(), NBDPort: set.cfg.NBDPort, ChunkSize: set.cfg.MaxChunkSize,
		Disks: []diskJSON{}, Jobs: []jobJSON{}}
	if state.ChunkSize <= 0 {
		state.ChunkSize = disksChunk
	}
	list, err := set.disks.List()
	if err != nil {
		state.Error = "The disks cannot be listed: " + err.Error()
	}
	for _, disk := range list {
		row := diskJSON{Name: disk.Name, Size: disk.Size, Allocated: disk.Allocated,
			Modified: disk.Modified.UnixMilli(), Busy: disk.Busy, Holds: disk.Holds, Sessions: []sessionJSON{}}
		for _, session := range disk.Sessions {
			row.Sessions = append(row.Sessions, sessionJSON{
				ID: session.ID, Account: session.Account, Client: session.Client, Writable: session.Writable,
				Since: session.Since.UnixMilli(), Read: session.Read(), Written: session.Written(),
				Serial: session.Serial, Expires: session.Expires.UnixMilli(),
			})
		}
		state.Disks = append(state.Disks, row)
	}
	for _, job := range set.disks.Jobs() {
		done, total := job.Progress()
		status, message, _ := job.State()
		state.Jobs = append(state.Jobs, jobJSON{ID: job.ID, Source: job.Source, Target: job.Target,
			State: status, Message: message, Done: done, Total: total, Started: job.Started.UnixMilli()})
	}
	return state
}

// --- what the page asks for -------------------------------------------------

// maxDiskRequest bounds the body of a change: a couple of names and a size.
const maxDiskRequest = 16 << 10

type diskRequest struct {
	Name string `json:"name"`
	// To is the new name of a rename and the name of a clone.
	To string `json:"to"`
	// Size is the size of a new disk or of a resize, as ParseSize reads it.
	Size string `json:"size"`
	// Shrink allows a resize to make the disk smaller.
	Shrink bool `json:"shrink"`
	// Confirm has to repeat the name for a delete.
	Confirm string `json:"confirm"`
	// ID is the session of a disconnect or the job of a cancel or a dismiss.
	ID string `json:"id"`
	// Cancel stops the job rather than dismissing it.
	Cancel bool `json:"cancel"`
}

// diskChange runs one of the changes the page posts.
func (s *Server) diskChange(set *settings, w http.ResponseWriter, r *http.Request, user *account, action string) {
	var asked diskRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDiskRequest)).Decode(&asked); err != nil {
		http.Error(w, "The request is not valid JSON.", http.StatusBadRequest)
		return
	}
	folder := set.disks
	log := s.logFor(r).With("user", user.name, "address", clientAddress(set, r))
	switch action {
	case actionDisksCreate:
		size, err := disks.ParseSize(asked.Size)
		if err == nil {
			err = folder.Create(asked.Name, size)
		}
		if err != nil {
			s.diskFailed(w, log, "create", asked.Name, err)
			return
		}
		log.Info("http disk created", "disk", asked.Name, "size", size)

	case actionDisksResize:
		size, err := disks.ParseSize(asked.Size)
		if err == nil {
			err = folder.Resize(asked.Name, size, asked.Shrink)
		}
		if err != nil {
			s.diskFailed(w, log, "resize", asked.Name, err)
			return
		}
		log.Info("http disk resized", "disk", asked.Name, "size", size)

	case actionDisksRename:
		if err := folder.Rename(asked.Name, asked.To); err != nil {
			s.diskFailed(w, log, "rename", asked.Name, err)
			return
		}
		log.Info("http disk renamed", "disk", asked.Name, "to", asked.To)

	case actionDisksClone:
		job, err := folder.Clone(asked.Name, asked.To)
		if err != nil {
			s.diskFailed(w, log, "clone", asked.Name, err)
			return
		}
		log.Info("http disk clone started", "disk", asked.Name, "to", asked.To)
		go func() {
			<-job.Done()
			state, message, _ := job.State()
			done, total := job.Progress()
			if state == disks.JobFailed {
				log.Error("http disk clone failed", "disk", job.Source, "to", job.Target, "error", message)
				return
			}
			log.Info("http disk clone ended", "disk", job.Source, "to", job.Target, "state", state,
				"bytes", done, "of", total, "took", time.Since(job.Started).Round(time.Millisecond))
		}()

	case actionDisksDelete:
		if asked.Confirm != asked.Name {
			http.Error(w, "Type the name of the disk to delete it.", http.StatusBadRequest)
			return
		}
		if err := folder.Delete(asked.Name); err != nil {
			s.diskFailed(w, log, "delete", asked.Name, err)
			return
		}
		log.Info("http disk deleted", "disk", asked.Name)

	case actionDisksDisconnect:
		var found *disks.Session
		for _, session := range folder.Sessions() {
			if session.ID == asked.ID {
				found = session
			}
		}
		if found == nil || !folder.Disconnect(asked.ID) {
			http.Error(w, "That client is no longer connected.", http.StatusNotFound)
			return
		}
		log.Info("http disk client disconnected", "disk", found.Disk, "client", found.Client, "account", found.Account)

	case actionDisksJob:
		job := folder.Job(asked.ID)
		switch {
		case job == nil:
			http.Error(w, "That clone is no longer listed.", http.StatusNotFound)
			return
		case asked.Cancel:
			job.Cancel()
			log.Info("http disk clone cancelled", "disk", job.Source, "to", job.Target)
		case !folder.Dismiss(asked.ID):
			http.Error(w, "A clone that is still running cannot be dismissed; cancel it.", http.StatusConflict)
			return
		}
	}
	writeJSON(w, http.StatusOK, disksState(set))
}

// diskFailed answers a change that failed, in words the page can show.
func (s *Server) diskFailed(w http.ResponseWriter, log interface {
	Error(string, ...any)
	Info(string, ...any)
}, op, name string, err error) {
	switch {
	case errors.Is(err, disks.ErrName), errors.Is(err, disks.ErrSize), errors.Is(err, disks.ErrShrink),
		errors.Is(err, disks.ErrShortChunk):
		http.Error(w, upperFirst(err.Error())+".", http.StatusBadRequest)
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, fmt.Sprintf("There is no disk %q.", name), http.StatusNotFound)
	case errors.Is(err, fs.ErrExist):
		http.Error(w, upperFirst(err.Error())+": choose another name.", http.StatusConflict)
	case errors.Is(err, disks.ErrBusy):
		log.Info("http disk "+op+" refused", "disk", name, "reason", err)
		http.Error(w, upperFirst(err.Error())+".", http.StatusConflict)
	default:
		log.Error("http disk "+op+" failed", "disk", name, "error", err)
		http.Error(w, "The disk could not be changed: "+err.Error(), http.StatusInternalServerError)
	}
}

func upperFirst(text string) string {
	if text == "" || text[0] < 'a' || text[0] > 'z' {
		return text
	}
	return string(text[0]-'a'+'A') + text[1:]
}

// diskUpload stores one chunk of a disk upload, PUT with Content-Range as
// the listing uploads a file, to ?go-fs=disks-upload&name=<disk>; replace=1
// lets the upload replace a disk of that name. The last chunk puts the disk in
// place.
func (s *Server) diskUpload(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	query := r.URL.Query()
	name := query.Get("name")
	replace := query.Get("replace") == "1"
	log := s.logFor(r).With("user", user.name, "address", clientAddress(set, r))
	if err := disks.ValidName(name); err != nil {
		s.diskFailed(w, log, "upload", name, err)
		return
	}
	rng, ok := parseContentRange(r.Header.Get("Content-Range"))
	if !ok {
		http.Error(w, "A disk is uploaded in pieces, each with a Content-Range: bytes start-end/total.",
			http.StatusBadRequest)
		return
	}
	if !rawUpload(r.Header.Get("Content-Type")) {
		http.Error(w, "The body has to be application/octet-stream.", http.StatusUnsupportedMediaType)
		return
	}
	limit := set.cfg.MaxChunkSize
	if limit <= 0 {
		limit = disksChunk
	}
	size := rng.end - rng.start + 1
	if size > limit {
		http.Error(w, fmt.Sprintf("a piece cannot be larger than %d bytes", limit), http.StatusRequestEntityTooLarge)
		return
	}
	if rng.start == 0 && !replace && set.disks.Exists(name) {
		http.Error(w, fmt.Sprintf("There is a disk %q already: choose another name, or replace it.", name),
			http.StatusConflict)
		return
	}
	if set.cfg.ReadTimeout > 0 {
		r.Body = &idleUploadBody{ReadCloser: r.Body, controller: http.NewResponseController(w),
			idle: seconds(set.cfg.ReadTimeout)}
	}
	started := time.Now()
	complete, err := set.disks.WriteChunk(name,
		disks.Chunk{Start: rng.start, End: rng.end, Total: rng.total}, http.MaxBytesReader(w, r.Body, size))
	var behind *disks.OutOfSyncError
	switch {
	case errors.As(err, &behind):
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", behind.Have))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	case err != nil && (isClientGone(err) || tooLarge(err)):
		log.Info("http disk upload failed", "disk", name, "start", rng.start, "error", err)
		http.Error(w, "The piece did not arrive whole.", http.StatusBadRequest)
		return
	case err != nil:
		s.diskFailed(w, log, "upload", name, err)
		return
	}
	if !complete {
		log.Debug("http disk upload piece stored", "disk", name, "start", rng.start, "end", rng.end,
			"total", rng.total, "took", time.Since(started).Round(time.Millisecond))
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := set.disks.FinishUpload(name, rng.total, replace); err != nil {
		s.diskFailed(w, log, "upload", name, err)
		return
	}
	log.Info("http disk uploaded", "disk", name, "bytes", rng.total, "replaced", replace)
	w.WriteHeader(http.StatusCreated)
}

// diskDownload sends a disk whole, or the range asked for. Nothing may write
// to it meanwhile, so a disk attached read-write is refused.
func (s *Server) diskDownload(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	name := r.URL.Query().Get("name")
	log := s.logFor(r).With("user", user.name, "address", clientAddress(set, r))
	file, lease, err := set.disks.OpenRead(name)
	if err != nil {
		s.diskFailed(w, log, "download", name, err)
		return
	}
	defer lease.Release()
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		s.diskFailed(w, log, "download", name, err)
		return
	}
	started := time.Now()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		log.Info("http disk download", "disk", name, "bytes", info.Size(),
			"took", time.Since(started).Round(time.Millisecond))
	}
}

// sweepDisks removes what uploads and clones of the Disks page left behind.
func (s *Server) sweepDisks() {
	set := s.settings()
	if set.disks == nil {
		return
	}
	for _, name := range set.disks.Sweep(stagingMaxAge) {
		s.log.Info("http cleanup removed an abandoned disk upload or clone", "file", name,
			"folder", set.disks.Base())
	}
}
