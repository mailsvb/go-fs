package remotetest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// RegistryHost is a container registry a test browses, as far as go-fs reads
// one: the catalog and the tags, a page at a time with a Link to the next,
// and manifests. It logs in the way docker's registries do, with a challenge
// for a token of its token service, which takes Basic with Username and
// Password, or Token as a Bearer token.
type RegistryHost struct {
	// URL is where its API is, under Prefix: URL/v2/.
	URL string
	// Prefix is the path its API is under, as an Artifactory has the API of
	// one repository: /artifactory/api/docker/<repo>.
	Prefix             string
	Username, Password string
	Token              string
	// NoCatalog refuses the catalog, as Docker Hub does.
	NoCatalog atomic.Bool
	// TokenRefusesCatalog has the token service refuse the scope of the
	// catalog with 400, as Docker Hub's does.
	TokenRefusesCatalog atomic.Bool
	// LinkWithoutPrefix links the next page without Prefix, as an
	// Artifactory does.
	LinkWithoutPrefix bool
	// CatalogRequests counts the pages of the catalog that were asked for.
	CatalogRequests atomic.Int32

	mu sync.Mutex
	// images are the manifests of each repository, by tag
	images map[string]map[string][]byte
	// scopes are those the token service handed tokens out for
	scopes []string
}

// NewRegistryHost starts a registry that takes the login given, anonymous
// when both are empty, under prefix, and stops it when the test ends.
func NewRegistryHost(t testing.TB, prefix, username, password, token string) *RegistryHost {
	t.Helper()
	host := &RegistryHost{Prefix: prefix, Username: username, Password: password, Token: token,
		images: map[string]map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(host.serve))
	t.Cleanup(server.Close)
	host.URL = server.URL + prefix
	return host
}

// Tag puts a manifest under a tag of a repository.
func (h *RegistryHost) Tag(repository, tag, manifest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.images[repository] == nil {
		h.images[repository] = map[string][]byte{}
	}
	h.images[repository][tag] = []byte(manifest)
}

// Scopes are the scopes the token service handed tokens out for.
func (h *RegistryHost) Scopes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.scopes)
}

func (h *RegistryHost) anonymous() bool {
	return h.Username == "" && h.Token == ""
}

// issued is the token the token service hands out.
const issued = "issued-token"

func (h *RegistryHost) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		h.serveToken(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, h.Prefix+"/v2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !h.anonymous() && r.Header.Get("Authorization") != "Bearer "+issued {
		realm := "http://" + r.Host + "/token"
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test"`, realm))
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case rest == "":
		w.WriteHeader(http.StatusOK)
	case rest == "_catalog" && h.TokenRefusesCatalog.Load():
		// no token it hands out is good for the catalog
		realm := "http://" + r.Host + "/token"
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="test",scope="registry:catalog:*"`, realm))
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	case rest == "_catalog":
		h.CatalogRequests.Add(1)
		if h.NoCatalog.Load() {
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "the catalog is not listed")
			return
		}
		var names []string
		for name := range h.images {
			names = append(names, name)
		}
		h.page(w, r, "/v2/_catalog", "repositories", names)
	case strings.HasSuffix(rest, "/tags/list"):
		repository := strings.TrimSuffix(rest, "/tags/list")
		tags, ok := h.images[repository]
		if !ok {
			writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
			return
		}
		var names []string
		for tag := range tags {
			names = append(names, tag)
		}
		h.page(w, r, "/v2/"+repository+"/tags/list", "tags", names)
	case strings.Contains(rest, "/manifests/"):
		at := strings.LastIndex(rest, "/manifests/")
		repository, tag := rest[:at], rest[at+len("/manifests/"):]
		manifest, ok := h.images[repository][tag]
		if !ok {
			writeRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(manifest)
		}
	default:
		http.NotFound(w, r)
	}
}

// page answers a page of names, as n and last ask for, with a Link to the
// next.
func (h *RegistryHost) page(w http.ResponseWriter, r *http.Request, path, field string, names []string) {
	sort.Strings(names)
	query := r.URL.Query()
	if last := query.Get("last"); last != "" {
		at := sort.SearchStrings(names, last)
		if at < len(names) && names[at] == last {
			at++
		}
		names = names[at:]
	}
	if n, err := strconv.Atoi(query.Get("n")); err == nil && n > 0 && n < len(names) {
		names = names[:n]
		link := path
		if !h.LinkWithoutPrefix {
			link = h.Prefix + path
		}
		next := url.Values{"n": {strconv.Itoa(n)}, "last": {names[n-1]}}
		w.Header().Set("Link", fmt.Sprintf(`<%s?%s>; rel="next"`, link, next.Encode()))
	}
	answer := map[string]any{field: names}
	if field == "tags" {
		answer["name"] = strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (h *RegistryHost) serveToken(w http.ResponseWriter, r *http.Request) {
	if h.TokenRefusesCatalog.Load() && strings.HasPrefix(r.URL.Query().Get("scope"), "registry:catalog") {
		writeRegistryError(w, http.StatusBadRequest, "UNSUPPORTED", "unknown resource type")
		return
	}
	given := r.Header.Get("Authorization")
	ok := false
	switch {
	case h.Token != "" && h.Username != "":
		ok = given == "Basic "+base64.StdEncoding.EncodeToString([]byte(h.Username+":"+h.Token))
	case h.Token != "":
		ok = given == "Bearer "+h.Token
	default:
		ok = given == "Basic "+base64.StdEncoding.EncodeToString([]byte(h.Username+":"+h.Password))
	}
	if !ok {
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
		return
	}
	h.mu.Lock()
	h.scopes = append(h.scopes, r.URL.Query().Get("scope"))
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": issued})
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}
