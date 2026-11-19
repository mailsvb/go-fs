package imagesig

import (
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// bundle is a Sigstore bundle v0.1 to v0.3 in protobuf JSON.
type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		PublicKey *struct {
			Hint string `json:"hint"`
		} `json:"publicKey"`
		X509CertificateChain *struct {
			Certificates []struct {
				RawBytes string `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x509CertificateChain"`
		Certificate *struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
		TlogEntries               []tlogEntry `json:"tlogEntries"`
		TimestampVerificationData *struct {
			RFC3161Timestamps []struct {
				SignedTimestamp string `json:"signedTimestamp"`
			} `json:"rfc3161Timestamps"`
		} `json:"timestampVerificationData"`
	} `json:"verificationMaterial"`
	MessageSignature *struct {
		MessageDigest struct {
			Algorithm string `json:"algorithm"`
			Digest    string `json:"digest"`
		} `json:"messageDigest"`
		Signature string `json:"signature"`
	} `json:"messageSignature"`
	DSSEEnvelope *envelope `json:"dsseEnvelope"`
}

// Fulcio certificate extensions with the OIDC issuer: the deprecated raw
// string and the DER UTF8String that replaced it.
var (
	oidIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
)

var bundleHashes = map[string]crypto.Hash{
	"SHA2_256": crypto.SHA256,
	"SHA2_384": crypto.SHA384,
	"SHA2_512": crypto.SHA512,
}

var subjectHashes = map[digest.Algorithm]crypto.Hash{
	digest.SHA256: crypto.SHA256,
	digest.SHA384: crypto.SHA384,
	digest.SHA512: crypto.SHA512,
}

// verifyBundleManifest checks an OCI 1.1 referrer holding a Sigstore
// bundle, as cosign v3 and actions/attest push them.
func verifyBundleManifest(subject digest.Digest, m *ocispec.Manifest, fetch Fetch, trusts []*Trust) Result {
	r := Result{Format: FormatBundle}
	for _, l := range m.Layers {
		if !strings.HasPrefix(l.MediaType, mediaTypeBundlePrefix) {
			continue
		}
		b, err := fetchLayer(fetch, l)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		return VerifyBundle(subject, b, trusts)
	}
	r.Err = "the manifest has no Sigstore bundle layer"
	return r
}

// VerifyBundle checks a Sigstore bundle by itself, for a subject that is an
// image (or any artifact) of the given digest.
func VerifyBundle(subject digest.Digest, raw []byte, trusts []*Trust) Result {
	r := Result{Format: FormatBundle, Kind: KindSignature}
	var b bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		r.Err = "Sigstore bundle: " + err.Error()
		return r
	}
	if !strings.HasPrefix(b.MediaType, mediaTypeBundlePrefix) {
		r.Err = fmt.Sprintf("media type %q is not a Sigstore bundle", b.MediaType)
		return r
	}

	// What is signed, and how to check a signature over it.
	var (
		check func(pub crypto.PublicKey) ([]byte, error)
		match func(kind, version string, body []byte) error
	)
	switch {
	case b.MessageSignature != nil && b.DSSEEnvelope == nil:
		ms := b.MessageSignature
		h, ok := bundleHashes[ms.MessageDigest.Algorithm]
		if !ok {
			r.Err = fmt.Sprintf("message digest algorithm %q is not supported", ms.MessageDigest.Algorithm)
			return r
		}
		dg, err := decodeB64(ms.MessageDigest.Digest)
		if err != nil || len(dg) != h.Size() {
			r.Err = "malformed message digest"
			return r
		}
		if subjectHashes[subject.Algorithm()] != h || hex.EncodeToString(dg) != subject.Encoded() {
			r.Err = fmt.Sprintf("the signature is for %s:%x, not %s", strings.ToLower(strings.ReplaceAll(ms.MessageDigest.Algorithm, "SHA2_", "sha")), dg, subject)
			return r
		}
		sig, err := decodeB64(ms.Signature)
		if err != nil || len(sig) == 0 {
			r.Err = "malformed message signature"
			return r
		}
		check = func(pub crypto.PublicKey) ([]byte, error) { return sig, verifyDigest(pub, h, dg, sig) }
		match = matchMessage(hex.EncodeToString(dg), sig)
		if h != crypto.SHA256 {
			match = func(string, string, []byte) error {
				return errors.New("only SHA-256 message signatures can be matched to the log")
			}
		}
	case b.DSSEEnvelope != nil && b.MessageSignature == nil:
		d, err := b.DSSEEnvelope.decode()
		if err != nil {
			r.Err = err.Error()
			return r
		}
		pt, err := checkStatement(d.payloadType, d.payload, subject)
		r.PredicateType = pt
		r.Kind = kindOf(pt)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		check = d.verify
		match = func(kind, version string, body []byte) error {
			// DSSE bundles carry one signature; the log has the same.
			return matchDSSE(d.payload, d.sigs[0])(kind, version, body)
		}
	default:
		r.Err = "the bundle has neither a message signature nor a DSSE envelope"
		return r
	}

	vm := &b.VerificationMaterial
	var certs []*x509.Certificate
	switch {
	case vm.Certificate != nil:
		c, err := parseCertB64(vm.Certificate.RawBytes)
		if err != nil {
			r.Err = "bundle certificate: " + err.Error()
			return r
		}
		certs = []*x509.Certificate{c}
	case vm.X509CertificateChain != nil && len(vm.X509CertificateChain.Certificates) > 0:
		for _, rb := range vm.X509CertificateChain.Certificates {
			c, err := parseCertB64(rb.RawBytes)
			if err != nil {
				r.Err = "bundle certificate: " + err.Error()
				return r
			}
			certs = append(certs, c)
		}
	case vm.PublicKey != nil:
		return verifyBundleKey(r, vm.PublicKey.Hint, check, trusts)
	default:
		r.Err = "the bundle has no verification material"
		return r
	}
	return verifyBundleKeyless(r, &b, certs, check, match, trusts)
}

