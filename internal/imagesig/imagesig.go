// Package imagesig verifies container-image signatures and attestations that
// are stored in an OCI registry as manifests of their own: cosign's legacy
// .sig and .att tags (key-based), Sigstore bundles v0.1 to v0.3 (what cosign
// v3 and GitHub's actions/attest push as OCI 1.1 referrers, key-based or
// keyless with Fulcio, Rekor and RFC 3161 timestamps) and Notation JWS
// signatures.
//
// It uses only the standard library: the Sigstore trusted root, Rekor
// entries, Merkle inclusion proofs, signed checkpoints, RFC 3161 timestamp
// tokens and JWS are parsed and checked here rather than through sigstore-go
// or notation-go. What it does not check is listed with each format: the
// signed certificate timestamps (CT) of Fulcio certificates, TUF freshness of
// the trusted root (it is whatever file the operator configured), Notation
// timestamp countersignatures and revocation, and COSE Notation signatures.
package imagesig

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Trust is one trust policy: the keys, certificate roots and Sigstore trusted
// root that may vouch for a signature, and for keyless signatures which
// identity the signing certificate has to carry.
type Trust struct {
	Name string
	// PublicKeys verify cosign key-based signatures: legacy .sig and .att
	// layers, and Sigstore bundles that name a public key instead of
	// carrying a certificate.
	PublicKeys []crypto.PublicKey
	// Roots is the Notation X.509 trust store.
	Roots *x509.CertPool
	// SigstoreRoot holds the Fulcio CAs, Rekor keys and timestamp
	// authorities of keyless signatures.
	SigstoreRoot *TrustedRoot
	// Identity is a glob ('*' matches any run of characters, '/' included,
	// '?' one character) matched against the subject alternative name of
	// the Fulcio certificate, for example
	// https://github.com/o/r/.github/workflows/release.yml@refs/tags/*.
	Identity string
	// Issuer is the OIDC issuer the certificate has to name, for example
	// https://token.actions.githubusercontent.com. Empty accepts any.
	Issuer string
}

// Fetch returns the bytes of a blob in the same repository by its digest.
type Fetch func(d digest.Digest) ([]byte, error)

// Result is what Verify found out about one signature manifest.
type Result struct {
	Format        string // "cosign", "sigstore-bundle", "notation"
	Kind          string // "signature" or "attestation"
	PredicateType string // for attestations (SLSA provenance, SPDX, CycloneDX, ...)
	TrustedBy     string // Trust.Name that verified it, "" if none
	Identity      string // signer: key fingerprint, cert SAN, or cert subject
	OK            bool
	Err           string // why not OK (human readable)
}

// Formats and kinds as reported in Result.
const (
	FormatCosign   = "cosign"
	FormatBundle   = "sigstore-bundle"
	FormatNotation = "notation"

	KindSignature   = "signature"
	KindAttestation = "attestation"
)

// Media types and annotations of the formats Verify reads.
const (
	mediaTypeSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	mediaTypeDSSE          = "application/vnd.dsse.envelope.v1+json"
	mediaTypeBundlePrefix  = "application/vnd.dev.sigstore.bundle"
	mediaTypeNotation      = "application/vnd.cncf.notary.signature"
	mediaTypeJWS           = "application/jose+json"
	mediaTypeCOSE          = "application/cose"
	mediaTypeInToto        = "application/vnd.in-toto+json"

	annotationCosignSignature   = "dev.cosignproject.cosign/signature"
	annotationCosignCertificate = "dev.sigstore.cosign/certificate"

	predicateCosignSign = "https://sigstore.dev/cosign/sign/v1"
)

// now is the clock Notation expiry and certificate validity are checked
// against; tests move it.
var now = time.Now

// maxManifest bounds what Verify parses; signature manifests are small.
const maxManifest = 4 << 20

// Verify checks the signature or attestation manifest that refers to the
// image subject against trusts, fetching the layers it needs with fetch. It
// never panics on hostile input; whatever goes wrong ends up in Result.Err.
// A manifest that verifies under the first matching trust stops the search.
func Verify(subject digest.Digest, manifest []byte, fetch Fetch, trusts []*Trust) Result {
	if err := subject.Validate(); err != nil {
		return Result{Err: "invalid subject digest: " + err.Error()}
	}
	if len(manifest) > maxManifest {
		return Result{Err: "signature manifest is too large"}
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return Result{Err: "not an OCI manifest: " + err.Error()}
	}
	if fetch == nil {
		fetch = func(digest.Digest) ([]byte, error) { return nil, errors.New("no blob store") }
	}
	switch {
	case m.ArtifactType == mediaTypeNotation || m.Config.MediaType == mediaTypeNotation:
		return verifyNotation(subject, &m, fetch, trusts)
	case strings.HasPrefix(m.ArtifactType, mediaTypeBundlePrefix) || hasLayerPrefix(&m, mediaTypeBundlePrefix):
		return verifyBundleManifest(subject, &m, fetch, trusts)
	case hasLayerPrefix(&m, mediaTypeSimpleSigning) || hasLayerPrefix(&m, mediaTypeDSSE):
		return verifyCosign(subject, &m, fetch, trusts)
	}
	return Result{Err: "not a signature manifest (no cosign, Sigstore bundle or Notation layer)"}
}

func hasLayerPrefix(m *ocispec.Manifest, prefix string) bool {
	for _, l := range m.Layers {
		if strings.HasPrefix(l.MediaType, prefix) {
			return true
		}
	}
	return false
}

