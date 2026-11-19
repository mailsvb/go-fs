package httpd

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/imagesig"
)

// registryTrust is a [[http.services.registryTrust]] entry, read: what it
// verifies signatures with, and the repositories it is trusted for.
type registryTrust struct {
	trust *imagesig.Trust
	// repositories match the repositories the trust is for; none is every
	// repository
	repositories []*regexp.Regexp
}

// loadRegistryTrusts reads the files of the trusts. The configuration has
// checked them already, so an error here is a file that changed since.
func loadRegistryTrusts(entries []config.RegistryTrust) ([]registryTrust, error) {
	trusts := make([]registryTrust, 0, len(entries))
	for i, entry := range entries {
		trust, err := imagesig.LoadTrust(entry.Name, entry.PublicKey, entry.Certificates, entry.TrustedRoot,
			entry.Identity, entry.Issuer)
		if err != nil {
			return nil, fmt.Errorf("http.services.registryTrust[%d] %q: %w", i, entry.Name, err)
		}
		loaded := registryTrust{trust: trust}
		for _, pattern := range entry.Repositories {
			loaded.repositories = append(loaded.repositories, globPattern(pattern))
		}
		trusts = append(trusts, loaded)
	}
	return trusts, nil
}

// globPattern compiles a pattern where * stands for anything, slashes
// included, and everything else for itself.
func globPattern(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// trustsFor are the trusts that may verify a signature of a repository.
func (set *settings) trustsFor(name string) []*imagesig.Trust {
	var found []*imagesig.Trust
	for _, entry := range set.registryTrusts {
		if len(entry.repositories) == 0 {
			found = append(found, entry.trust)
			continue
		}
		for _, pattern := range entry.repositories {
			if pattern.MatchString(name) {
				found = append(found, entry.trust)
				break
			}
		}
	}
	return found
}

// maxVerifications bounds the cache of verifications, as maxSummaries bounds
// that of summaries.
const maxVerifications = 4096

// verificationCache keeps what verifying an artifact came to. The answer
// depends on the artifact, the image it is for and the trusts of the
// repository, which a reload replaces along with the cache, so it is kept
// until then.
type verificationCache struct {
	mu      sync.Mutex
	entries map[string]imagesig.Result
}

func (c *verificationCache) get(key string) (imagesig.Result, bool) {
	if c == nil {
		return imagesig.Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	found, ok := c.entries[key]
	return found, ok
}

func (c *verificationCache) put(key string, result imagesig.Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxVerifications {
		c.entries = make(map[string]imagesig.Result)
	}
	c.entries[key] = result
}

// verifyArtifact checks a signature or attestation of a repository, the
// manifest d, for the image subject, against the trusts of the repository.
// It reports false when there is nothing to check it against.
func (set *settings) verifyArtifact(name string, subject, d digest.Digest) (imagesig.Result, bool) {
	trusts := set.trustsFor(name)
	if len(trusts) == 0 || set.registry == nil {
		return imagesig.Result{}, false
	}
	key := name + " " + subject.String() + " " + d.String()
	if found, ok := set.verifications.get(key); ok {
		return found, true
	}
	store := set.registry
	manifest, err := store.readBlob(d, maxManifestSize)
	if err != nil {
		return imagesig.Result{Err: "the manifest cannot be read: " + err.Error()}, true
	}
	fetch := func(blob digest.Digest) ([]byte, error) {
		// imagesig checks what it is given against the digest
		return store.readBlob(blob, maxManifestSize)
	}
	result := imagesig.Verify(subject, manifest, fetch, trusts)
	set.verifications.put(key, result)
	return result, true
}

// logVerification verifies a signature or an attestation that was just
// pushed and logs what that came to, so the log says who signed an image as
// it arrives. What it is for is its subject, or the digest its fallback tag
// is named after.
func (q *registryRequest) logVerification(d digest.Digest, doc manifestDoc, tag string) {
	if !artifactSigned(doc) {
		return
	}
	var subject digest.Digest
	switch {
	case doc.Subject != nil:
		subject = doc.Subject.Digest
	case tag != "":
		named, suffix, ok := fallbackTagSubject(tag)
		if !ok || suffix == "" {
			return
		}
		subject = named
	default:
		return
	}
	result, checked := q.set.verifyArtifact(q.route.name, subject, d)
	if !checked {
		return
	}
	attrs := []any{"repository", q.route.name, "digest", d.String(), "subject", subject.String(),
		"artifact", artifactKindOf(doc)}
	if result.OK {
		q.s.log.Info("registry signature verified", append(attrs, "trust", result.TrustedBy,
			"signer", result.Identity)...)
		return
	}
	q.s.log.Info("registry signature not verified", append(attrs, "reason", result.Err)...)
}
