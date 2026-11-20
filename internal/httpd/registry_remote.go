package httpd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/ociauth"
)

// The registry page can copy an image from another registry into this one,
// and a tag of this one to another registry. What talks to the other registry
// is the client here: the part of the distribution API a pull and a push
// need, and the token flow docker's registries log in with.

// A reference is written the way docker takes it: a first component with a
// dot or a colon in it, or localhost, is the registry, and anything else is on
// Docker Hub, where a name of one component is an official image under
// library/.
const (
	dockerHub    = "docker.io"
	dockerHubAPI = "registry-1.docker.io"
)

// remoteRef is an image of another registry.
type remoteRef struct {
	// scheme is https unless the reference started with http://.
	scheme string
	// host is the registry as it was written, api where its API is.
	host, api  string
	repository string
	// tag and digest are what the reference names; one of them at least.
	tag    string
	digest digest.Digest
}

// String is the reference written in full.
func (r remoteRef) String() string {
	name := r.host + "/" + r.repository
	if r.tag != "" {
		name += ":" + r.tag
	}
	if r.digest != "" {
		name += "@" + r.digest.String()
	}
	return name
}

// ref is what the manifest is fetched by: the digest where there is one, as
// it cannot change under the pull.
func (r remoteRef) ref() string {
	if r.digest != "" {
		return r.digest.String()
	}
	return r.tag
}

// cutScheme takes an http:// or https:// off the front of what was typed.
func cutScheme(value string) (scheme, rest string) {
	if rest, ok := strings.CutPrefix(value, "http://"); ok {
		return "http", rest
	}
	if rest, ok := strings.CutPrefix(value, "https://"); ok {
		return "https", rest
	}
	return "https", value
}

// isRegistryHost reports whether the first component of a name is a
// registry rather than a namespace of Docker Hub.
func isRegistryHost(component string) bool {
	return strings.ContainsAny(component, ".:") || component == "localhost"
}

// splitHost cuts the registry off a name, Docker Hub where the name does not
// start with one, and gives the host the API is at.
func splitHost(name string) (host, api, rest string) {
	host, rest, found := strings.Cut(name, "/")
	if !found || !isRegistryHost(host) {
		host, rest = dockerHub, name
	}
	host = strings.ToLower(host)
	api = host
	if host == dockerHub || host == "index.docker.io" {
		host, api = dockerHub, dockerHubAPI
	}
	return host, api, rest
}

// hubName is a repository of Docker Hub with its official images under
// library/, as docker names them.
func hubName(host, repository string) string {
	if host == dockerHub && repository != "" && !strings.Contains(repository, "/") {
		return "library/" + repository
	}
	return repository
}

// parseRemoteReference reads an image reference as docker does. A reference
// without a tag or a digest is for latest.
func parseRemoteReference(value string) (remoteRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return remoteRef{}, errors.New("name the image")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return remoteRef{}, fmt.Errorf("%q is not an image reference", value)
	}
	ref := remoteRef{}
	scheme, rest := cutScheme(value)
	ref.scheme = scheme
	if before, after, found := strings.Cut(rest, "@"); found {
		d, ok := parseBlobDigest(after)
		if !ok {
			return remoteRef{}, fmt.Errorf("%q is not a digest", after)
		}
		ref.digest, rest = d, before
	}
	// the tag is after the last colon that comes after the last slash: a
	// colon before it is the port of the registry
	if at := strings.LastIndex(rest, ":"); at > strings.LastIndex(rest, "/") {
		ref.tag, rest = rest[at+1:], rest[:at]
		if !tagPattern.MatchString(ref.tag) {
			return remoteRef{}, fmt.Errorf("%q is not a tag", ref.tag)
		}
	}
	ref.host, ref.api, ref.repository = splitHost(rest)
	ref.repository = hubName(ref.host, ref.repository)
	if !validRepository(ref.repository) {
		return remoteRef{}, fmt.Errorf("%q is not a repository name: lowercase letters, digits and separators only",
			ref.repository)
	}
	if ref.tag == "" && ref.digest == "" {
		ref.tag = "latest"
	}
	return ref, nil
}

// remoteIdleTimeout is how long a blob may go without a byte arriving or
// leaving before the transfer is given up.
var remoteIdleTimeout = 2 * time.Minute

// manifestAccept is every manifest type the registry stores.
var manifestAccept = strings.Join([]string{v1.MediaTypeImageIndex, v1.MediaTypeImageManifest,
	mediaTypeDockerList, mediaTypeDockerManifest}, ", ")

// remoteClient talks to one repository of another registry.
type remoteClient struct {
	http       *http.Client
	base       string
	host       string
	repository string
	// actions are what the token is asked for: pull, or pull,push.
	actions string
	// auth is the login, and the header every request carries once the
	// registry has said what it wants.
	auth *ociauth.Authorizer
}

