package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/ociauth"
)

// A container registry is reached by the distribution API of OCI, under
// /v2/ of its address, and only ever read: the registry page pulls its
// images. It is browsed as a tree of folders, one for each component of the
// names of its repositories, with the tags of a repository as the files of
// its folder:
//
//	GET  v2/_catalog?n=…&last=…            the repositories, where the registry lists them
//	GET  v2/<repository>/tags/list?n=…     the tags of a repository
//	HEAD v2/<repository>/manifests/<tag>   whether a tag is there, and the size of its manifest
//	GET  v2/<repository>/manifests/<tag>   the manifest, which is what a tag reads as
//
// Docker Hub and the GitHub Container Registry list no catalog: the top of
// such a registry cannot be listed, but a repository named in full is, by
// its tags.

// ErrNoCatalog is what listing the top of a registry fails with when the
// registry lists no catalog: a repository has to be named instead.
var ErrNoCatalog = errors.New("the registry does not list its repositories; open one by its name")

// maxRegistryNames bounds the repositories of a catalog, and the tags of a
// repository, that are listed.
var maxRegistryNames = 20000

// registryPage is how many names one request of a listing asks for.
const registryPage = 1000

// catalogLifetime is how long a catalog is kept: every page of a stored
// server logs in to it again, and a catalog of thousands of repositories
// takes many requests.
const catalogLifetime = time.Minute

// The manifests a tag may name, as the registry page stores them.
var registryManifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// maxRegistryManifest bounds a manifest that is read, as the registry page
// bounds one it pulls.
const maxRegistryManifest = 4 << 20

// registryTag is a tag as the distribution API allows one.
var registryTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// registryRepository is a repository name as the distribution API allows it.
var registryRepository = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

// RegistryCredentials are what a login to a container registry sends.
func (l Login) RegistryCredentials() ociauth.Credentials {
	switch l.Auth {
	case config.RegistryAuthBasic:
		return ociauth.Credentials{Username: l.Username, Password: l.Password}
	case config.RegistryAuthBearer:
		return ociauth.Credentials{Username: l.Username, Token: l.Token}
	}
	return ociauth.Credentials{}
}

// RegistryTLS is what the connection of a login to a container registry
// trusts and shows.
func (l Login) RegistryTLS() ociauth.TLS {
	return ociauth.TLS{ClientCert: l.ClientCert, ClientKey: l.ClientKey, CACert: l.CACert, SkipVerify: l.SkipVerify}
}

// checkedRegistry reads a login to a container registry.
func (l Login) checkedRegistry(keyOnly bool) (Login, error) {
	if keyOnly {
		return Login{}, errors.New("a container registry shows no key")
	}
	address, err := config.RegistryURL(l.URL)
	if err != nil {
		return Login{}, err
	}
	l.URL = address
	l.Host, l.Port, l.HostKey, l.VaultPath, l.VaultPassword = "", 0, "", "", ""
	l.Username, l.Token = strings.TrimSpace(l.Username), strings.TrimSpace(l.Token)
	switch l.Auth {
	case "", config.RegistryAuthNone:
		l.Auth, l.Username, l.Password, l.Token = config.RegistryAuthNone, "", "", ""
	case config.RegistryAuthBasic:
		if l.Username == "" {
			return Login{}, errors.New("name the user to log in as")
		}
		l.Token = ""
	case config.RegistryAuthBearer:
		if l.Token == "" {
			return Login{}, errors.New("paste the token to log in with")
		}
		l.Password = ""
	default:
		return Login{}, fmt.Errorf("%q is not a way to log in to a registry: none, basic or bearer", l.Auth)
	}
	if (l.ClientCert == "") != (l.ClientKey == "") {
		return Login{}, errors.New("a client certificate needs its key, and a key its certificate")
	}
	if _, err := l.RegistryTLS().Transport(); err != nil {
		return Login{}, err
	}
	return l, nil
}

// registryFS is a container registry, logged in to.
type registryFS struct {
	ctx   context.Context
	login Login
	auth  *ociauth.Authorizer
	// truncated says the last ReadDir left names out
	truncated bool
}

func openRegistry(ctx context.Context, l Login) (FS, error) {
	transport, err := l.RegistryTLS().Transport()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: logging.Transport(logOf(ctx), "registry", transport),
		CheckRedirect: ociauth.KeepCredentialsHome}
	return &registryFS{ctx: ctx, login: l, auth: ociauth.NewAuthorizer(client, l.Shown(), l.RegistryCredentials())}, nil
}

