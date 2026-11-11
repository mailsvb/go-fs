package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/vfs"
)

// The page of a stored server fetches a file of it into the served tree: the
// other way round from Send File. Its dialog lists the folders of the served
// tree, from the root, for the user to choose where the file goes, and the
// fetch is a job like one from a URL, which the header follows among the
// transfers and which stores the file the way that one does: in the staging
// folder until all of it is there, and then under its name.
//
// A folder is fetched the same way, with all that is below it, as a folder of
// its name in the one chosen. The dialog first measures it, to say how many
// files it holds and how large they are together, and both the dialog and the
// job make sure they fit into the folder chosen before a byte is read.
//
// The endpoints are markers on the root of the served tree, as the page of
// the server is:
//
//	GET  /?go-fs=local-browse&path=<folder>
//	GET  /?go-fs=remote-measure&server=<name>&path=<file or folder>
//	POST /?go-fs=remote-fetch&server=<name>&path=<file or folder>   {"folder": "<folder>"}
//
// A folder is listed to whoever may list it in the served tree, and a fetch
// into it is offered as a fetch from a URL is: to a session of an account that
// may create there, and that may create folders as well for a folder. What is
// fetched is read with the login the server was stored with, so it is offered
// to whoever may browse the server.

const (
	actionLocalBrowse   = "local-browse"
	actionRemoteMeasure = "remote-measure"
	actionRemoteFetch   = "remote-fetch"
	// localBrowsePathParam names the folder of the served tree to list.
	localBrowsePathParam = "path"
)

// remoteFetchAction reports which endpoint of a fetch from a stored server a
// request is for, or "" for any other request.
func remoteFetchAction(r *http.Request) string {
	switch action := r.URL.Query().Get(sessionParam); action {
	case actionLocalBrowse, actionRemoteMeasure, actionRemoteFetch:
		return action
	default:
		return ""
	}
}