// remoteLogin is how a transfer reaches the other registry: the credentials
// it was given, and what its connection trusts and shows.
type remoteLogin struct {
	credentials ociauth.Credentials
	tls         ociauth.TLS
}

// newRemoteClient is a client of the repository of ref. log takes the trace
// of every request the client sends.
func newRemoteClient(ref remoteRef, login remoteLogin, actions string, log *slog.Logger) (*remoteClient, error) {
	transport, err := login.tls.Transport()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref.host, err)
	}
	client := &http.Client{Transport: logging.Transport(log, "registry", transport),
		CheckRedirect: ociauth.KeepCredentialsHome}
	return &remoteClient{
		http:       client,
		base:       ref.scheme + "://" + ref.api,
		host:       ref.host,
		repository: ref.repository,
		actions:    actions,
		auth:       ociauth.NewAuthorizer(client, ref.host, login.credentials),
	}, nil
}

// do sends a request to the other registry.
func (c *remoteClient) do(req *http.Request) (*http.Response, error) {
	return c.auth.Do(req)
}

// scope is what a token is asked for.
func (c *remoteClient) scope() string {
	return "repository:" + c.repository + ":" + c.actions
}

// authenticate asks /v2/ what the registry wants, and gets it: a token for
// the repository, or the Basic credentials on every request. It is done before
// anything is sent, since a body that is streamed cannot be sent again.
func (c *remoteClient) authenticate(ctx context.Context) error {
	return c.auth.Authenticate(ctx, c.base, c.scope())
}

// send makes one request with the authorization there is. A request that
// may be repeated, one without a body or with one held in memory, meets a
// challenge it is answered with once and goes again: a token runs out, and
// some registries only challenge on the repository.
func (c *remoteClient) send(ctx context.Context, method, target string, header http.Header, body []byte, retry bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		for key, values := range header {
			req.Header[key] = values
		}
		if c.auth.Header != "" {
			req.Header.Set("Authorization", c.auth.Header)
		}
		res, err := c.do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.host, err)
		}
		challenge := res.Header.Get("WWW-Authenticate")
		if res.StatusCode != http.StatusUnauthorized || !retry || attempt > 0 || challenge == "" {
			return res, nil
		}
		drain(res)
		if err := c.auth.Answer(ctx, challenge, c.scope()); err != nil {
			return nil, err
		}
	}
}

// repoURL is a path of the repository's API.
func (c *remoteClient) repoURL(rest string) string {
	return c.base + "/v2/" + c.repository + "/" + rest
}

// remoteManifest is a manifest as the other registry has it.
type remoteManifest struct {
	body      []byte
	mediaType string
	digest    digest.Digest
}

// getManifest fetches a manifest by tag or digest. One fetched by digest is
// checked against it, and so is one whose answer names its digest.
func (c *remoteClient) getManifest(ctx context.Context, ref string) (remoteManifest, error) {
	res, err := c.send(ctx, http.MethodGet, c.repoURL("manifests/"+ref),
		http.Header{"Accept": {manifestAccept}}, nil, true)
	if err != nil {
		return remoteManifest{}, err
	}
	defer drain(res)
	if res.StatusCode != http.StatusOK {
		return remoteManifest{}, c.failure(res, c.repository+":"+ref)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxManifestSize+1))
	if err != nil {
		return remoteManifest{}, fmt.Errorf("%s: the manifest could not be read: %w", c.host, err)
	}
	if len(body) > maxManifestSize {
		return remoteManifest{}, fmt.Errorf("%s: the manifest of %s is larger than 4 MiB", c.host, ref)
	}
	found := remoteManifest{body: body, digest: digest.SHA256.FromBytes(body)}
	if mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil {
		found.mediaType = mediaType
	}
	want, byDigest := parseBlobDigest(ref)
	if !byDigest {
		want, byDigest = parseBlobDigest(res.Header.Get("Docker-Content-Digest"))
	}
	if byDigest {
		if actual := want.Algorithm().FromBytes(body); actual != want {
			return remoteManifest{}, fmt.Errorf("%s: the manifest of %s is not what its digest %s says",
				c.host, ref, want)
		}
		found.digest = want
	}
	return found, nil
}

// getBlob opens a blob. The registry may send the client elsewhere for it,
// to a bucket or a CDN, which is followed without the Authorization header.
func (c *remoteClient) getBlob(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
	res, err := c.send(ctx, http.MethodGet, c.repoURL("blobs/"+d.String()), nil, nil, true)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer drain(res)
		return nil, c.failure(res, d.String())
	}
	return res.Body, nil
}