// get sends a request with the authorization there is, and meets a
// challenge once, with a token for scope.
func (r *registryFS) get(method, target, scope string, header http.Header) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(r.ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		for key, values := range header {
			req.Header[key] = values
		}
		if r.auth.Header != "" {
			req.Header.Set("Authorization", r.auth.Header)
		}
		req.Header.Set("User-Agent", "go-fs")
		res, err := r.auth.Do(req)
		if err != nil {
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return nil, fmt.Errorf("%s: %w", r.login.Shown(), err)
		}
		challenge := res.Header.Get("WWW-Authenticate")
		if res.StatusCode != http.StatusUnauthorized || attempt > 0 || challenge == "" {
			return res, nil
		}
		ociauth.Drain(res)
		if err := r.auth.Answer(r.ctx, challenge, scope); err != nil {
			// a challenge that cannot be met refuses what was asked for:
			// Docker Hub's token service answers the scope of the catalog
			// with 400, unknown resource type
			return nil, fmt.Errorf("%w (%w)", err, fs.ErrPermission)
		}
	}
}

// failure reads an answer that is not a success, wrapping fs.ErrNotExist
// and fs.ErrPermission where it is one of those.
func (r *registryFS) failure(res *http.Response, what string) error {
	said := ociauth.Failure(res)
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s refused %s (%v): %w", r.login.Shown(), what, said, fs.ErrPermission)
	case http.StatusNotFound:
		return fmt.Errorf("%s has no %s (%v): %w", r.login.Shown(), what, said, fs.ErrNotExist)
	}
	return fmt.Errorf("%s: %s: %w", r.login.Shown(), what, said)
}

// listing reads every page of a listing of names, _catalog or tags/list,
// at most maxRegistryNames of them. The next page is the one the Link of an
// answer names, asked for at target with that page's query: an Artifactory
// whose API is under a path links to /v2/ without it.
func (r *registryFS) listing(target, scope, what, field string) (names []string, truncated bool, err error) {
	query := url.Values{"n": {fmt.Sprint(registryPage)}}
	seen := map[string]bool{}
	for {
		res, err := r.get(http.MethodGet, target+"?"+query.Encode(), scope, nil)
		if err != nil {
			return nil, false, err
		}
		if res.StatusCode != http.StatusOK {
			defer ociauth.Drain(res)
			return nil, false, r.failure(res, what)
		}
		var answer map[string]json.RawMessage
		err = json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(&answer)
		next := res.Header.Get("Link")
		ociauth.Drain(res)
		if err != nil {
			return nil, false, fmt.Errorf("%s does not answer as a registry for %s: %w", r.login.Shown(), what, err)
		}
		var page []string
		if raw, ok := answer[field]; ok && string(raw) != "null" {
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, false, fmt.Errorf("%s does not answer as a registry for %s: %w", r.login.Shown(), what, err)
			}
		}
		added := 0
		for _, name := range page {
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
				added++
			}
		}
		if len(names) >= maxRegistryNames {
			sort.Strings(names)
			return names[:maxRegistryNames], true, nil
		}
		nextQuery, ok := linkedPage(next)
		if !ok || added == 0 {
			sort.Strings(names)
			return names, false, nil
		}
		query = nextQuery
	}
}

// linkedPage is the query of the page a Link header names as the next one.
func linkedPage(header string) (url.Values, bool) {
	for _, link := range strings.Split(header, ",") {
		target, params, _ := strings.Cut(strings.TrimSpace(link), ";")
		if !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) &&
			!strings.Contains(strings.ReplaceAll(params, " ", ""), "rel=next") {
			continue
		}
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		u, err := url.Parse(target)
		if err != nil {
			return nil, false
		}
		query := u.Query()
		if query.Get("last") == "" {
			return nil, false
		}
		return query, true
	}
	return nil, false
}

// registryCatalog is the catalog of a registry, as it was asked for at a
// time.
type registryCatalog struct {
	names     []string
	truncated bool
	// err is why there is none: ErrNoCatalog for a registry that lists none
	err error
	at  time.Time
}

var catalogs = struct {
	sync.Mutex
	byLogin map[[32]byte]*registryCatalog
}{byLogin: map[[32]byte]*registryCatalog{}}

