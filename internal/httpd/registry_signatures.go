package httpd

import (
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
)

// The media and artifact types of the signatures and attestations clients
// store next to an image. The registry does not need any of them to store or
// serve these: they are ordinary manifests, attached to their image by their
// subject or, by tools written for registries without the referrers API, by a
// tag named after the image's digest. It reads them to tell them apart.
const (
	// cosign before version 3: a manifest tagged sha256-<hex>.sig whose
	// layers are each a payload with the signature in an annotation
	mediaTypeCosignSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	annotationCosignSignature    = "dev.cosignproject.cosign/signature"
	// an in-toto statement in a DSSE envelope: what cosign attest stores
	// under sha256-<hex>.att, and what a Sigstore bundle can carry
	mediaTypeDSSEEnvelope = "application/vnd.dsse.envelope.v1+json"
	// a Sigstore bundle as a referrer: cosign 3 and GitHub's
	// actions/attest-build-provenance store these
	mediaTypeSigstoreBundle   = "application/vnd.dev.sigstore.bundle.v0.3+json"
	annotationBundlePredicate = "dev.sigstore.bundle.predicateType"
	// a Notary Project signature, as notation stores it
	artifactTypeNotation  = "application/vnd.cncf.notary.signature"
	mediaTypeNotationJWS  = "application/jose+json"
	mediaTypeNotationCOSE = "application/cose"
)

// artifactKindOf names what a manifest is when it is a signature, an
// attestation or an SBOM, as the registry page and the log show it, and is ""
// for anything else: an image, an index, or an artifact of a kind not known
// here.
func artifactKindOf(doc manifestDoc) string {
	artifactType := doc.ArtifactType
	if artifactType == "" && doc.Config != nil {
		artifactType = doc.Config.MediaType
	}
	switch {
	case artifactType == artifactTypeNotation:
		return "Notation signature"
	case strings.HasPrefix(artifactType, "application/vnd.dev.sigstore.bundle"):
		return predicateKind(doc.Annotations[annotationBundlePredicate], "Sigstore signature")
	}
	if kind := sbomKind(artifactType); kind != "" {
		return kind
	}
	for _, layer := range doc.Layers {
		switch layer.MediaType {
		case mediaTypeCosignSimpleSigning:
			return "cosign signature"
		case mediaTypeDSSEEnvelope:
			return predicateKind(layer.Annotations["predicateType"], "attestation")
		}
		if kind := sbomKind(layer.MediaType); kind != "" {
			return kind
		}
	}
	return ""
}

// artifactSigned reports whether an artifact carries a signature: a Notation
// signature, a Sigstore bundle, or a layer cosign signed. An SBOM cosign
// attached as it is carries none.
func artifactSigned(doc manifestDoc) bool {
	if doc.ArtifactType == artifactTypeNotation || strings.HasPrefix(doc.ArtifactType, "application/vnd.dev.sigstore.bundle") {
		return true
	}
	for _, layer := range doc.Layers {
		switch {
		case layer.MediaType == mediaTypeDSSEEnvelope, layer.MediaType == mediaTypeNotationJWS,
			layer.MediaType == mediaTypeNotationCOSE, strings.HasPrefix(layer.MediaType, "application/vnd.dev.sigstore.bundle"),
			layer.Annotations[annotationCosignSignature] != "":
			return true
		}
	}
	return false
}

// predicateKind names an in-toto predicate type by what it attests; fallback
// is the name for a bundle that holds no statement, or one not known here.
func predicateKind(predicateType, fallback string) string {
	switch {
	case predicateType == "", predicateType == "https://sigstore.dev/cosign/sign/v1":
		return fallback
	case strings.HasPrefix(predicateType, "https://slsa.dev/provenance/"):
		return "SLSA provenance"
	case strings.HasPrefix(predicateType, "https://spdx.dev/Document"):
		return "SPDX SBOM"
	case strings.HasPrefix(predicateType, "https://cyclonedx.org/bom"):
		return "CycloneDX SBOM"
	case strings.HasPrefix(predicateType, "https://in-toto.io/attestation/vulns"),
		predicateType == "https://cosign.sigstore.dev/attestation/vuln/v1":
		return "vulnerability scan"
	}
	return "attestation"
}

// sbomKind names an SBOM by its media type, and is "" for anything else.
func sbomKind(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "text/spdx"), strings.HasPrefix(mediaType, "application/spdx"),
		strings.HasPrefix(mediaType, "application/vnd.spdx"):
		return "SPDX SBOM"
	case strings.HasPrefix(mediaType, "application/vnd.cyclonedx"):
		return "CycloneDX SBOM"
	}
	return ""
}

// fallbackTagPattern is a tag that names the manifest it belongs to by that
// manifest's digest: sha256-<hex>.sig, .att and .sbom as cosign tags its
// signatures, attestations and SBOMs, and sha256-<hex> as the distribution
// specification's tag schema names an index of referrers, for registries
// without the referrers API.
var fallbackTagPattern = regexp.MustCompile(`^(sha256|sha512)-([0-9a-f]{64}|[0-9a-f]{128})(\.(sig|att|sbom))?$`)

// fallbackTagSubject reads the digest a fallback tag is named after, and its
// suffix without the dot: "" for the specification's index of referrers.
func fallbackTagSubject(tag string) (digest.Digest, string, bool) {
	match := fallbackTagPattern.FindStringSubmatch(tag)
	if match == nil {
		return "", "", false
	}
	d := digest.NewDigestFromEncoded(digest.Algorithm(match[1]), match[2])
	if d.Validate() != nil {
		return "", "", false
	}
	return d, match[4], true
}