func parseCertB64(s string) (*x509.Certificate, error) {
	der, err := decodeB64(s)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// verifyBundleKey checks a bundle signed with a key of a trust. The hint,
// when it is a key fingerprint, says which key; all of them are tried.
func verifyBundleKey(r Result, hint string, check func(crypto.PublicKey) ([]byte, error), trusts []*Trust) Result {
	if h, err := decodeB64(hint); err == nil && len(h) == 32 {
		r.Identity = "sha256:" + hex.EncodeToString(h)
	}
	te := trustErrors{what: "public keys"}
	for _, t := range trusts {
		if len(t.PublicKeys) == 0 {
			continue
		}
		var last error = errBadSignature
		for _, k := range t.PublicKeys {
			if _, last = check(k); last == nil {
				r.OK, r.TrustedBy, r.Identity = true, t.Name, keyFingerprint(k)
				return r
			}
		}
		te.add(t.Name, wrongKey(last))
	}
	r.Err = te.String()
	return r
}

// verifyBundleKeyless checks a bundle with a Fulcio certificate: the
// signature, a verified time from the tlog or a timestamp authority, the
// certificate chain at that time, and the identity and issuer it names.
func verifyBundleKeyless(r Result, b *bundle, certs []*x509.Certificate, check func(crypto.PublicKey) ([]byte, error), match func(string, string, []byte) error, trusts []*Trust) Result {
	leaf := certs[0]
	r.Identity = certIdentity(leaf)
	sig, err := check(leaf.PublicKey)
	if err != nil {
		r.Err = "the signature does not verify with the bundle's certificate: " + err.Error()
		return r
	}
	te := trustErrors{what: "a Sigstore trusted root"}
	for _, t := range trusts {
		if t.SigstoreRoot == nil {
			continue
		}
		if err := verifyKeyless(b, leaf, certs[1:], sig, match, t); err != nil {
			te.add(t.Name, err)
			continue
		}
		r.OK, r.TrustedBy = true, t.Name
		return r
	}
	r.Err = te.String()
	return r
}

func verifyKeyless(b *bundle, leaf *x509.Certificate, extra []*x509.Certificate, sig []byte, match func(string, string, []byte) error, t *Trust) error {
	// Identity first: it is the most likely reason a bundle is not the
	// trust's, and cheap.
	if t.Identity == "" {
		return errors.New("the trust has no identity to match the certificate against")
	}
	id := certIdentities(leaf)
	if !anyGlob(t.Identity, id) {
		return fmt.Errorf("signed by %s, not %s", strings.Join(orUnknown(id), ", "), t.Identity)
	}
	iss := certIssuer(leaf)
	if t.Issuer != "" && iss != t.Issuer {
		return fmt.Errorf("the certificate's OIDC issuer is %s, not %s", orNone(iss), t.Issuer)
	}

	root := t.SigstoreRoot
	var times []time.Time
	vm := &b.VerificationMaterial
	for i := range vm.TlogEntries {
		tm, err := verifyTlogEntry(&vm.TlogEntries[i], root, match)
		if err != nil {
			return err
		}
		if !tm.IsZero() {
			times = append(times, tm)
		}
	}
	if tv := vm.TimestampVerificationData; tv != nil {
		for _, ts := range tv.RFC3161Timestamps {
			der, err := decodeB64(ts.SignedTimestamp)
			if err != nil {
				return fmt.Errorf("RFC 3161 timestamp: %w", err)
			}
			tm, err := verifyTimestamp(der, sig, root)
			if err != nil {
				return fmt.Errorf("RFC 3161 timestamp: %w", err)
			}
			times = append(times, tm)
		}
	}
	if len(times) == 0 {
		return errors.New("no verified signing time: the bundle has neither a signed tlog entry timestamp nor an RFC 3161 timestamp")
	}

	// The certificate must chain to a Fulcio CA and be valid at every
	// verified time.
	for _, tm := range times {
		if err := chainFulcio(leaf, extra, root, tm); err != nil {
			return err
		}
	}
	return nil
}

func chainFulcio(leaf *x509.Certificate, extra []*x509.Certificate, root *TrustedRoot, at time.Time) error {
	if len(root.CAs) == 0 {
		return errors.New("the trusted root has no certificate authority")
	}
	var last error
	for i := range root.CAs {
		ca := &root.CAs[i]
		if !ca.validAt(at) {
			continue
		}
		roots, inter := ca.roots()
		pool := inter.Clone()
		for _, e := range extra {
			pool.AddCert(e)
		}
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: pool,
			CurrentTime:   at,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		})
		if err == nil {
			return nil
		}
		last = err
	}
	if last == nil {
		return fmt.Errorf("no certificate authority of the trusted root was in service at %s", at.Format(time.RFC3339))
	}
	if !within(at, leaf.NotBefore, leaf.NotAfter) {
		return fmt.Errorf("the certificate was not valid at the signing time %s", at.Format(time.RFC3339))
	}
	return fmt.Errorf("the certificate does not chain to a certificate authority of the trusted root: %v", last)
}

