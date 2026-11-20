package httpd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote"
)

// The Pull image dialog pulls either from an image typed in with its
// registry and a login, or from a container registry that is a stored
// server, which the dialog browses for the image: its repositories by the
// catalog, or one named where it lists none, and their tags. What the
// browser is sent of a stored registry is its name and what it holds, never
// its login.

// isRegistryServer reports a stored server that is a container registry.
func isRegistryServer(server config.Server) bool {
	return server.Type == config.ServerTypeRegistry
}

// registrySources are the stored registries the account may pull from, by
// name: those that admit it and allow downloads.
func registrySources(set *settings, user *account) []string {
	var names []string
	for _, server := range serversFor(set, user) {
		if isRegistryServer(server) && server.Rights().Download {
			names = append(names, server.Name)
		}
	}
	return names
}

// errNoSource is a stored registry that is not there for the account.
var errNoSource = errors.New("there is no such registry")

// registrySource finds a stored registry the account may pull from, and
// logs in with what it stores.
func registrySource(set *settings, user *account, name string) (config.Server, remote.Login, error) {
	server, ok := config.FindServer(serversFor(set, user), name)
	if !ok || !isRegistryServer(server) || !server.Rights().Download {
		return config.Server{}, remote.Login{}, fmt.Errorf("%w: %q", errNoSource, name)
	}
	login, err := remote.FromServer(server).Checked(false)
	if err != nil {
		return config.Server{}, remote.Login{}, fmt.Errorf("the registry %s: %w", server.Name, err)
	}
	return server, login, nil
}

// storedReference reads repository:tag of a stored registry as the image it
// is there.
func storedReference(login remote.Login, value string) (remoteRef, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "/")
	at := strings.LastIndex(value, ":")
	if at < 0 || at < strings.LastIndex(value, "/") {
		return remoteRef{}, errors.New("choose a tag of the image")
	}
	repository, tag := value[:at], value[at+1:]
	if !validRepository(repository) {
		return remoteRef{}, fmt.Errorf("%q is not a repository name: lowercase letters, digits and separators only",
			repository)
	}
	if !tagPattern.MatchString(tag) {
		return remoteRef{}, fmt.Errorf("%q is not a tag", tag)
	}
	u, err := url.Parse(login.URL)
	if err != nil {
		return remoteRef{}, err
	}
	return remoteRef{scheme: u.Scheme, host: u.Host, api: u.Host + u.EscapedPath(), repository: repository, tag: tag}, nil
}

// pullSource is the image a pull or its check is for, and how its registry
// is reached: the image typed in with the login given, or one of a stored
// registry with the login it stores. status is what a failure is answered
// with.
func pullSource(set *settings, user *account, body transferBody) (ref remoteRef, login remoteLogin, status int, err error) {
	if body.Server == "" {
		ref, err = pullReference(body.Reference)
		return ref, body.login(), http.StatusBadRequest, err
	}
	_, stored, err := registrySource(set, user, body.Server)
	if errors.Is(err, errNoSource) {
		return remoteRef{}, remoteLogin{}, http.StatusNotFound, err
	}
	if err != nil {
		return remoteRef{}, remoteLogin{}, http.StatusBadGateway, err
	}
	ref, err = storedReference(stored, body.Reference)
	return ref, remoteLogin{credentials: stored.RegistryCredentials(), tls: stored.RegistryTLS()},
		http.StatusBadRequest, err
}

// registryBrowseBody asks for a folder of a stored registry.
type registryBrowseBody struct {
	Server string `json:"server"`
	Path   string `json:"path"`
}

// registryBrowseEntry is a folder of a registry, or a tag.
type registryBrowseEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// registryBrowseJSON is a folder of a stored registry: the folders under
// it, and the tags where it is a repository.
type registryBrowseJSON struct {
	Server  string                `json:"server"`
	Path    string                `json:"path"`
	Parent  string                `json:"parent"`
	Entries []registryBrowseEntry `json:"entries"`
	// Truncated says there were more than are listed.
	Truncated bool `json:"truncated"`
	// NoCatalog says the registry lists no repositories, so one is opened
	// by its name.
	NoCatalog bool `json:"noCatalog"`
}

// registryBrowse lists a folder of a stored registry for the Pull image
// dialog.
func (s *Server) registryBrowse(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	var body registryBrowseBody
	if !s.readTransferRequest(set, w, r, user, &body) {
		return
	}
	server, login, err := registrySource(set, user, body.Server)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNoSource) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	fsys, err := remote.Open(ctx, login, set.ssh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = fsys.Close() }()

	p, _ := fsys.Resolve(body.Path)
	answer := registryBrowseJSON{Server: server.Name, Path: p, Entries: []registryBrowseEntry{}}
	if p != "/" {
		answer.Parent = path.Dir(p)
	}
	{
		// the top is the login alone, so that a wrong one is said as such
		// rather than as a registry without a catalog
		info, err := fsys.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			http.Error(w, fmt.Sprintf("%s has no repository %s", server.Name, strings.TrimPrefix(p, "/")),
				http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		case !info.IsDir():
			http.Error(w, fmt.Sprintf("%s is a tag, not a folder", strings.TrimPrefix(p, "/")), http.StatusBadRequest)
			return
		}
	}
	infos, err := fsys.ReadDir(p)
	switch {
	case errors.Is(err, remote.ErrNoCatalog):
		answer.NoCatalog = true
	case err != nil:
		s.logFor(r).Info("registry browse failed", "server", server.Name, "path", p, "user", user.name,
			"address", clientAddress(set, r), "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, info := range infos {
		answer.Entries = append(answer.Entries, registryBrowseEntry{Name: info.Name(), Dir: info.IsDir()})
	}
	answer.Truncated = remote.RegistryTruncated(fsys)
	writeJSON(w, http.StatusOK, answer)
}
