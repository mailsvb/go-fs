package httpd

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/imagesig"
)

// imageSign is a [[http.services.imagesign]] entry, read: who may sign an
// image in its labels, what the registry refuses for it, and the
// repositories it is for.
type imageSign struct {
	trust *imagesig.LabelTrust
	mode  string
	// repositories match the repositories the entry is for; none is every
	// repository
	repositories []*regexp.Regexp
}

// loadImageSigns reads the entries. The configuration has checked them
// already, so an error here is one the checks let through.
func loadImageSigns(entries []config.ImageSign) ([]imageSign, error) {
	signs := make([]imageSign, 0, len(entries))
	for i, entry := range entries {
		trust, err := config.LoadImageSign(entry)
		if err != nil {
			return nil, fmt.Errorf("http.services.imagesign[%d] %q: %w", i, entry.Name, err)
		}
		loaded := imageSign{trust: trust, mode: entry.ImageSignMode()}
		for _, pattern := range entry.Repositories {
			loaded.repositories = append(loaded.repositories, globPattern(pattern))
		}
		signs = append(signs, loaded)
	}
	return signs, nil
}

// modeRank orders the modes by how much they refuse.
func modeRank(mode string) int {
	switch mode {
	case config.ImageSignEnforced:
		return 2
	case config.ImageSignMandatory:
		return 1
	}
	return 0
}

// imageSignsFor are the trusts of the entries for a repository, and the
// strictest of their modes. None is a repository nothing is checked for.
func (set *settings) imageSignsFor(name string) ([]*imagesig.LabelTrust, string) {
	var trusts []*imagesig.LabelTrust
	mode := config.ImageSignOptional
	for _, entry := range set.imageSigns {
		applies := len(entry.repositories) == 0
		for _, pattern := range entry.repositories {
			if pattern.MatchString(name) {
				applies = true
				break
			}
		}
		if !applies {
			continue
		}
		trusts = append(trusts, entry.trust)
		if modeRank(entry.mode) > modeRank(mode) {
			mode = entry.mode
		}
	}
	return trusts, mode
}

// imageSignCheck is what checking the label signature of one image manifest
// came to.
type imageSignCheck struct {
	result imagesig.LabelResult
	// mode is the strictest mode of the entries for the repository
	mode string
	// signers are the names of those entries
	signers []string
	// platform is the image's, as its configuration names it
	platform string
	// image is the manifest checked, and config the digest of its
	// configuration, which carries the labels
	image  digest.Digest
	config digest.Digest
}

// refused reports whether the mode refuses the image.
func (c imageSignCheck) refused() bool {
	switch c.mode {
	case config.ImageSignEnforced:
		return c.result.Outcome != imagesig.LabelValid
	case config.ImageSignMandatory:
		return c.result.Outcome == imagesig.LabelInvalid
	}
	return false
}

// reason is what a refusal says, to the client and in the log.
func (c imageSignCheck) reason() string {
	what := "the image is not signed"
	if c.result.Outcome == imagesig.LabelInvalid {
		what = "the signature of the image does not verify"
	}
	if c.platform != "" {
		what += " for " + c.platform
	}
	return fmt.Sprintf("imagesign %s (%s): %s: %s", strings.Join(c.signers, ", "), c.mode, what, c.result.Err)
}

// imageSignCache keeps what checking an image manifest came to, by the
// repository and the manifest, until a reload replaces it with the entries.
// A manifest that is not an image's is kept as well, so that a pull of an
// index does not read it again and again.
type imageSignCache struct {
	mu      sync.Mutex
	entries map[string]imageSignCached
}

type imageSignCached struct {
	check   imageSignCheck
	applies bool
}