// certIdentities are the subject alternative names a Fulcio certificate
// identifies its signer by.
func certIdentities(c *x509.Certificate) []string {
	var ids []string
	for _, u := range c.URIs {
		ids = append(ids, u.String())
	}
	ids = append(ids, c.EmailAddresses...)
	return ids
}

func certIdentity(c *x509.Certificate) string {
	if ids := certIdentities(c); len(ids) > 0 {
		return ids[0]
	}
	return c.Subject.String()
}

func orUnknown(ids []string) []string {
	if len(ids) == 0 {
		return []string{"an unknown identity"}
	}
	return ids
}

// certIssuer is the OIDC issuer Fulcio wrote into the certificate.
func certIssuer(c *x509.Certificate) string {
	var v1 string
	for _, e := range c.Extensions {
		switch {
		case e.Id.Equal(oidIssuerV2):
			var s string
			if _, err := asn1.UnmarshalWithParams(e.Value, &s, "utf8"); err == nil {
				return s
			}
		case e.Id.Equal(oidIssuerV1):
			v1 = string(e.Value)
		}
	}
	return v1
}

func anyGlob(pattern string, ss []string) bool {
	for _, s := range ss {
		if globMatch(pattern, s) {
			return true
		}
	}
	return false
}

// globMatch matches s against pattern, where '*' stands for any run of
// characters ('/' included) and '?' for one character.
func globMatch(pattern, s string) bool {
	p := []rune(pattern)
	r := []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(r) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == r[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