// catalogKey tells the logins apart whose catalogs may differ.
func (l Login) catalogKey() [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%q %q %q %q %q %q %q %q", l.Server, l.URL, l.Auth, l.Username,
		l.Password, l.Token, l.ClientCert, l.CACert)))
}

// catalog is the catalog of the registry, from the last minute where it was
// asked for then.
func (r *registryFS) catalog() (*registryCatalog, error) {
	key := r.login.catalogKey()
	catalogs.Lock()
	found := catalogs.byLogin[key]
	catalogs.Unlock()
	if found != nil && time.Since(found.at) < catalogLifetime {
		return found, found.err
	}
	names, truncated, err := r.listing(r.login.URL+"/v2/_catalog", "registry:catalog:*", "the catalog", "repositories")
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, fs.ErrNotExist) {
			// a failure of the connection or the login is not kept
			return nil, err
		}
		err = fmt.Errorf("%s: %w (%w)", r.login.Shown(), ErrNoCatalog, err)
	}
	found = &registryCatalog{names: names, truncated: truncated, err: err, at: time.Now()}
	catalogs.Lock()
	for k, c := range catalogs.byLogin {
		if time.Since(c.at) >= catalogLifetime {
			delete(catalogs.byLogin, k)
		}
	}
	catalogs.byLogin[key] = found
	catalogs.Unlock()
	return found, err
}

// ForgetCatalogs drops the catalogs that are kept, once a stored server was
// changed: its repositories are listed anew.
func ForgetCatalogs() {
	catalogs.Lock()
	defer catalogs.Unlock()
	clear(catalogs.byLogin)
}

// contains reports whether the catalog holds a repository or a namespace
// that is name.
func (c *registryCatalog) contains(name string) (repository, namespace bool) {
	i := sort.SearchStrings(c.names, name)
	if i < len(c.names) && c.names[i] == name {
		repository = true
	}
	j := sort.SearchStrings(c.names, name+"/")
	namespace = j < len(c.names) && strings.HasPrefix(c.names[j], name+"/")
	return repository, namespace
}

// folders are the components of the catalog under prefix, a folder each.
func (c *registryCatalog) folders(prefix string) []string {
	var found []string
	start := 0
	if prefix != "" {
		prefix += "/"
		start = sort.SearchStrings(c.names, prefix)
	}
	for _, name := range c.names[start:] {
		if !strings.HasPrefix(name, prefix) {
			break
		}
		component, _, _ := strings.Cut(name[len(prefix):], "/")
		if component != "" && (len(found) == 0 || found[len(found)-1] != component) {
			found = append(found, component)
		}
	}
	return found
}

// RegistryTruncated reports whether the last listing of a folder of fsys,
// a registry, left names out.
func RegistryTruncated(fsys FS) bool {
	if r, ok := unwrapRegistry(fsys); ok {
		return r.truncated
	}
	return false
}

func unwrapRegistry(fsys FS) (*registryFS, bool) {
	for {
		switch f := fsys.(type) {
		case *registryFS:
			return f, true
		case tracedFS:
			fsys = f.FS
		default:
			return nil, false
		}
	}
}

// tags are the tags of a repository.
func (r *registryFS) tags(repository string) ([]string, bool, error) {
	return r.listing(r.login.URL+"/v2/"+escaped(repository)+"/tags/list", "repository:"+repository+":pull",
		repository, "tags")
}

// manifest asks for the manifest of a tag.
func (r *registryFS) manifest(method, repository, tag string) (*http.Response, error) {
	res, err := r.get(method, r.login.URL+"/v2/"+escaped(repository)+"/manifests/"+tag,
		"repository:"+repository+":pull", http.Header{"Accept": {registryManifestAccept}})
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer ociauth.Drain(res)
		return nil, r.failure(res, repository+":"+tag)
	}
	return res, nil
}

// registryInfo is a folder or a tag of a registry.
type registryInfo struct {
	name string
	size int64
	dir  bool
}

