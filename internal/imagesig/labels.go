package imagesig

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Label signatures are the other way an image can carry its signature: not
// as a manifest of its own next to the image, but in the labels of the
// image's configuration, as image-sign.sh of container-image-validation
// writes them with umoci, which adds no layer:
//
//	<prefix>.signature  base64 of an Ed25519 signature over LAYER_HASH
//	<prefix>.certchain  base64 of the PEM of the signing certificate and
//	                    the intermediates it was issued by
//	<prefix>.alg        ed25519
//
// where LAYER_HASH is the SHA-256 of the image's rootfs diff_ids, one per
// line with a newline after the last, written as 64 lowercase hex digits;
// that string, without a newline, is what is signed. The signature covers
// the layers only: the rest of the configuration, its entrypoint, its
// environment and the user it runs as, and the name of the image, are not
// signed.

// LabelTrust is who may sign an image in its labels: the label prefixes the
// signature is looked for under, the root certificates its certificate has
// to chain to, and the Ed25519 keys that are trusted as they are, whatever
// certificate carries them.
type LabelTrust struct {
	Name     string
	Prefixes []string
	// Roots are the CAs a signing certificate has to chain to, through
	// the intermediates the image carries. Nil trusts no CA.
	Roots *x509.CertPool
	// Keys are signing keys trusted without a chain: a certificate that
	// carries one of them is taken whoever issued it and whenever it
	// expires.
	Keys []ed25519.PublicKey
}

// LabelOutcome is what checking the label signature of an image came to.
type LabelOutcome int

const (
	// LabelMissing is an image that carries no signature labels under any
	// prefix of the trusts.
	LabelMissing LabelOutcome = iota
	// LabelValid is a signature that a trust verified.
	LabelValid
	// LabelInvalid is a signature that is there but that no trust
	// verified, or labels that are not a signature at all.
	LabelInvalid
)

func (o LabelOutcome) String() string {
	switch o {
	case LabelValid:
		return "valid"
	case LabelInvalid:
		return "invalid"
	}
	return "missing"
}

// LabelResult is what VerifyLabels found out about an image.
type LabelResult struct {
	Outcome   LabelOutcome
	TrustedBy string // LabelTrust.Name that verified it
	Prefix    string // the label prefix the verified signature was under
	Signer    string // the subject of the signing certificate and its key's fingerprint
	Err       string // why it is not valid
}

// LayerHash is LAYER_HASH of an image with these rootfs diff_ids, byte for
// byte what `jq -r '.rootfs.diff_ids[]' | grep . | sha256sum` makes of them:
// each on its own line with a newline after the last, empty ones dropped.
func LayerHash(diffIDs []string) string {
	lines := make([]string, 0, len(diffIDs))
	for _, d := range diffIDs {
		if strings.TrimSpace(d) != "" {
			lines = append(lines, d)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// ParseLabelTrustMaterial reads the PEM blocks a label trust is made of:
// CERTIFICATE blocks, each a CA's, that signing certificates chain to, and
// PUBLIC KEY blocks, each an Ed25519 key, trusted as they are. Any other
// block, and text that holds no block at all, is refused.
func ParseLabelTrustMaterial(b []byte) ([]*x509.Certificate, []ed25519.PublicKey, error) {
	var (
		roots []*x509.Certificate
		keys  []ed25519.PublicKey
	)
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("a certificate cannot be parsed: %w", err)
			}
			if !c.BasicConstraintsValid || !c.IsCA {
				return nil, nil, fmt.Errorf("the certificate of %q is not a CA's; paste the CA, or the signer's public key", certName(c))
			}
			roots = append(roots, c)
		case "PUBLIC KEY":
			k, err := x509.ParsePKIXPublicKey(blk.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("a public key cannot be parsed: %w", err)
			}
			ed, ok := k.(ed25519.PublicKey)
			if !ok {
				return nil, nil, fmt.Errorf("a public key is %s, but label signatures are made with Ed25519", keyType(k))
			}
			keys = append(keys, ed)
		default:
			return nil, nil, fmt.Errorf("a %s block is neither a certificate nor a public key", blk.Type)
		}
	}
	if strings.TrimSpace(string(rest)) != "" && strings.Contains(string(rest), "-----BEGIN") {
		return nil, nil, errors.New("a PEM block is cut off or damaged")
	}
	if len(roots) == 0 && len(keys) == 0 {
		return nil, nil, errors.New("holds no PEM certificate or public key")
	}
	return roots, keys, nil
}

// NewLabelTrust builds a LabelTrust from its PEM material, as
// ParseLabelTrustMaterial reads it.
func NewLabelTrust(name string, prefixes []string, material []byte) (*LabelTrust, error) {
	roots, keys, err := ParseLabelTrustMaterial(material)
	if err != nil {
		return nil, err
	}
	t := &LabelTrust{Name: name, Prefixes: prefixes, Keys: keys}
	if len(roots) > 0 {
		t.Roots = x509.NewCertPool()
		for _, c := range roots {
			t.Roots.AddCert(c)
		}
	}
	return t, nil
}

