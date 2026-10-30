package httpd

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// The Trash page shows what the trash holds, the way the listing shows a
// folder, and restores it or deletes it for good. It is served under the
// go-fs marker like the registry page, on the path of the trash it shows, so
// it reserves no name in the served folder.
//
// Who may use it is an account that sets isAdmin, with a session, as for the
// admin interface: what is in the trash has left the served folder and the
// paths an account is limited to, so nobody else may see it. It does not
// depend on the admin interface being on.

const (
	actionTrash        = "trash"
	actionTrashRestore = "trash-restore"
	actionTrashDelete  = "trash-delete"
	actionTrashEmpty   = "trash-empty"
)

// trashPageMethods are the methods each endpoint of the page answers.
var trashPageMethods = map[string]string{
	actionTrash:        "GET, HEAD",
	actionTrashRestore: "POST",
	actionTrashDelete:  "POST",
	actionTrashEmpty:   "POST",
}

// trashPageAction reports which trash endpoint a request is for, or "" for
// any other request.
func trashPageAction(r *http.Request) string {
	switch action := r.URL.Query().Get(sessionParam); action {
	case actionTrash, actionTrashRestore, actionTrashDelete, actionTrashEmpty:
		return action
	default:
		return ""
	}
}

// offersTrash reports whether the request behind cred may use the trash.
func offersTrash(set *settings, cred credential) bool {
	return trashOn(set) && cred.token && cred.user != nil && cred.user.isAdmin
}

// trashURL is the Trash page at the top of the trash.
func trashURL() string {
	return "/?" + sessionParam + "=" + actionTrash
}