func (c *imageSignCache) get(key string) (imageSignCached, bool) {
	if c == nil {
		return imageSignCached{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	found, ok := c.entries[key]
	return found, ok
}

func (c *imageSignCache) put(key string, found imageSignCached) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxVerifications {
		c.entries = make(map[string]imageSignCached)
	}
	c.entries[key] = found
}

// checkImageSign checks the label signature of a manifest of a repository.
// It reports false when there is nothing to check: no entry is for the
// repository, or the manifest is no image's but an index, a signature, an
// attestation or another artifact. doc is the manifest, nil to read it from
// the store. Its configuration has to be in the store.
func (set *settings) checkImageSign(name string, d digest.Digest, mediaType string, doc *manifestDoc) (imageSignCheck, bool) {
	trusts, mode := set.imageSignsFor(name)
	if len(trusts) == 0 || set.registry == nil {
		return imageSignCheck{}, false
	}
	key := name + " " + d.String()
	if found, ok := set.imageSignChecks.get(key); ok {
		return found.check, found.applies
	}
	check, applies := set.readImageSign(d, mediaType, doc, trusts)
	if applies {
		check.mode = mode
		for _, trust := range trusts {
			check.signers = append(check.signers, trust.Name)
		}
	}
	set.imageSignChecks.put(key, imageSignCached{check: check, applies: applies})
	return check, applies
}

func (set *settings) readImageSign(d digest.Digest, mediaType string, doc *manifestDoc, trusts []*imagesig.LabelTrust) (imageSignCheck, bool) {
	store := set.registry
	if doc == nil {
		read, err := readManifest(store, d)
		if err != nil {
			return imageSignCheck{}, false
		}
		doc = &read
	}
	if !hasImageConfig(mediaType, *doc) || artifactKindOf(*doc) != "" {
		return imageSignCheck{}, false
	}
	data, err := store.readBlob(doc.Config.Digest, maxConfigSize)
	if err != nil {
		// checkReferences has made sure the configuration is there; one
		// that cannot be read is an image that cannot be checked
		return imageSignCheck{image: d, config: doc.Config.Digest, result: imagesig.LabelResult{Outcome: imagesig.LabelInvalid,
			Err: "the configuration of the image cannot be read: " + err.Error()}}, true
	}
	found, ok := decodeImageConfig(data)
	if !ok || (found.platform.OS == "unknown" && found.platform.Architecture == "unknown") {
		// the attestations buildx attaches to an image are manifests of
		// an image for no platform
		return imageSignCheck{}, false
	}
	return imageSignCheck{result: imagesig.VerifyLabels(data, trusts), platform: platformKey(&found.platform),
		image: d, config: doc.Config.Digest}, true
}

// refuseImageSign answers a push or a pull of a manifest its imagesign mode
// refuses, and says whether it did.
func (q *registryRequest) refuseImageSign(check imageSignCheck, applies bool, d digest.Digest, action string) bool {
	if !applies || !check.refused() {
		return false
	}
	q.s.log.Warn("registry "+action+" refused for the image's signature", "repository", q.route.name,
		"reference", q.route.ref, "digest", d.String(), "platform", check.platform, "mode", check.mode,
		"signature", check.result.Outcome.String(), "reason", check.result.Err, "user", nameOf(q.user),
		"address", clientAddress(q.set, q.r))
	q.fail(regErr(http.StatusForbidden, "DENIED", check.reason()))
	return true
}

// logImageSign logs what the label signature of a manifest just pushed came
// to: who signed it, or a warning that nobody did.
func (q *registryRequest) logImageSign(check imageSignCheck, applies bool, d digest.Digest) {
	if !applies {
		return
	}
	attrs := []any{"repository", q.route.name, "digest", d.String(), "platform", check.platform, "mode", check.mode}
	switch check.result.Outcome {
	case imagesig.LabelValid:
		q.s.log.Info("registry image signature verified", append(attrs, "signer", check.result.TrustedBy,
			"prefix", check.result.Prefix, "identity", check.result.Signer)...)
	case imagesig.LabelMissing:
		q.s.log.Warn("registry image is not signed", append(attrs, "signers", strings.Join(check.signers, ","))...)
	default:
		q.s.log.Warn("registry image signature does not verify", append(attrs, "reason", check.result.Err)...)
	}
}

// imageSignCandidate is a manifest a job is about to store or send.
type imageSignCandidate struct {
	digest    digest.Digest
	mediaType string
	doc       manifestDoc
}

// refuseImageSigns checks the manifests a pull, an import or a push of the
// page is about to store or send, and refuses them all when the mode of the
// repository refuses one, naming each platform it refuses. The
// configurations have to be in the store.
func (set *settings) refuseImageSigns(name string, manifests []imageSignCandidate) error {
	var refused []imageSignCheck
	passed := 0
	for _, manifest := range manifests {
		doc := manifest.doc
		check, applies := set.checkImageSign(name, manifest.digest, manifest.mediaType, &doc)
		switch {
		case !applies:
		case check.refused():
			refused = append(refused, check)
		default:
			passed++
		}
	}
	if len(refused) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(refused))
	for _, check := range refused {
		if reason := check.reason(); !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	message := strings.Join(reasons, "; ")
	if passed > 0 {
		message += "; choose the platforms that are signed"
	}
	return errors.New(message)
}

// imageSignsOf checks the images of a tag: the one it points to, or each
// platform of its index.
func imageSignsOf(set *settings, name string, summary imageSummary) []imageSignCheck {
	var checks []imageSignCheck
	for _, platform := range summary.platforms {
		if platform.key == "" {
			continue
		}
		if check, applies := set.checkImageSign(name, platform.digest, platform.mediaType, nil); applies {
			checks = append(checks, check)
		}
	}
	return checks
}

// describe says what checking one image came to, for the title of a badge.
func (c imageSignCheck) describe() string {
	switch c.result.Outcome {
	case imagesig.LabelValid:
		return "signed by " + c.result.TrustedBy + " (" + c.result.Signer + ")"
	case imagesig.LabelMissing:
		return "not signed"
	}
	return "the signature does not verify: " + c.result.Err
}

// imageSignBadge sums up the label signatures of a tag in one badge: signed
// when every platform has one that verifies, and a warning otherwise, that
// names what is wrong. Its title says what each platform came to, and
// whether the mode refuses one.
func imageSignBadge(checks []imageSignCheck) *registryBadge {
	if len(checks) == 0 {
		return nil
	}
	valid, invalid, refused := 0, 0, false
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		switch check.result.Outcome {
		case imagesig.LabelValid:
			valid++
		case imagesig.LabelInvalid:
			invalid++
		}
		refused = refused || check.refused()
		what := check.describe()
		if len(checks) > 1 {
			what = check.platform + ": " + what
		}
		parts = append(parts, what)
	}
	title := "imagesign " + strings.Join(checks[0].signers, ", ") + " (" + checks[0].mode + ") · " + strings.Join(parts, "; ")
	if refused {
		title += " · the registry refuses to hand it out"
	}
	switch {
	case valid == len(checks):
		return &registryBadge{Label: "signed", Title: title, Class: "imagesign signed"}
	case invalid > 0:
		return &registryBadge{Label: "\u26a0 bad signature", Title: title, Class: "imagesign warning"}
	case valid > 0:
		return &registryBadge{Label: "\u26a0 partly signed", Title: title, Class: "imagesign warning"}
	}
	return &registryBadge{Label: "\u26a0 unsigned", Title: title, Class: "imagesign warning"}
}

// imageSignJSON lists the label signatures of a tag that points to d for its
// details, among its artifacts.
func imageSignJSON(set *settings, name string, d digest.Digest, summary imageSummary) []registryArtifactJSON {
	var found []registryArtifactJSON
	for _, check := range imageSignsOf(set, name, summary) {
		item := registryArtifactJSON{
			Kind:      "imagesign signature",
			Digest:    check.config.String(),
			MediaType: "image configuration labels",
			Subject:   check.image.String(),
			Signed:    check.result.Outcome != imagesig.LabelMissing,
			InConfig:  true,
			Mode:      check.mode,
			Refused:   check.refused(),
		}
		if check.image != d {
			item.Platform = check.platform
		}
		switch check.result.Outcome {
		case imagesig.LabelValid:
			item.Verified, item.TrustedBy, item.Signer = "yes", check.result.TrustedBy, check.result.Signer
		case imagesig.LabelInvalid:
			item.Verified, item.Problem = "no", check.result.Err
		default:
			item.Problem = check.result.Err
		}
		found = append(found, item)
	}
	return found
}