// VerifyLabels checks the label signature of the image whose configuration
// is config against trusts. An image with no signature label under any of
// their prefixes is LabelMissing; labels that are there are LabelValid when
// a trust verifies them and LabelInvalid otherwise. It never panics on
// hostile input.
func VerifyLabels(config []byte, trusts []*LabelTrust) LabelResult {
	if len(config) > maxManifest {
		return LabelResult{Outcome: LabelInvalid, Err: "the image configuration is too large"}
	}
	var cfg struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return LabelResult{Outcome: LabelInvalid, Err: "not an image configuration: " + err.Error()}
	}
	labels := cfg.Config.Labels
	found := false
	failed := trustErrors{what: "label prefixes"}
	var prefixes []string
	for _, trust := range trusts {
		for _, prefix := range trust.Prefixes {
			if !containsFold(prefixes, prefix) {
				prefixes = append(prefixes, prefix)
			}
			if labels[prefix+".signature"] == "" && labels[prefix+".certchain"] == "" {
				continue
			}
			found = true
			signer, err := verifyLabelSignature(cfg.RootFS.DiffIDs, labels, prefix, trust)
			if err == nil {
				return LabelResult{Outcome: LabelValid, TrustedBy: trust.Name, Prefix: prefix, Signer: signer}
			}
			failed.add(trust.Name, err)
		}
	}
	if !found {
		return LabelResult{Outcome: LabelMissing,
			Err: "the image carries no signature labels under " + strings.Join(prefixes, ", ")}
	}
	return LabelResult{Outcome: LabelInvalid, Err: failed.String()}
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// verifyLabelSignature checks the signature under one prefix against one
// trust, and names the signer.
func verifyLabelSignature(diffIDs []string, labels map[string]string, prefix string, trust *LabelTrust) (string, error) {
	sigB64, chainB64 := labels[prefix+".signature"], labels[prefix+".certchain"]
	switch {
	case sigB64 == "":
		return "", fmt.Errorf("the label %s.signature is missing", prefix)
	case chainB64 == "":
		return "", fmt.Errorf("the label %s.certchain is missing", prefix)
	}
	if alg := labels[prefix+".alg"]; alg != "" && !strings.EqualFold(alg, "ed25519") {
		return "", fmt.Errorf("the signature is made with %q, not ed25519", alg)
	}
	leaf, intermediates, err := parseLabelChain(chainB64)
	if err != nil {
		return "", err
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("the signing certificate %q holds %s, not an Ed25519 key", certName(leaf), keyType(leaf.PublicKey))
	}
	signer := certName(leaf) + " " + keyFingerprint(pub)
	if !pinned(trust.Keys, pub) {
		if trust.Roots == nil {
			return "", fmt.Errorf("the signing key of %q is none of the trusted keys", certName(leaf))
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         trust.Roots,
			Intermediates: intermediates,
			CurrentTime:   now(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err != nil {
			return "", fmt.Errorf("the signing certificate %q: %w", certName(leaf), err)
		}
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return "", fmt.Errorf("the label %s.signature is not base64", prefix)
	}
	if !ed25519.Verify(pub, []byte(LayerHash(diffIDs)), sig) {
		return "", errors.New("the signature does not match the layers of the image")
	}
	return signer, nil
}

func pinned(keys []ed25519.PublicKey, pub ed25519.PublicKey) bool {
	for _, k := range keys {
		if k.Equal(pub) {
			return true
		}
	}
	return false
}

// parseLabelChain reads the certchain label: the signing certificate first,
// then the intermediates.
func parseLabelChain(chainB64 string) (*x509.Certificate, *x509.CertPool, error) {
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(chainB64)
	chainPEM, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, nil, errors.New("the certchain label is not base64")
	}
	var certs []*x509.Certificate
	rest := chainPEM
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
			return nil, nil, fmt.Errorf("a certificate of the certchain label cannot be parsed: %w", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, nil, errors.New("the certchain label holds no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	return certs[0], intermediates, nil
}

// certName is how a certificate is named in messages: its common name, or
// its whole subject where it has none.
func certName(c *x509.Certificate) string {
	if c.Subject.CommonName != "" {
		return c.Subject.CommonName
	}
	return c.Subject.String()
}

func keyType(k any) string {
	switch k.(type) {
	case ed25519.PublicKey:
		return "an Ed25519 key"
	}
	name := fmt.Sprintf("%T", k)
	switch {
	case strings.Contains(name, "rsa"):
		return "an RSA key"
	case strings.Contains(name, "ecdsa"):
		return "an ECDSA key"
	}
	return "a key of type " + name
}

// KeyFingerprint names a public key as Result.Identity and LabelResult.Signer
// do: sha256: and the hex of the SHA-256 of its PKIX encoding.
func KeyFingerprint(pub crypto.PublicKey) string { return keyFingerprint(pub) }