// handleTrash guards the Trash page and its endpoints and hands the request
// over.
func (s *Server) handleTrash(set *settings, w http.ResponseWriter, r *http.Request, action string) {
	if !trashOn(set) {
		// switched off, the marker names nothing
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Cache-Control", "no-store")
	cred := s.identify(set, w, r)
	if !cred.token || cred.user == nil {
		s.log.Info("http trash refused, no session", "credentials", cred.method,
			"address", clientAddress(set, r))
		if action == actionTrash && browserRequest(r) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, loginURL(r.URL), http.StatusSeeOther)
			return
		}
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	if !cred.user.isAdmin {
		s.log.Warn("http trash refused, the account is not an admin",
			"user", cred.user.name, "address", clientAddress(set, r))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := os.MkdirAll(set.cfg.TrashFolder, 0o700); err != nil {
		s.log.Error("http cannot make the trash folder", "path", set.cfg.TrashFolder, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	root, err := vfs.New(set.cfg.TrashFolder)
	if err != nil {
		s.log.Error("http cannot open the trash folder", "path", set.cfg.TrashFolder, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	folder := root.Resolve("/", r.URL.Path)
	if !folder.Valid || isTrashIndex(folder.Virtual) {
		http.NotFound(w, r)
		return
	}

	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case action == actionTrash && read:
		s.trashPage(set, w, r, cred, folder)
	case action == actionTrashRestore && r.Method == http.MethodPost:
		if !s.sameSite(set, w, r) {
			return
		}
		s.trashRestore(set, w, r, cred.user, folder)
	case action == actionTrashDelete && r.Method == http.MethodPost:
		if !s.sameSite(set, w, r) {
			return
		}
		s.trashDelete(set, w, r, cred.user, folder)
	case action == actionTrashEmpty && r.Method == http.MethodPost:
		if !s.sameSite(set, w, r) {
			return
		}
		s.trashEmpty(set, w, r, cred.user)
	default:
		w.Header().Set("Allow", trashPageMethods[action])
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// --- the page ---------------------------------------------------------------

type trashRow struct {
	Name     string
	Label    string
	Link     string
	IsDir    bool
	Group    string
	Bytes    int64
	Unix     int64
	Kind     string
	Size     string
	Modified string
	Deleted  string
}

type trashData struct {
	Title   string
	Crumbs  []crumb
	Parent  string
	Columns []column
	Entries []trashRow
	// Base is the path the script sends its requests to.
	Base string
	// Retention is how many days the trash keeps what is in it, 0 for ever.
	Retention int
	Session   sessionView
	Nonce     string
	Style     template.CSS
	Script    template.JS
}

var trashTemplate = template.Must(template.ParseFS(assets, "assets/trash.html", "assets/partials.html"))

var trashScript = template.JS(mustRead("assets/trash.js"))

// trashPage renders a folder of the trash.
func (s *Server) trashPage(set *settings, w http.ResponseWriter, r *http.Request, cred credential, folder vfs.Target) {
	info, err := os.Stat(folder.Path)
	if err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/") {
		// the rows are relative links, which only work below a folder's slash
		http.Redirect(w, r, (&url.URL{Path: vfs.AsFolder(folder.Virtual), RawQuery: r.URL.RawQuery}).RequestURI(),
			http.StatusMovedPermanently)
		return
	}
	entries, err := readDirectory(folder.Path)
	if err != nil {
		s.log.Error("http cannot read the trash", "path", folder.Virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	index := func() trashIndex {
		s.trashMu.Lock()
		defer s.trashMu.Unlock()
		return readTrashIndex(set.cfg.TrashFolder)
	}()
	order := parseSort(r.URL.Query())
	kept := sessionParam + "=" + actionTrash
	rows := make([]trashRow, 0, len(entries))
	for _, item := range sortEntries(entries, order) {
		virtual := path.Join(folder.Virtual, item.bare())
		if folder.Virtual == "/" && (item.bare() == trashIndexName || strings.HasPrefix(item.bare(), trashIndexName+".")) {
			continue
		}
		deleted := index.deleted(virtual)
		if deleted.IsZero() {
			deleted = item.ModTime
		}
		row := trashRow{
			Name:     item.bare(),
			Label:    item.Name,
			IsDir:    item.isFolder(),
			Group:    "0",
			Bytes:    sizeOf(item),
			Unix:     item.ModTime.Unix(),
			Kind:     kindOf(item),
			Size:     readableSize(item.Size),
			Modified: item.ModTime.Format("2006-01-02 15:04"),
			Deleted:  deleted.Format("2006-01-02 15:04"),
		}
		if row.IsDir {
			row.Group = "1"
			row.Size = "—"
			row.Link = (&url.URL{Path: item.Name, RawQuery: kept}).String()
		}
		rows = append(rows, row)
	}

	nonce, err := pageNonce()
	if err != nil {
		s.log.Error("http cannot render the trash page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	who := s.sessionViewFor(set, r, cred)
	who.Page, who.Trash, who.Files = "Trash", "", liveFolderLink(s.root, folder.Virtual)
	data := trashData{
		Title:     vfs.AsFolder(folder.Virtual) + " · Trash",
		Crumbs:    trashCrumbs(folder.Virtual),
		Columns:   columnsOf(order, kept),
		Entries:   rows,
		Base:      (&url.URL{Path: vfs.AsFolder(folder.Virtual)}).EscapedPath(),
		Retention: set.cfg.TrashRetentionDays,
		Session:   who,
		Nonce:     nonce,
		Style:     listingStyle,
		Script:    trashScript,
	}
	if parent := parentOf(folder.Virtual); parent != "" {
		data.Parent = parent + "?" + kept
	}

	var page bytes.Buffer
	if err := trashTemplate.Execute(&page, data); err != nil {
		s.log.Error("http cannot render the trash page", "error", err)
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

// trashCrumbs are the crumbs of a folder of the trash, every one of them a
// link to the Trash page.
func trashCrumbs(virtual string) []crumb {
	crumbs := crumbsOf(virtual)
	for i := range crumbs {
		if crumbs[i].Link != "" {
			crumbs[i].Link += "?" + sessionParam + "=" + actionTrash
		}
	}
	return crumbs
}

// liveFolderLink is the Files link of a folder of the trash: the same folder
// of the served tree while it is there, and the top of it otherwise.
func liveFolderLink(root *vfs.Root, virtual string) string {
	for at := virtual; at != "/" && at != "."; at = path.Dir(at) {
		target := root.Resolve("/", at)
		if info, err := os.Stat(target.Path); target.Valid && err == nil && info.IsDir() {
			return (&url.URL{Path: vfs.AsFolder(at)}).EscapedPath()
		}
	}
	return "/"
}

// --- what the page asks for -------------------------------------------------

// maxTrashRequest bounds the body of a restore or a delete: a name and a flag.
const maxTrashRequest = 16 << 10

type trashRequest struct {
	Name      string `json:"name"`
	Overwrite bool   `json:"overwrite"`
}

type trashRestoreJSON struct {
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
}

type trashEmptyJSON struct {
	Items int `json:"items"`
}

// readTrashRequest decodes the body and resolves the name in it, which is
// one entry of the folder the request was sent to.
func readTrashRequest(w http.ResponseWriter, r *http.Request, folder vfs.Target) (trashRequest, string, bool) {
	var asked trashRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxTrashRequest)).Decode(&asked); err != nil {
		http.Error(w, "The request is not valid JSON.", http.StatusBadRequest)
		return asked, "", false
	}
	if asked.Name == "" || asked.Name == "." || asked.Name == ".." || strings.ContainsAny(asked.Name, "/\\") {
		http.Error(w, "That is not a name in this folder.", http.StatusBadRequest)
		return asked, "", false
	}
	virtual := path.Join(folder.Virtual, asked.Name)
	if isTrashIndex(virtual) {
		http.NotFound(w, r)
		return asked, "", false
	}
	return asked, virtual, true
}

// trashRestore puts an entry of the trash back where it was deleted from.
func (s *Server) trashRestore(set *settings, w http.ResponseWriter, r *http.Request, user *account, folder vfs.Target) {
	asked, virtual, ok := readTrashRequest(w, r, folder)
	if !ok {
		return
	}
	restored, skipped, err := s.restoreFromTrash(set, virtual, asked.Overwrite)
	if errors.Is(err, errNotInTrash) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("trash restore failed", "user", user.name, "path", virtual, "error", err)
		http.Error(w, "The restore did not finish: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("trash restore", "user", user.name, "path", virtual, "overwrite", asked.Overwrite,
		"restored", restored, "skipped", skipped, "address", clientAddress(set, r))
	writeJSON(w, http.StatusOK, trashRestoreJSON{Restored: restored, Skipped: skipped})
}

// trashDelete deletes an entry of the trash for good.
func (s *Server) trashDelete(set *settings, w http.ResponseWriter, r *http.Request, user *account, folder vfs.Target) {
	_, virtual, ok := readTrashRequest(w, r, folder)
	if !ok {
		return
	}
	err := s.deleteFromTrash(set, virtual)
	if errors.Is(err, errNotInTrash) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("trash delete failed", "user", user.name, "path", virtual, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	s.log.Info("trash delete", "user", user.name, "path", virtual, "address", clientAddress(set, r))
	writeJSON(w, http.StatusOK, struct{}{})
}

// trashEmpty deletes everything in the trash for good.
func (s *Server) trashEmpty(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	started := time.Now()
	items, err := s.emptyTrash(set)
	if err != nil {
		s.log.Error("trash empty failed", "user", user.name, "items", items, "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	s.log.Info("trash empty", "user", user.name, "items", items,
		"took", time.Since(started).Round(time.Millisecond), "address", clientAddress(set, r))
	writeJSON(w, http.StatusOK, trashEmptyJSON{Items: items})
}