// fetchLayer fetches a layer and checks that the bytes are the ones the
// descriptor names.
func fetchLayer(fetch Fetch, d ocispec.Descriptor) ([]byte, error) {
	if err := d.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("layer digest: %w", err)
	}
	if d.Size > maxManifest {
		return nil, fmt.Errorf("layer %s is too large (%d bytes)", d.Digest, d.Size)
	}
	if d.Data != nil {
		if d.Digest.Algorithm().FromBytes(d.Data) != d.Digest {
			return nil, fmt.Errorf("embedded data of layer %s does not match its digest", d.Digest)
		}
		return d.Data, nil
	}
	b, err := fetch(d.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetching layer %s: %w", d.Digest, err)
	}
	if d.Digest.Algorithm().FromBytes(b) != d.Digest {
		return nil, fmt.Errorf("layer %s does not match its digest", d.Digest)
	}
	if d.Size > 0 && int64(len(b)) != d.Size {
		return nil, fmt.Errorf("layer %s is %d bytes, the manifest says %d", d.Digest, len(b), d.Size)
	}
	return b, nil
}

// trustErrors collects why each applicable trust refused a signature.
type trustErrors struct {
	what  string // the kind of trust material, for "no trust with ..."
	names []string
	errs  []error
}

func (t *trustErrors) add(name string, err error) {
	t.names = append(t.names, name)
	t.errs = append(t.errs, err)
}

func (t *trustErrors) String() string {
	switch len(t.errs) {
	case 0:
		return "no trust policy with " + t.what + " is configured"
	case 1:
		return t.errs[0].Error()
	}
	// The same reason under every trust is said once.
	same := true
	for _, e := range t.errs[1:] {
		if e.Error() != t.errs[0].Error() {
			same = false
		}
	}
	if same {
		return t.errs[0].Error()
	}
	parts := make([]string, len(t.errs))
	for i, e := range t.errs {
		name := t.names[i]
		if name == "" {
			name = fmt.Sprintf("trust %d", i+1)
		}
		parts[i] = name + ": " + e.Error()
	}
	return strings.Join(parts, "; ")
}

// keyFingerprint is how a public key is named in Result.Identity: the
// SHA-256 of its PKIX DER encoding, which is also what Sigstore bundles put
// in their public-key hint.
func keyFingerprint(pub crypto.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LoadTrust builds a Trust from files, any of which may be empty but not
// all: publicKeyFile holds PEM public keys (cosign.pub), certificatesFile PEM
// or DER root certificates (the Notation trust store), and trustedRootFile a
// Sigstore trusted_root.json, or several of them one per line as
// `gh attestation trusted-root` prints them. A trusted root needs an
// identity to match keyless certificates against.
func LoadTrust(name, publicKeyFile, certificatesFile, trustedRootFile, identity, issuer string) (*Trust, error) {
	if publicKeyFile == "" && certificatesFile == "" && trustedRootFile == "" {
		return nil, fmt.Errorf("trust %q: needs a public key, certificates or a trusted root", name)
	}
	t := &Trust{Name: name, Identity: identity, Issuer: issuer}
	if publicKeyFile != "" {
		b, err := os.ReadFile(publicKeyFile)
		if err != nil {
			return nil, fmt.Errorf("trust %q: %w", name, err)
		}
		if t.PublicKeys, err = ParsePublicKeys(b); err != nil {
			return nil, fmt.Errorf("trust %q: %s: %w", name, publicKeyFile, err)
		}
	}
	if certificatesFile != "" {
		b, err := os.ReadFile(certificatesFile)
		if err != nil {
			return nil, fmt.Errorf("trust %q: %w", name, err)
		}
		certs, err := parseCertificates(b)
		if err != nil {
			return nil, fmt.Errorf("trust %q: %s: %w", name, certificatesFile, err)
		}
		t.Roots = x509.NewCertPool()
		for _, c := range certs {
			t.Roots.AddCert(c)
		}
	}
	if trustedRootFile != "" {
		b, err := os.ReadFile(trustedRootFile)
		if err != nil {
			return nil, fmt.Errorf("trust %q: %w", name, err)
		}
		if t.SigstoreRoot, err = ParseTrustedRoot(b); err != nil {
			return nil, fmt.Errorf("trust %q: %s: %w", name, trustedRootFile, err)
		}
		if identity == "" {
			return nil, fmt.Errorf("trust %q: a trusted root needs an identity to match signing certificates against", name)
		}
	}
	return t, nil
}

// ParsePublicKeys reads the PEM public keys in b: PKIX "PUBLIC KEY" blocks
// (ECDSA, ed25519, RSA) and PKCS #1 "RSA PUBLIC KEY" blocks.
func ParsePublicKeys(b []byte) ([]crypto.PublicKey, error) {
	var keys []crypto.PublicKey
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		var (
			k   crypto.PublicKey
			err error
		)
		switch blk.Type {
		case "PUBLIC KEY":
			k, err = x509.ParsePKIXPublicKey(blk.Bytes)
		case "RSA PUBLIC KEY":
			k, err = x509.ParsePKCS1PublicKey(blk.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := checkKey(k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("no PEM public key found")
	}
	return keys, nil
}

// parseCertificates reads PEM certificates, or one DER certificate.
func parseCertificates(b []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		c, err := x509.ParseCertificate(b)
		if err != nil {
			return nil, errors.New("no certificate found")
		}
		certs = append(certs, c)
	}
	return certs, nil
}