// handleRemoteFetch answers the endpoints of a fetch from a stored server.
func (s *Server) handleRemoteFetch(set *settings, w http.ResponseWriter, r *http.Request, cred credential, action string) {
	w.Header().Set("Cache-Control", "no-store")
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case action == actionLocalBrowse && read:
		s.localBrowse(set, w, r, cred)
	case action == actionRemoteMeasure && read:
		s.remoteMeasure(set, w, r, cred)
	case action == actionRemoteFetch && r.Method == http.MethodPost:
		if !s.sameSite(set, w, r) {
			return
		}
		s.remoteFetchStart(set, w, r, cred)
	default:
		if action != actionRemoteFetch {
			w.Header().Set("Allow", "GET, HEAD")
		} else {
			w.Header().Set("Allow", http.MethodPost)
		}
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// localFolderJSON is a folder of the served tree, as the dialog lists it.
type localFolderJSON struct {
	sendFolderJSON
	// Writable says the account may fetch a file into the folder, and Mkdir
	// that it may fetch a folder into it.
	Writable bool `json:"writable"`
	Mkdir    bool `json:"mkdir"`
	// Free is how many bytes the disk of the folder has free, or -1 when
	// that cannot be told.
	Free int64 `json:"free"`
}

// localBrowse lists a folder of the served tree, folders first.
func (s *Server) localBrowse(set *settings, w http.ResponseWriter, r *http.Request, cred credential) {
	if !mayFollowFetches(cred) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	asked := strings.TrimSpace(r.URL.Query().Get(localBrowsePathParam))
	if asked == "" {
		asked = "/"
	}
	target := s.root.Resolve("/", asked)
	if !target.Valid {
		http.NotFound(w, r)
		return
	}
	if info, err := logging.Stat(s.logFor(r), target.Path); err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	folder := vfs.AsFolder(target.Virtual)
	if !s.permits(set, cred.user, http.MethodGet, folder, actRead) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	entries, err := readDirectory(s.root, target.Path)
	if err != nil {
		s.logFor(r).Error("http cannot read the folder", "path", target.Virtual, "error", err)
		http.Error(w, "The folder cannot be read.", http.StatusInternalServerError)
		return
	}
	shown := strings.TrimSuffix(folder, "/")
	if shown == "" {
		shown = "/"
	}
	view := localFolderJSON{
		sendFolderJSON: sendFolderJSON{Path: shown, Entries: []sendEntryJSON{}},
		Writable:       mayFetch(cred, folder),
		Mkdir:          mayFetchFolder(cred, folder),
		Free:           tracedFreeSpace(s.logFor(r), target.Path),
	}
	if shown != "/" {
		view.Parent = path.Dir(shown)
	}
	// readDirectory has the folders first, the newest files next; the dialog
	// lists files by name, as it does those of a host
	for _, item := range entries {
		if len(view.Entries) == maxSendEntries {
			view.Truncated = true
			break
		}
		if item.isFolder() {
			view.Entries = append(view.Entries, sendEntryJSON{Name: item.bare(), Dir: true})
		} else {
			view.Entries = append(view.Entries, sendEntryJSON{Name: item.Name, Size: item.Size})
		}
	}
	sortFolderEntries(view.Entries)
	writeJSON(w, http.StatusOK, view)
}

// remoteFetchBody is the body of a fetch from a stored server.
type remoteFetchBody struct {
	// Folder is the folder of the served tree to store the file in.
	Folder string `json:"folder"`
}

// remoteSource reads which file or folder of which stored server a request
// to fetch or to measure one names, and answers one that the account may not
// fetch from, or that names none. It reports whether the request goes on.
func (s *Server) remoteSource(set *settings, w http.ResponseWriter, r *http.Request, cred credential,
	address string) (config.Server, remote.Login, string, bool) {
	if !mayBrowseRemote(cred) {
		s.logFor(r).Info("http remote fetch refused, no session of an account that may read",
			"user", nameOf(cred.user), "address", address)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return config.Server{}, remote.Login{}, "", false
	}
	query := r.URL.Query()
	server, ok := config.FindServer(serversFor(set, cred.user), query.Get(remoteServerParam))
	if !ok {
		s.logUnadmitted(set, cred, query.Get(remoteServerParam), address)
		http.NotFound(w, r)
		return config.Server{}, remote.Login{}, "", false
	}
	if !server.Rights().Download {
		s.logFor(r).Info("http remote fetch refused, the server does not allow downloads",
			"server", server.Name, "user", cred.user.name, "address", address)
		http.Error(w, fmt.Sprintf("the server %s does not allow to download", server.Name), http.StatusForbidden)
		return config.Server{}, remote.Login{}, "", false
	}
	login, err := remote.FromServer(server).Checked(false)
	if err != nil {
		http.Error(w, fmt.Sprintf("the server %s: %v", server.Name, err), http.StatusBadGateway)
		return config.Server{}, remote.Login{}, "", false
	}
	file := remotePath(query)
	if file == "" || file == "/" {
		http.Error(w, "name the file or folder to fetch", http.StatusBadRequest)
		return config.Server{}, remote.Login{}, "", false
	}
	return server, login, file, true
}

// remoteMeasureJSON is what the dialog reads of what it is about to fetch.
type remoteMeasureJSON struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	// Files and Folders count what is below a folder, and Skipped the
	// folders below it that could not be read, which a fetch leaves out.
	Files   int `json:"files"`
	Folders int `json:"folders"`
	Skipped int `json:"skipped"`
	// Bytes is how large the files are together, and Largest the largest.
	Bytes   int64 `json:"bytes"`
	Largest int64 `json:"largest"`
}

