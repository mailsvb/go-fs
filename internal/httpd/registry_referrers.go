package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// The signatures, attestations and SBOMs of an image travel with it when the
// registry moves it: a pull, a push and an import. They are found two ways.
// A manifest with the image as its subject is a referrer, which a registry
// lists at /v2/<name>/referrers/<digest>, and which one without that API
// lists in an index under the tag sha256-<hex>, as the distribution
// specification has clients write it. cosign writes its own under tags of
// the image's digest instead, sha256-<hex>.sig, .att and .sbom, whether or
// not the manifest under the tag names a subject. Both are followed one level
// down as well, for a signature on an SBOM, and no further.

// fallbackSuffixes are the tags cosign puts beside an image.
var fallbackSuffixes = []string{".sig", ".att", ".sbom"}

const (
	// referrerDepth is how many levels of referrers travel: those of the
	// image, and those of its referrers.
	referrerDepth = 2
	// maxAttached bounds the referrers that travel with one image, so that a
	// registry that lists without end cannot hold a transfer.
	maxAttached = 256
)

// referrersIndexTag is the tag the distribution specification lists the
// referrers of a manifest under, where the registry has no referrers API.
func referrersIndexTag(d digest.Digest) string {
	return d.Algorithm().String() + "-" + d.Encoded()
}

// fallbackTags are cosign's tags for the manifests attached to d.
func fallbackTags(d digest.Digest) []string {
	tags := make([]string, 0, len(fallbackSuffixes))
	for _, suffix := range fallbackSuffixes {
		tags = append(tags, referrersIndexTag(d)+suffix)
	}
	return tags
}

// parseFallbackTag reads a tag of cosign's, giving the digest it is beside.
func parseFallbackTag(tag string) (digest.Digest, bool) {
	for _, suffix := range fallbackSuffixes {
		rest, ok := strings.CutSuffix(tag, suffix)
		if !ok {
			continue
		}
		algorithm, encoded, ok := strings.Cut(rest, "-")
		if !ok {
			return "", false
		}
		d, ok := parseBlobDigest(algorithm + ":" + encoded)
		return d, ok
	}
	return "", false
}

// attachedManifest is a manifest that travels with an image.
type attachedManifest struct {
	manifest pulledManifest
	// children are the images of a referrer that is an index.
	children []pulledManifest
	// subject is the manifest it is attached to: the one its subject field
	// names, or the one its fallback tag is beside.
	subject digest.Digest
	// tags are cosign's tags it is under.
	tags []string
}

// referrer reports whether the manifest names its subject itself, which is
// what the referrers API lists.
func (a attachedManifest) referrer() bool {
	return a.manifest.doc.Subject != nil && a.manifest.doc.Subject.Digest == a.subject
}

// blobs are the blobs of an attached manifest and of its children.
func (a attachedManifest) blobs() []v1.Descriptor {
	blobs := imageBlobs(a.manifest.doc)
	for _, child := range a.children {
		blobs = addBlobs(blobs, imageBlobs(child.doc)...)
	}
	return blobs
}

// attachedBlobs are the blobs of all of a list, each once.
func attachedBlobs(list []attachedManifest) []v1.Descriptor {
	var blobs []v1.Descriptor
	for _, attached := range list {
		blobs = addBlobs(blobs, attached.blobs()...)
	}
	return blobs
}

// keepAttached is what of a list stays with an image cut to some of its
// manifests: what is attached to one of keep, or to what stays.
func keepAttached(list []attachedManifest, keep map[digest.Digest]bool) []attachedManifest {
	kept := map[digest.Digest]bool{}
	for d := range keep {
		kept[d] = true
	}
	var out []attachedManifest
	// the list is in the order it was found in, the referrers of a referrer
	// after it
	for _, attached := range list {
		if kept[attached.subject] {
			out = append(out, attached)
			kept[attached.manifest.digest] = true
		}
	}
	return out
}

// describeAttached says what travelled with an image, for the job's message:
// "" for nothing.
func describeAttached(list []attachedManifest) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return "with its signature or attestation"
	}
	return fmt.Sprintf("with its %d signatures and attestations", len(list))
}

