package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/syncer"
)

// The syncs are edited in a dialog, as the remote servers are: it asks for
// the protocol, then for one of the stored servers the protocol reaches, then
// for the remote and the local folder, which it browses, and the direction,
// and last for when the sync runs. The server is logged in to and the remote
// folder looked up before the sync is written into the file, at once and with
// nothing else of the page.

// KindSync is the Create of the [[syncs]] table, which tells the page to add
// and edit its records with the dialog.
const KindSync = "sync"

// maxBrowsed caps the folders a browse lists, as the Send File dialog caps
// its own.
const maxBrowsed = 2000

// syncJSON is a sync as the dialog edits it.
type syncJSON struct {
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Protocol     string   `json:"protocol"`
	Server       string   `json:"server"`
	RemoteFolder string   `json:"remoteFolder"`
	LocalFolder  string   `json:"localFolder"`
	Direction    string   `json:"direction"`
	Delete       bool     `json:"delete"`
	Every        string   `json:"every"`
	At           string   `json:"at"`
	Days         []string `json:"days"`
}

// syncSaveBody stores a sync.
type syncSaveBody struct {
	// Was is the name the sync had in the file when the page read it, ""
	// for a sync that is new.
	Was  string   `json:"was"`
	Sync syncJSON `json:"sync"`
}

// syncBrowseBody asks for the folders in a folder: of a stored server, by
// its name, or of the served folder.
type syncBrowseBody struct {
	Remote bool   `json:"remote"`
	Server string `json:"server"`
	Path   string `json:"path"`
}

// syncFolderJSON is a folder a browse lists the folders of.
type syncFolderJSON struct {
	// Path is the folder, as it is stored: absolute on the server, from the
	// top of the served folder locally. Parent is the one above it, "" at
	// the top.
	Path      string   `json:"path"`
	Parent    string   `json:"parent"`
	Folders   []string `json:"folders"`
	Truncated bool     `json:"truncated,omitempty"`
}

// syncRunBody runs a sync now.
type syncRunBody struct {
	Name string `json:"name"`
}

// syncView is what the page shows of a stored sync besides its keys: how it
// is doing, when it is running.
type syncView struct {
	Status *syncer.Status `json:"status,omitempty"`
}

// syncViews describes the syncs of cfg, by name.
func syncViews(cfg config.Config) map[string]syncView {
	statuses := syncer.Statuses()
	views := make(map[string]syncView, len(cfg.Syncs))
	for _, s := range cfg.Syncs {
		var view syncView
		if status, ok := statuses[s.Name]; ok {
			view.Status = &status
		}
		views[s.Name] = view
	}
	return views
}