func (i registryInfo) Name() string       { return i.name }
func (i registryInfo) Size() int64        { return i.size }
func (i registryInfo) ModTime() time.Time { return time.Time{} }
func (i registryInfo) IsDir() bool        { return i.dir }
func (i registryInfo) Sys() any           { return nil }
func (i registryInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (r *registryFS) Resolve(p string) (string, error) {
	return path.Clean("/" + strings.TrimSpace(p)), nil
}

// name is a path as the name of a repository.
func name(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }

func (r *registryFS) Stat(p string) (fs.FileInfo, error) {
	full := name(p)
	if full == "" {
		// the login alone: a registry that lists no catalog is reached all
		// the same
		if err := r.auth.Authenticate(r.ctx, r.login.URL, ""); err != nil {
			return nil, err
		}
		return registryInfo{name: "/", dir: true}, nil
	}
	if catalog, err := r.catalog(); err == nil {
		if repository, namespace := catalog.contains(full); repository || namespace {
			return registryInfo{name: path.Base(full), dir: true}, nil
		}
		parent := path.Dir(full)
		if repository, _ := catalog.contains(parent); !repository || !registryTag.MatchString(path.Base(full)) {
			return nil, fmt.Errorf("%s has no %s: %w", r.login.Shown(), full, fs.ErrNotExist)
		}
	} else if !errors.Is(err, ErrNoCatalog) {
		return nil, err
	} else if registryRepository.MatchString(full) {
		// without a catalog, a repository is what lists tags
		if _, _, err := r.tags(full); err == nil {
			return registryInfo{name: path.Base(full), dir: true}, nil
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
	}
	repository, tag := path.Dir(full), path.Base(full)
	if repository == "." || !registryTag.MatchString(tag) || !registryRepository.MatchString(repository) {
		return nil, fmt.Errorf("%s has no %s: %w", r.login.Shown(), full, fs.ErrNotExist)
	}
	res, err := r.manifest(http.MethodHead, repository, tag)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// what a registry may not show is what it does not have
			return nil, fmt.Errorf("%s has no repository %s, or refuses it to this login: %w",
				r.login.Shown(), full, fs.ErrNotExist)
		}
		return nil, err
	}
	ociauth.Drain(res)
	return registryInfo{name: tag, size: max(res.ContentLength, 0)}, nil
}

func (r *registryFS) Lstat(p string) (fs.FileInfo, error) { return r.Stat(p) }

func (r *registryFS) ReadDir(p string) ([]fs.FileInfo, error) {
	full := name(p)
	var infos []fs.FileInfo
	folders := map[string]bool{}
	repository := full != "" && registryRepository.MatchString(full)
	catalog, err := r.catalog()
	switch {
	case err == nil:
		for _, folder := range catalog.folders(full) {
			folders[folder] = true
			infos = append(infos, registryInfo{name: folder, dir: true})
		}
		r.truncated = catalog.truncated
		repository, _ = catalog.contains(full)
	case full == "" || !errors.Is(err, ErrNoCatalog):
		return nil, err
	}
	if repository {
		tags, truncated, err := r.tags(full)
		if err != nil {
			return nil, err
		}
		r.truncated = r.truncated || truncated
		for _, tag := range tags {
			// a tag that is a folder's name too is left to the folder
			if !folders[tag] {
				infos = append(infos, registryInfo{name: tag})
			}
		}
	}
	return infos, nil
}

func (r *registryFS) Open(p string) (io.ReadSeekCloser, error) {
	full := name(p)
	repository, tag := path.Dir(full), path.Base(full)
	if repository == "." || !registryTag.MatchString(tag) {
		return nil, fmt.Errorf("%s is not a tag: %w", full, fs.ErrNotExist)
	}
	res, err := r.manifest(http.MethodGet, repository, tag)
	if err != nil {
		return nil, err
	}
	defer ociauth.Drain(res)
	body, err := io.ReadAll(io.LimitReader(res.Body, maxRegistryManifest+1))
	if err != nil {
		return nil, fmt.Errorf("%s: the manifest of %s could not be read: %w", r.login.Shown(), full, err)
	}
	if len(body) > maxRegistryManifest {
		return nil, fmt.Errorf("%s: the manifest of %s is larger than 4 MiB", r.login.Shown(), full)
	}
	return nopSeekCloser{bytes.NewReader(body)}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// errRegistryReadOnly is what changing a registry fails with.
var errRegistryReadOnly = fmt.Errorf("a container registry is only pulled from: %w", fs.ErrPermission)

func (r *registryFS) Put(string, io.Reader, int64) (int64, error) { return 0, errRegistryReadOnly }
func (r *registryFS) Mkdir(string) error                          { return errRegistryReadOnly }
func (r *registryFS) Rename(string, string) error                 { return errRegistryReadOnly }
func (r *registryFS) Remove(string) error                         { return errRegistryReadOnly }
func (r *registryFS) RemoveDir(string) error                      { return errRegistryReadOnly }
func (r *registryFS) Close() error                                { return nil }