// withAttached adds what describeAttached says to a message.
func withAttached(message string, list []attachedManifest) string {
	if text := describeAttached(list); text != "" {
		return message + ", " + text
	}
	return message
}

// manifestSource is where attached manifests are looked for: another
// registry, or a repository of this one.
type manifestSource interface {
	// referrers lists what names subject as its subject.
	referrers(ctx context.Context, subject digest.Digest) ([]digest.Digest, error)
	// tagged is the manifest under a tag, if there is one.
	tagged(ctx context.Context, tag string) (pulledManifest, bool, error)
	// manifest reads a manifest by its digest.
	manifest(ctx context.Context, d digest.Digest) (pulledManifest, error)
}

// findAttached walks what is attached to subjects, two levels deep. What
// cannot be read is left out, with a warning: an image is not to fail to
// move for a signature beside it.
func findAttached(ctx context.Context, src manifestSource, subjects []digest.Digest, log *slog.Logger) ([]attachedManifest, error) {
	var found []attachedManifest
	at := map[digest.Digest]int{}
	seen := map[digest.Digest]bool{}
	for _, d := range subjects {
		seen[d] = true
	}
	add := func(attached attachedManifest) bool {
		if i, ok := at[attached.manifest.digest]; ok {
			for _, tag := range attached.tags {
				if !slices.Contains(found[i].tags, tag) {
					found[i].tags = append(found[i].tags, tag)
				}
			}
			return false
		}
		if seen[attached.manifest.digest] || len(found) == maxAttached {
			return false
		}
		at[attached.manifest.digest] = len(found)
		found = append(found, attached)
		return true
	}
	level := subjects
	for depth := 0; depth < referrerDepth && len(level) > 0; depth++ {
		var next []digest.Digest
		for _, subject := range level {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			listed, err := src.referrers(ctx, subject)
			if err != nil {
				log.Warn("registry cannot list the referrers", "subject", subject.String(), "error", err)
			}
			for _, d := range listed {
				if _, had := at[d]; had || seen[d] {
					continue
				}
				manifest, err := src.manifest(ctx, d)
				if err == nil && (manifest.doc.Subject == nil || manifest.doc.Subject.Digest != subject) {
					err = fmt.Errorf("the referrer %s does not name %s as its subject", d, subject)
				}
				if err != nil {
					log.Warn("registry leaves out a referrer", "subject", subject.String(), "error", err)
					continue
				}
				attached, err := withChildren(ctx, src, attachedManifest{manifest: manifest, subject: subject})
				if err != nil {
					log.Warn("registry leaves out a referrer", "subject", subject.String(), "error", err)
					continue
				}
				if add(attached) {
					next = append(next, d)
				}
			}
			for _, tag := range fallbackTags(subject) {
				manifest, ok, err := src.tagged(ctx, tag)
				if err != nil {
					log.Warn("registry leaves out a tag beside an image", "tag", tag, "error", err)
					continue
				}
				if !ok {
					continue
				}
				attached, err := withChildren(ctx, src,
					attachedManifest{manifest: manifest, subject: subject, tags: []string{tag}})
				if err != nil {
					log.Warn("registry leaves out a tag beside an image", "tag", tag, "error", err)
					continue
				}
				if add(attached) {
					next = append(next, manifest.digest)
				}
			}
		}
		level = next
	}
	return found, nil
}

// withChildren reads the images of an attached manifest that is an index.
func withChildren(ctx context.Context, src manifestSource, attached attachedManifest) (attachedManifest, error) {
	if !isIndexType(attached.manifest.mediaType) {
		return attached, nil
	}
	for _, entry := range attached.manifest.doc.Manifests {
		child, err := src.manifest(ctx, entry.Digest)
		if err != nil {
			return attachedManifest{}, err
		}
		if !isImageType(child.mediaType) {
			return attachedManifest{}, fmt.Errorf("the index %s names %s, which is not an image",
				attached.manifest.digest, entry.Digest)
		}
		attached.children = append(attached.children, child)
	}
	return attached, nil
}

// --- another registry ---------------------------------------------------------

// remoteSource looks for attached manifests on another registry.
type remoteSource struct {
	client *remoteClient
}