// decodeSync reads a body the sync dialog posted.
func (h *Handler) decodeSync(w http.ResponseWriter, r *http.Request, into any) bool {
	if !h.posted(w, r) {
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPost)).Decode(into); err != nil {
		http.Error(w, "the body is not a sync: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// openSyncServer logs in to a stored server, for a browse or to look up a
// remote folder.
func openSyncServer(ctx context.Context, cfg config.Config, name string) (remote.FS, remote.Login, error) {
	server, ok := config.FindServer(cfg.UsableServers(), name)
	if !ok {
		return nil, remote.Login{}, fmt.Errorf("%q is not a stored server", name)
	}
	login, err := remote.FromServer(server).Checked(false)
	if err != nil {
		return nil, remote.Login{}, err
	}
	fsys, err := remote.Open(ctx, login, cfg.General.SSH)
	if err != nil {
		return nil, login, err
	}
	return fsys, login, nil
}

// handleSyncSave looks the remote folder of a sync up and, once it is there,
// writes the sync into the file: in place of the one it was, or after the
// others.
func (h *Handler) handleSyncSave(w http.ResponseWriter, r *http.Request) {
	var body syncSaveBody
	if !h.decodeSync(w, r, &body) {
		return
	}
	h.saving.Lock()
	defer h.saving.Unlock()

	cfg, err := h.read()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	index := -1
	if body.Was != "" {
		for i, s := range cfg.Syncs {
			if s.Name == body.Was {
				index = i
				break
			}
		}
		if index < 0 {
			http.Error(w, fmt.Sprintf("the file no longer holds the sync %q; reload the page", body.Was),
				http.StatusConflict)
			return
		}
	}

	posted := body.Sync
	s := config.Sync{
		Name:      strings.TrimSpace(posted.Name),
		Enabled:   posted.Enabled,
		Protocol:  strings.TrimSpace(posted.Protocol),
		Server:    strings.TrimSpace(posted.Server),
		Direction: strings.TrimSpace(posted.Direction),
		Delete:    posted.Delete,
		Every:     strings.TrimSpace(posted.Every),
		At:        strings.TrimSpace(posted.At),
		Days:      posted.Days,
	}
	if s.Name == "" {
		http.Error(w, "name the sync", http.StatusBadRequest)
		return
	}
	if _, ok := config.SyncProtocolOf(s.Protocol); !ok {
		http.Error(w, fmt.Sprintf("protocol %q is not one this build runs", s.Protocol), http.StatusBadRequest)
		return
	}
	if s.LocalFolder, err = config.SyncFolder(posted.LocalFolder); err != nil {
		http.Error(w, "local folder: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.RemoteFolder, err = config.SyncRemoteFolder(posted.RemoteFolder); err != nil {
		http.Error(w, "remote folder: "+err.Error(), http.StatusBadRequest)
		return
	}
	if index >= 0 {
		cfg.Syncs[index] = s
	} else {
		cfg.Syncs = append(cfg.Syncs, s)
		index = len(cfg.Syncs) - 1
	}
	// what the file would be is checked before the server is logged in to,
	// so that a name that is taken is said at once
	if err := cfg.Resolved().Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverTimeout)
	defer cancel()
	if err := lookUpRemoteFolder(ctx, cfg, s); err != nil {
		h.log.Info("the admin interface did not store a sync, its remote folder cannot be reached",
			"sync", s.Name, "server", s.Server, "remoteFolder", s.RemoteFolder, "error", err, "address", addressOf(r))
		status := http.StatusBadGateway
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotAFolder) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	backup, err := h.write(cfg)
	if err != nil {
		h.log.Error("the admin interface cannot write the configuration",
			"path", h.path, "error", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	h.log.Info("the admin interface stored a sync", "sync", s.Name, "enabled", s.Enabled, "server", s.Server,
		"remoteFolder", s.RemoteFolder, "localFolder", s.LocalFolder, "direction", s.Direction, "delete", s.Delete,
		"path", h.path, "backup", backup, "address", addressOf(r))

	records, _ := h.schema.Values(cfg)["syncs"].([]any)
	var record any
	if index < len(records) {
		record = records[index]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"record": record,
		"view":   syncViews(cfg)[s.Name],
		"path":   h.path,
		"backup": backup,
		"reload": cfg.General.ReloadConfig,
	})
}

// errNotAFolder is a remote folder that is a file.
var errNotAFolder = errors.New("is not a folder")

// lookUpRemoteFolder logs in to the server of a sync and checks its remote
// folder is one.
func lookUpRemoteFolder(ctx context.Context, cfg config.Config, s config.Sync) error {
	fsys, login, err := openSyncServer(ctx, cfg, s.Server)
	if err != nil {
		return err
	}
	defer func() { _ = fsys.Close() }()
	if _, ok := fsys.(remote.Chtimer); !ok {
		return fmt.Errorf("%s cannot keep the modification time of a file, which %s needs", login.Shown(), s.Protocol)
	}
	folder, err := fsys.Resolve(s.RemoteFolder)
	if err != nil {
		return fmt.Errorf("the folder %q of %s: %w", s.RemoteFolder, login.Shown(), err)
	}
	info, err := fsys.Stat(folder)
	if err != nil {
		return fmt.Errorf("the folder %q of %s: %w", s.RemoteFolder, login.Shown(), err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s of %s %w", folder, login.Shown(), errNotAFolder)
	}
	return nil
}

// handleSyncBrowse lists the folders in a folder, of a stored server or of
// the served folder, for the dialog to pick one.
func (h *Handler) handleSyncBrowse(w http.ResponseWriter, r *http.Request) {
	var body syncBrowseBody
	if !h.decodeSync(w, r, &body) {
		return
	}
	cfg, err := h.read()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var folder syncFolderJSON
	if body.Remote {
		ctx, cancel := context.WithTimeout(r.Context(), serverTimeout)
		defer cancel()
		folder, err = browseRemote(ctx, cfg, body.Server, body.Path)
	} else {
		folder, err = browseLocal(cfg, body.Path)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		writeJSON(w, http.StatusOK, folder)
	}
}

// browseRemote lists the folders in a folder of a stored server.
func browseRemote(ctx context.Context, cfg config.Config, server, at string) (syncFolderJSON, error) {
	fsys, _, err := openSyncServer(ctx, cfg, server)
	if err != nil {
		return syncFolderJSON{}, err
	}
	defer func() { _ = fsys.Close() }()
	folder, err := fsys.Resolve(at)
	if err != nil {
		return syncFolderJSON{}, err
	}
	infos, err := fsys.ReadDir(folder)
	if err != nil {
		return syncFolderJSON{}, err
	}
	out := syncFolderJSON{Path: folder, Folders: []string{}}
	if folder != "/" {
		out.Parent = path.Dir(folder)
	}
	for _, info := range infos {
		if info.IsDir() && !remote.IsLink(info) {
			out.Folders = append(out.Folders, info.Name())
		}
	}
	return capped(out), nil
}

// browseLocal lists the folders in a folder of the served folder, as the
// file listing names it.
func browseLocal(cfg config.Config, at string) (syncFolderJSON, error) {
	if strings.TrimSpace(at) == "" {
		at = "/"
	}
	folder, err := config.SyncFolder(at)
	if err != nil {
		return syncFolderJSON{}, err
	}
	entries, err := os.ReadDir(config.RepositoryPath(cfg.General.BaseFolderFiles, folder))
	if err != nil {
		return syncFolderJSON{}, fmt.Errorf("%s: %w", folder, err)
	}
	out := syncFolderJSON{Path: folder, Folders: []string{}}
	if folder != "/" {
		out.Parent = path.Dir(folder)
	}
	// a link is left out, as a sync leaves it out
	for _, entry := range entries {
		if entry.IsDir() {
			out.Folders = append(out.Folders, entry.Name())
		}
	}
	return capped(out), nil
}

// capped sorts the folders and keeps as many as a browse lists.
func capped(folder syncFolderJSON) syncFolderJSON {
	slices.SortFunc(folder.Folders, func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	if len(folder.Folders) > maxBrowsed {
		folder.Folders = folder.Folders[:maxBrowsed]
		folder.Truncated = true
	}
	return folder
}

// handleSyncRun runs a sync now, rather than at its next time.
func (h *Handler) handleSyncRun(w http.ResponseWriter, r *http.Request) {
	var body syncRunBody
	if !h.decodeSync(w, r, &body) {
		return
	}
	err := syncer.Trigger(body.Name)
	switch {
	case errors.Is(err, syncer.ErrNotRunning):
		http.Error(w, fmt.Sprintf("the sync %q is not running: it is disabled, or go-fs has not read the file since it was stored",
			body.Name), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	h.log.Info("the admin interface ran a sync", "sync", body.Name, "address", addressOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"name": body.Name})
}