// remoteMeasure says how many files a fetch of a file or a folder of a
// stored server stores, and how large they are. It walks a folder to its
// bottom, which the dialog waits for; a dialog that is closed stops it.
func (s *Server) remoteMeasure(set *settings, w http.ResponseWriter, r *http.Request, cred credential) {
	address := clientAddress(set, r)
	_, login, file, ok := s.remoteSource(set, w, r, cred, address)
	if !ok {
		return
	}
	fsys, err := remote.Open(r.Context(), login, set.ssh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = fsys.Close() }()
	fail := func(err error) {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, remoteFolderFailure(sendRequest{Login: login}, file, err).Error(), http.StatusBadGateway)
	}
	p, err := fsys.Resolve(file)
	var info fs.FileInfo
	if err == nil {
		info, err = fsys.Stat(p)
	}
	if err != nil {
		fail(err)
		return
	}
	view := remoteMeasureJSON{Name: cleanFetchedName(p), Dir: info.IsDir()}
	switch {
	case info.Mode().IsRegular():
		view.Files, view.Bytes, view.Largest = 1, info.Size(), info.Size()
	case info.IsDir():
		started := time.Now()
		err := walkRemoteTree(r.Context(), fsys, p, func(_, _ string, info fs.FileInfo) error {
			if info.IsDir() {
				view.Folders++
				return nil
			}
			view.Files++
			view.Bytes += info.Size()
			view.Largest = max(view.Largest, info.Size())
			return nil
		}, func(string, error) { view.Skipped++ })
		if err != nil {
			if r.Context().Err() == nil {
				fail(err)
			}
			return
		}
		s.logFor(r).Debug("http remote fetch measured", "folder", login.Where(p)+" (server "+login.Server+")",
			"files", view.Files, "bytes", view.Bytes, "took", time.Since(started).Round(time.Millisecond))
	default:
		http.Error(w, fmt.Sprintf("%s on %s is neither a file nor a folder", p, login.Shown()), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// remoteFetchStart starts a fetch of a file or a folder of a stored server.
func (s *Server) remoteFetchStart(set *settings, w http.ResponseWriter, r *http.Request, cred credential) {
	address := clientAddress(set, r)
	server, login, file, ok := s.remoteSource(set, w, r, cred, address)
	if !ok {
		return
	}
	var body remoteFetchBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxTransferRequest)).Decode(&body); err != nil {
		http.Error(w, "The request could not be read.", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Folder) == "" {
		http.Error(w, "choose the folder to fetch into", http.StatusBadRequest)
		return
	}
	target := s.root.Resolve("/", body.Folder)
	if !target.Valid {
		http.Error(w, "There is no such folder here.", http.StatusBadRequest)
		return
	}
	if info, err := logging.Stat(s.logFor(r), target.Path); err != nil || !info.IsDir() {
		http.Error(w, "There is no such folder here.", http.StatusBadRequest)
		return
	}
	folder := vfs.AsFolder(target.Virtual)
	if !mayFetch(cred, folder) {
		s.logFor(r).Info("http remote fetch refused, no session of an account that may create here",
			"user", cred.user.name, "folder", folder, "address", address)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	user := cred.user
	shown := login.Where(file) + " (server " + server.Name + ")"
	job, err := s.startJob(jobFetch, user.name, shown+" → "+folder, func(ctx context.Context, job *registryJob) (string, error) {
		job.named(cleanFetchedName(file), folder)
		message, err := s.remoteFetchFile(ctx, set, job, login, file, folder, user, address)
		if err != nil && ctx.Err() == nil {
			s.logFor(r).Warn("http remote fetch failed", "file", shown, "folder", folder, "user", user.name,
				"address", address, "error", err)
		}
		return message, err
	})
	if err == nil {
		s.logFor(r).Info("http remote fetch started", "file", shown, "folder", folder, "user", user.name,
			"address", address, "job", job.id)
	}
	s.answerStarted(w, job, err)
}

// remoteFetchFile downloads a file of a stored server into a folder of the
// served tree, and says what it stored.
func (s *Server) remoteFetchFile(ctx context.Context, set *settings, job *registryJob, login remote.Login,
	file, folder string, user *account, address string) (string, error) {
	started := time.Now()
	// the connection lives as long as getCtx, so a fetch that is stopped or
	// stalls is cut at once; a read leaves nothing behind on the server
	getCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fsys, err := remote.Open(logging.Ensure(getCtx, s.log), login, set.ssh)
	if err != nil {
		return "", err
	}
	defer func() { _ = fsys.Close() }()

	failure := func(err error) error {
		return remoteFolderFailure(sendRequest{Login: login}, file, err)
	}
	p, err := fsys.Resolve(file)
	if err != nil {
		return "", failure(err)
	}
	info, err := fsys.Stat(p)
	if err != nil {
		return "", failure(err)
	}
	if info.IsDir() {
		return s.remoteFetchFolder(getCtx, cancel, set, job, fsys, login, p, folder, user, address)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s on %s is neither a file nor a folder", p, login.Shown())
	}

	name := cleanFetchedName(p)
	target := s.root.Resolve(folder, name)
	if name == "" || !target.Valid || vfs.AsFolder(path.Dir(target.Virtual)) != folder {
		return "", fmt.Errorf("a file named %q cannot be stored here", name)
	}
	// asked before the download as well as after it, so that a name that is
	// taken costs nothing
	if _, err := fetchReplaces(target, name, user); err != nil {
		return "", err
	}
	if limit := set.cfg.MaxUploadSize; limit > 0 && info.Size() > limit {
		return "", fetchTooLarge(name, limit)
	}
	if err := checkRoom(s.log, set, filepath.Dir(target.Path), folder, info.Size(), info.Size()); err != nil {
		return "", err
	}
	job.named(name, "")
	job.plan(0, info.Size())

	source, err := fsys.Open(p)
	if err != nil {
		return "", failure(err)
	}
	defer func() { _ = source.Close() }()
	written, replace, err := s.storeFetched(set, job, stoppable{r: source, ctx: getCtx}, cancel,
		target, name, login.Shown(), user)
	if err != nil {
		if errors.Is(context.Cause(getCtx), errStalled) {
			return "", fmt.Errorf("%s from %s: %w", name, login.Shown(), errStalled)
		}
		return "", err
	}
	if written != info.Size() {
		// the file changed while it was read; what is stored is what was read
		s.log.Warn("http remote fetch read another size than the server named", "file", login.Where(p),
			"named", info.Size(), "read", written)
	}
	s.log.Info("http remote fetch", "user", user.name, "from", login.Where(p)+" (server "+login.Server+")",
		"file", target.Virtual, "bytes", written, "replaced", replace, "address", address,
		"took", time.Since(started).Round(time.Millisecond))
	return fmt.Sprintf("Fetched %s (%s) from %s into %s.", name, readableSize(written), login.Server, folder), nil
}

// fetchListing is the phase of a fetch of a folder while it walks the folder
// to learn what it stores, before it reads a byte.
const fetchListing = "listing"

// fetchedFile is a file a fetch of a folder stores: where it is read from,
// and where it goes.
type fetchedFile struct {
	from   string
	target vfs.Target
	size   int64
}

// remoteFetchFolder downloads a folder of a stored server, with all that is
// below it, into a folder of the served tree as a folder of its name, and
// says what it stored. A folder of that name already there is filled in.
// Everything is checked before the first byte is read: that each name can be
// stored here, that each folder may be created and each file stored or
// replaced, and that all of it fits. A fetch that fails or is stopped part of
// the way keeps the files it stored.
func (s *Server) remoteFetchFolder(ctx context.Context, cancel context.CancelCauseFunc, set *settings,
	job *registryJob, fsys remote.FS, login remote.Login, p, folder string, user *account,
	address string) (string, error) {
	started := time.Now()
	name := cleanFetchedName(p)
	top := s.root.Resolve(folder, name)
	if name == "" || !top.Valid || vfs.AsFolder(path.Dir(top.Virtual)) != folder {
		return "", fmt.Errorf("a folder named %q cannot be stored here", name)
	}
	job.named(name+"/", folder)
	job.setPhase(fetchListing, 0)

	folders := []vfs.Target{top}
	var files []fetchedFile
	var total, largest int64
	skipped := 0
	err := walkRemoteTree(ctx, fsys, p, func(from, inside string, info fs.FileInfo) error {
		want := path.Join(top.Virtual, inside)
		target := s.root.Resolve("/", want)
		// a name of the server that is not one name here, or that this
		// system cannot store, is refused rather than stored elsewhere
		if cleanFetchedName(info.Name()) != info.Name() || !target.Valid || target.Virtual != want {
			return fmt.Errorf("%q cannot be stored here", path.Join(name, inside))
		}
		if info.IsDir() {
			folders = append(folders, target)
			return nil
		}
		files = append(files, fetchedFile{from: from, target: target, size: info.Size()})
		total += info.Size()
		largest = max(largest, info.Size())
		return nil
	}, func(dir string, err error) {
		skipped++
		s.log.Info("http remote fetch skips a folder it cannot read", "folder", login.Where(dir),
			"user", user.name, "error", err)
	})
	if err != nil {
		return "", remoteFolderFailure(sendRequest{Login: login}, p, err)
	}

	for _, dir := range folders {
		info, err := logging.Stat(s.log, dir.Path)
		switch {
		case err == nil && !info.IsDir():
			return "", fmt.Errorf("%q is taken here by something that is not a folder", dir.Virtual)
		case err == nil:
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		case !fetchGranted(user, dir.Virtual, actMkdir):
			return "", fmt.Errorf("you may not create the folder %q here", dir.Virtual)
		}
	}
	limit := set.cfg.MaxUploadSize
	for _, file := range files {
		if _, err := fetchReplaces(file.target, file.target.Virtual, user); err != nil {
			return "", err
		}
		if limit > 0 && file.size > limit {
			return "", fetchTooLarge(file.target.Virtual, limit)
		}
	}
	if err := checkRoom(s.log, set, filepath.Dir(top.Path), folder, total, largest); err != nil {
		return "", err
	}

	job.plan(len(files), total)
	job.downloading()
	for _, dir := range folders {
		if err := logging.MkdirAll(s.log, dir.Path, 0o755); err != nil {
			return "", err
		}
	}
	stored := 0
	fail := func(err error) (string, error) {
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errStalled) {
			s.log.Info("http remote fetch stopped, keeps what it stored", "folder", top.Virtual,
				"files", stored, "of", len(files), "user", user.name)
			return "", err
		}
		if stored > 0 {
			err = fmt.Errorf("%w (%d of %d files were stored in %s)", err, stored, len(files), top.Virtual)
		}
		return "", err
	}
	var written int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		n, err := s.remoteFetchOne(ctx, cancel, set, job, fsys, login, file, user)
		if err != nil {
			return fail(err)
		}
		written += n
		stored++
		job.blobsDone.Add(1)
	}

	s.log.Info("http remote fetch", "user", user.name, "from", login.Where(p)+" (server "+login.Server+")",
		"folder", top.Virtual, "files", stored, "folders", len(folders), "skipped", skipped, "bytes", written,
		"address", address, "took", time.Since(started).Round(time.Millisecond))
	message := fmt.Sprintf("Fetched %s (%s) of %s from %s into %s.", countFiles(stored), readableSize(written),
		name, login.Server, folder)
	switch skipped {
	case 0:
	case 1:
		message += " 1 folder could not be read and was left out."
	default:
		message += fmt.Sprintf(" %d folders could not be read and were left out.", skipped)
	}
	return message, nil
}

// remoteFetchOne stores one file of a fetch of a folder, and says how much of
// it it stored.
func (s *Server) remoteFetchOne(ctx context.Context, cancel context.CancelCauseFunc, set *settings,
	job *registryJob, fsys remote.FS, login remote.Login, file fetchedFile, user *account) (int64, error) {
	source, err := fsys.Open(file.from)
	if err != nil {
		return 0, remoteFolderFailure(sendRequest{Login: login}, file.from, err)
	}
	defer func() { _ = source.Close() }()
	name := path.Base(file.target.Virtual)
	written, _, err := s.storeFetched(set, job, stoppable{r: source, ctx: ctx}, cancel,
		file.target, name, login.Shown(), user)
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return 0, fmt.Errorf("%s from %s: %w", name, login.Shown(), errStalled)
	}
	return written, err
}

// countFiles says how many files there are, in words.
func countFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}