func (r remoteSource) manifest(ctx context.Context, d digest.Digest) (pulledManifest, error) {
	found, err := r.client.getManifest(ctx, d.String())
	if err != nil {
		return pulledManifest{}, err
	}
	return decodeManifest(found)
}

func (r remoteSource) tagged(ctx context.Context, tag string) (pulledManifest, bool, error) {
	// a HEAD first, which a registry does not count against a pull limit as
	// it counts a GET
	there, err := r.client.manifestExists(ctx, tag)
	if err != nil || !there {
		return pulledManifest{}, false, err
	}
	found, err := r.client.getManifest(ctx, tag)
	if err != nil {
		return pulledManifest{}, false, err
	}
	manifest, err := decodeManifest(found)
	return manifest, err == nil, err
}

func (r remoteSource) referrers(ctx context.Context, subject digest.Digest) ([]digest.Digest, error) {
	index, supported, err := r.client.referrers(ctx, subject)
	if err != nil || supported {
		return index, err
	}
	// no referrers API: the index of them a client keeps under a tag
	manifest, ok, err := r.tagged(ctx, referrersIndexTag(subject))
	if err != nil || !ok {
		return nil, err
	}
	if !isIndexType(manifest.mediaType) {
		return nil, nil
	}
	var found []digest.Digest
	for _, entry := range manifest.doc.Manifests {
		found = append(found, entry.Digest)
	}
	return found, nil
}

// referrers asks the registry what names subject as its subject. supported
// is false for a registry without the API, which answers 404, or anything
// else than an index.
func (c *remoteClient) referrers(ctx context.Context, subject digest.Digest) (found []digest.Digest, supported bool, err error) {
	res, err := c.send(ctx, http.MethodGet, c.repoURL("referrers/"+subject.String()),
		http.Header{"Accept": {v1.MediaTypeImageIndex}}, nil, true)
	if err != nil {
		return nil, false, err
	}
	defer drain(res)
	if res.StatusCode != http.StatusOK {
		// an error of its own the registry would have answered for the
		// manifests too, so it is not one here
		return nil, false, nil
	}
	if mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil ||
		(mediaType != v1.MediaTypeImageIndex && mediaType != "application/json") {
		return nil, false, nil
	}
	var index manifestDoc
	if err := json.NewDecoder(io.LimitReader(res.Body, maxManifestSize)).Decode(&index); err != nil {
		return nil, false, nil
	}
	for _, entry := range index.Manifests {
		if _, ok := parseBlobDigest(entry.Digest.String()); ok {
			found = append(found, entry.Digest)
		}
	}
	return found, true, nil
}

// --- this registry --------------------------------------------------------------

// localSource looks for attached manifests in a repository of the store.
type localSource struct {
	store *registryStore
	name  string
	tags  map[string]digest.Digest
}

func newLocalSource(store *registryStore, name string) (localSource, error) {
	tags, err := store.readTags(name)
	if err != nil {
		return localSource{}, err
	}
	return localSource{store: store, name: name, tags: tags}, nil
}

func (l localSource) manifest(_ context.Context, d digest.Digest) (pulledManifest, error) {
	found, doc, err := readLocal(l.store, l.name, d)
	if err != nil {
		return pulledManifest{}, err
	}
	return pulledManifest{body: found.body, mediaType: found.mediaType, digest: d, doc: doc}, nil
}

func (l localSource) tagged(ctx context.Context, tag string) (pulledManifest, bool, error) {
	d, ok := l.tags[tag]
	if !ok {
		return pulledManifest{}, false, nil
	}
	manifest, err := l.manifest(ctx, d)
	if errors.Is(err, errRegistryNotFound) {
		return pulledManifest{}, false, nil
	}
	return manifest, err == nil, err
}

func (l localSource) referrers(_ context.Context, subject digest.Digest) ([]digest.Digest, error) {
	return l.store.referrers(l.name, subject)
}

// --- storing ----------------------------------------------------------------------