// blobExists reports whether the repository already has a blob, which a push
// then leaves out.
func (c *remoteClient) blobExists(ctx context.Context, d digest.Digest) (bool, error) {
	res, err := c.send(ctx, http.MethodHead, c.repoURL("blobs/"+d.String()), nil, nil, true)
	if err != nil {
		return false, err
	}
	drain(res)
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, c.failure(res, d.String())
	}
}

// manifestExists reports whether the repository has a manifest under a tag
// or a digest.
func (c *remoteClient) manifestExists(ctx context.Context, ref string) (bool, error) {
	res, err := c.send(ctx, http.MethodHead, c.repoURL("manifests/"+ref),
		http.Header{"Accept": {manifestAccept}}, nil, true)
	if err != nil {
		return false, err
	}
	drain(res)
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, c.failure(res, c.repository+":"+ref)
	}
}

// canPush proves that the login may push to the repository by opening an
// upload, which it then drops. Nothing less proves it: a token service hands
// out a token for less than was asked rather than refuse.
func (c *remoteClient) canPush(ctx context.Context) error {
	res, err := c.send(ctx, http.MethodPost, c.repoURL("blobs/uploads/"), nil, nil, true)
	if err != nil {
		return err
	}
	drain(res)
	if res.StatusCode != http.StatusAccepted {
		return c.failure(res, c.repository)
	}
	if location, err := uploadLocation(res); err == nil {
		// an upload left open runs out on its own, so a failure here is no loss
		if res, err := c.send(ctx, http.MethodDelete, location.String(), nil, nil, false); err == nil {
			drain(res)
		}
	}
	return nil
}

// pushBlob uploads a blob the way docker does: an upload is opened, the whole
// blob is streamed into it in one PATCH, and a PUT with the digest closes it.
func (c *remoteClient) pushBlob(ctx context.Context, d digest.Digest, size int64, body io.Reader) error {
	res, err := c.send(ctx, http.MethodPost, c.repoURL("blobs/uploads/"), nil, nil, true)
	if err != nil {
		return err
	}
	drain(res)
	if res.StatusCode != http.StatusAccepted {
		return c.failure(res, d.String())
	}
	location, err := uploadLocation(res)
	if err != nil {
		return err
	}

	if size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, location.String(), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.auth.Header != "" {
		req.Header.Set("Authorization", c.auth.Header)
	}
	res, err = c.do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", c.host, err)
	}
	drain(res)
	if res.StatusCode != http.StatusAccepted && res.StatusCode != http.StatusNoContent {
		return c.failure(res, d.String())
	}
	if next, err := uploadLocation(res); err == nil {
		location = next
	}

	query := location.Query()
	query.Set("digest", d.String())
	location.RawQuery = query.Encode()
	res, err = c.send(ctx, http.MethodPut, location.String(),
		http.Header{"Content-Type": {"application/octet-stream"}}, nil, false)
	if err != nil {
		return err
	}
	drain(res)
	if res.StatusCode != http.StatusCreated {
		return c.failure(res, d.String())
	}
	return nil
}

// uploadLocation is where an upload goes on: the Location of the answer,
// relative to the request it answers.
func uploadLocation(res *http.Response) (*url.URL, error) {
	value := res.Header.Get("Location")
	if value == "" {
		return nil, errors.New("the registry opened an upload without saying where it is")
	}
	location, err := res.Request.URL.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("the registry named an upload that is not a URL: %q", value)
	}
	return location, nil
}

// putManifest stores a manifest under a tag or its digest, and gives the
// header of the answer, whose OCI-Subject says whether the registry took a
// subject the manifest names.
func (c *remoteClient) putManifest(ctx context.Context, ref, mediaType string, body []byte) (http.Header, error) {
	res, err := c.send(ctx, http.MethodPut, c.repoURL("manifests/"+ref),
		http.Header{"Content-Type": {mediaType}}, body, true)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer drain(res)
		return nil, c.failure(res, c.repository+":"+ref)
	}
	drain(res)
	return res.Header, nil
}

// failure says what went wrong with a request about what, in the words of
// the registry where it has any.
func (c *remoteClient) failure(res *http.Response, what string) error {
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if c.auth.Credentials.Empty() {
			return fmt.Errorf("%s refused access to %s: it does not exist, or it needs a username and password (%w)",
				c.host, what, remoteFailure(res))
		}
		return fmt.Errorf("%s refused access to %s (%w)", c.host, what, remoteFailure(res))
	case http.StatusNotFound:
		return fmt.Errorf("%s has no %s (%w)", c.host, what, remoteFailure(res))
	}
	return fmt.Errorf("%s: %s: %w", c.host, what, remoteFailure(res))
}

// remoteFailure reads the errors of an answer as the specification writes
// them, or whatever else the body says.
func remoteFailure(res *http.Response) error { return ociauth.Failure(res) }

// drain reads what is left of a body and closes it, so the connection can be
// used again.
func drain(res *http.Response) { ociauth.Drain(res) }