// storeAttachedLocked stores what travels with an image in a repository:
// the manifests, each recorded as a referrer of its subject where it names
// one, and points cosign's tags at them in tags, which the caller writes.
// The caller holds the repository's lock, and has linked the blobs.
func storeAttachedLocked(store *registryStore, name string, list []attachedManifest, tags map[string]digest.Digest) error {
	for _, attached := range list {
		for _, manifest := range append(slices.Clone(attached.children), attached.manifest) {
			if _, err := store.putBlob(manifest.body, manifest.digest.Algorithm()); err != nil {
				return err
			}
			if err := store.writeRevision(name, manifest.digest,
				revision{MediaType: manifest.mediaType, Size: int64(len(manifest.body))}); err != nil {
				return err
			}
			if manifest.doc.Subject != nil {
				if err := store.addReferrer(name, manifest.doc.Subject.Digest, manifest.digest); err != nil {
					return err
				}
			}
		}
		for _, tag := range attached.tags {
			tags[tag] = attached.manifest.digest
		}
	}
	return nil
}

// pushAttached sends what travels with an image to another registry, once
// the image is there: the manifests by digest, and cosign's tags where the
// registry has no referrers API, which the answer to a referrer's PUT says
// by leaving out OCI-Subject. What names a subject but has no tag of cosign's
// is then listed in the index under sha256-<hex>, as a client of such a
// registry keeps it. A manifest that is only under a tag goes under it.
func pushAttached(ctx context.Context, client *remoteClient, list []attachedManifest) error {
	pending := map[digest.Digest][]v1.Descriptor{}
	var order []digest.Digest
	for _, attached := range list {
		for _, child := range attached.children {
			if _, err := client.putManifest(ctx, child.digest.String(), child.mediaType, child.body); err != nil {
				return err
			}
		}
		manifest := attached.manifest
		send := func(ref string) (http.Header, error) {
			return client.putManifest(ctx, ref, manifest.mediaType, manifest.body)
		}
		if !attached.referrer() {
			for _, tag := range attached.tags {
				if _, err := send(tag); err != nil {
					return err
				}
			}
			continue
		}
		header, err := send(manifest.digest.String())
		if err != nil {
			return err
		}
		if header.Get("OCI-Subject") != "" {
			continue
		}
		for _, tag := range attached.tags {
			if _, err := send(tag); err != nil {
				return err
			}
		}
		if len(attached.tags) == 0 {
			if _, ok := pending[attached.subject]; !ok {
				order = append(order, attached.subject)
			}
			pending[attached.subject] = append(pending[attached.subject], referrerDescriptor(manifest))
		}
	}
	for _, subject := range order {
		if err := updateReferrersIndex(ctx, client, subject, pending[subject]); err != nil {
			return err
		}
	}
	return nil
}

// referrerDescriptor is how an index of referrers names one.
func referrerDescriptor(manifest pulledManifest) v1.Descriptor {
	artifactType := manifest.doc.ArtifactType
	if artifactType == "" && manifest.doc.Config != nil {
		artifactType = manifest.doc.Config.MediaType
	}
	return v1.Descriptor{MediaType: manifest.mediaType, Digest: manifest.digest, Size: int64(len(manifest.body)),
		ArtifactType: artifactType, Annotations: manifest.doc.Annotations}
}

// updateReferrersIndex adds referrers to the index under sha256-<hex> of
// another registry, keeping what it lists already.
func updateReferrersIndex(ctx context.Context, client *remoteClient, subject digest.Digest, add []v1.Descriptor) error {
	tag := referrersIndexTag(subject)
	index := manifestDoc{SchemaVersion: 2, MediaType: v1.MediaTypeImageIndex}
	there, err := client.manifestExists(ctx, tag)
	if err != nil {
		return err
	}
	if there {
		found, err := client.getManifest(ctx, tag)
		if err != nil {
			return err
		}
		var current manifestDoc
		if json.Unmarshal(found.body, &current) == nil && current.Manifests != nil {
			index.Manifests = current.Manifests
		}
	}
	changed := false
	for _, entry := range add {
		if !slices.ContainsFunc(index.Manifests, func(had v1.Descriptor) bool { return had.Digest == entry.Digest }) {
			index.Manifests = append(index.Manifests, entry)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	body, err := json.Marshal(index)
	if err != nil {
		return err
	}
	_, err = client.putManifest(ctx, tag, v1.MediaTypeImageIndex, body)
	return err
}
