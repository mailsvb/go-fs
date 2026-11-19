package imagesig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var testImage = digest.FromString("an image manifest")

func signMessage(t *testing.T, key crypto.Signer, msg []byte) []byte {
	t.Helper()
	if _, ok := key.(ed25519.PrivateKey); ok {
		sig, err := key.Sign(rand.Reader, msg, crypto.Hash(0))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	h := sha256.Sum256(msg)
	sig, err := key.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func manifestOf(t *testing.T, m ocispec.Manifest) []byte {
	m.SchemaVersion = 2
	m.MediaType = ocispec.MediaTypeImageManifest
	if m.Config.MediaType == "" {
		m.Config = ocispec.DescriptorEmptyJSON
	}
	return marshal(t, m)
}

// cosignSig makes a legacy cosign .sig manifest for image signed by key.
func cosignSig(t *testing.T, s store, image digest.Digest, key crypto.Signer) []byte {
	payload := []byte(fmt.Sprintf(`{"critical":{"identity":{"docker-reference":"registry.test/app"},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, image))
	l := s.put(payload)
	l.MediaType = mediaTypeSimpleSigning
	l.Annotations = map[string]string{annotationCosignSignature: encB64(signMessage(t, key, payload))}
	return manifestOf(t, ocispec.Manifest{Config: ocispec.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: s.put([]byte("{}")).Digest, Size: 2}, Layers: []ocispec.Descriptor{l}})
}

func statementFor(image digest.Digest, predicateType string) []byte {
	return []byte(fmt.Sprintf(`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"registry.test/app","digest":{%q:%q}}],"predicateType":%q,"predicate":{}}`,
		image.Algorithm(), image.Encoded(), predicateType))
}

func dsseEnvelope(t *testing.T, key crypto.Signer, payload []byte) (envelope, []byte) {
	sig := signMessage(t, key, pae(mediaTypeInToto, payload))
	var e envelope
	e.PayloadType = mediaTypeInToto
	e.Payload = encB64(payload)
	e.Signatures = append(e.Signatures, struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	}{Sig: encB64(sig)})
	return e, sig
}

func keyTrust(name string, keys ...crypto.Signer) *Trust {
	t := &Trust{Name: name}
	for _, k := range keys {
		t.PublicKeys = append(t.PublicKeys, k.Public())
	}
	return t
}

func TestCosignLegacyKeys(t *testing.T) {
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p384 := ecKey(t, elliptic.P384())
	for name, key := range map[string]crypto.Signer{"ed25519": edKey, "rsa": rsaKey, "p384": p384} {
		t.Run(name, func(t *testing.T) {
			s := store{}
			m := cosignSig(t, s, testImage, key)
			r := Verify(testImage, m, s.fetch, []*Trust{keyTrust("k", key)})
			expectOK(t, r, "k")
			expectErr(t, Verify(testImage, m, s.fetch, []*Trust{keyTrust("x", ecKey(t, elliptic.P256()))}), "not signed by any of the trusted keys")
		})
	}
}

func TestCosignLegacyAttestation(t *testing.T) {
	key := ecKey(t, elliptic.P256())
	s := store{}
	att := func(st []byte, sign crypto.Signer) []byte {
		e, _ := dsseEnvelope(t, sign, st)
		l := s.put(marshal(t, e))
		l.MediaType = mediaTypeDSSE
		l.Annotations = map[string]string{"predicateType": "https://spdx.dev/Document", annotationCosignSignature: ""}
		return manifestOf(t, ocispec.Manifest{Layers: []ocispec.Descriptor{l}})
	}
	trust := keyTrust("k", key)

	r := Verify(testImage, att(statementFor(testImage, "https://spdx.dev/Document"), key), s.fetch, []*Trust{trust})
	expectOK(t, r, "k")
	if r.Format != FormatCosign || r.Kind != KindAttestation || r.PredicateType != "https://spdx.dev/Document" {
		t.Fatalf("unexpected result %+v", r)
	}
	t.Run("wrong key", func(t *testing.T) {
		m := att(statementFor(testImage, "https://spdx.dev/Document"), ecKey(t, elliptic.P256()))
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{trust}), "not signed by any of the trusted keys")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		m := att(statementFor(otherDigest, "https://spdx.dev/Document"), key)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{trust}), "no subject of the attestation is")
	})
	t.Run("tampered payload", func(t *testing.T) {
		e, _ := dsseEnvelope(t, key, statementFor(testImage, "https://spdx.dev/Document"))
		e.Payload = encB64(statementFor(testImage, "https://cyclonedx.org/bom"))
		l := s.put(marshal(t, e))
		l.MediaType = mediaTypeDSSE
		m := manifestOf(t, ocispec.Manifest{Layers: []ocispec.Descriptor{l}})
		r := Verify(testImage, m, s.fetch, []*Trust{trust})
		expectErr(t, r, "not signed by any of the trusted keys")
		if r.PredicateType != "https://cyclonedx.org/bom" {
			t.Fatalf("predicate type %q", r.PredicateType)
		}
	})
}

func TestBundleWithKey(t *testing.T) {
	key := ecKey(t, elliptic.P256())
	der, _ := x509.MarshalPKIXPublicKey(key.Public())
	hint := sha256.Sum256(der)
	trust := keyTrust("k", key)

	t.Run("DSSE cosign v3 signature", func(t *testing.T) {
		e, _ := dsseEnvelope(t, key, statementFor(testImage, predicateCosignSign))
		b := marshal(t, map[string]any{
			"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
			"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": encB64(hint[:])}},
			"dsseEnvelope":         e,
		})
		s := store{}
		r := Verify(testImage, bundleManifest(t, s, testImage, b), s.fetch, []*Trust{trust})
		expectOK(t, r, "k")
		if r.Kind != KindSignature || r.PredicateType != predicateCosignSign {
			t.Fatalf("unexpected result %+v", r)
		}
		r = Verify(testImage, bundleManifest(t, s, testImage, b), s.fetch, []*Trust{keyTrust("x", ecKey(t, elliptic.P256()))})
		expectErr(t, r, "not signed by any of the trusted keys")
	})
	t.Run("message signature", func(t *testing.T) {
		manifest := []byte(`{"schemaVersion":2}`)
		subject := digest.FromBytes(manifest)
		sig := signMessage(t, key, manifest)
		sum := sha256.Sum256(manifest)
		b := marshal(t, map[string]any{
			"mediaType":            "application/vnd.dev.sigstore.bundle.v0.2+json",
			"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": ""}},
			"messageSignature": map[string]any{
				"messageDigest": map[string]string{"algorithm": "SHA2_256", "digest": encB64(sum[:])},
				"signature":     encB64(sig),
			},
		})
		expectOK(t, VerifyBundle(subject, b, []*Trust{trust}), "k")
		expectErr(t, VerifyBundle(testImage, b, []*Trust{trust}), "the signature is for sha256:")
		sig[10] ^= 1
		b = marshal(t, map[string]any{
			"mediaType":            "application/vnd.dev.sigstore.bundle.v0.2+json",
			"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": ""}},
			"messageSignature": map[string]any{
				"messageDigest": map[string]string{"algorithm": "SHA2_256", "digest": encB64(sum[:])},
				"signature":     encB64(sig),
			},
		})
		expectErr(t, VerifyBundle(subject, b, []*Trust{trust}), "not signed by any of the trusted keys")
	})
}

// keylessFixture is a test Fulcio and TSA with a GitHub-style TSA-only
// bundle (what GitHub writes for private repositories).
type keylessFixture struct {
	fulcioRoot, fulcioInter, tsaRoot *testCA
	tsa                              *testCA
	root                             []byte
}

const (
	testIdentity = "https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1.2.3"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

func newKeyless(t *testing.T) *keylessFixture {
	f := &keylessFixture{}
	f.fulcioRoot = newRoot(t, "fulcio root", ecKey(t, elliptic.P384()))
	f.fulcioInter = newIntermediate(t, "fulcio intermediate", f.fulcioRoot, x509.ExtKeyUsageCodeSigning)
	f.tsaRoot = newRoot(t, "tsa root", ecKey(t, elliptic.P384()))
	tsaKey := ecKey(t, elliptic.P256())
	f.tsa = &testCA{key: tsaKey}
	f.tsa.cert = makeCert(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "tsa", Organization: []string{"imagesig test"}},
		NotBefore:   f.tsaRoot.cert.NotBefore,
		NotAfter:    f.tsaRoot.cert.NotAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}, tsaKey.Public(), f.tsaRoot)
	f.root = rootJSON(t, []*x509.Certificate{f.fulcioInter.cert, f.fulcioRoot.cert}, []*x509.Certificate{f.tsa.cert, f.tsaRoot.cert}, time.Now().Add(-365*24*time.Hour))
	return f
}

func (f *keylessFixture) trust(t *testing.T, identity, issuer string) *Trust {
	dir := t.TempDir()
	p := filepath.Join(dir, "trusted_root.json")
	if err := os.WriteFile(p, f.root, 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := LoadTrust("keyless", "", "", p, identity, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// bundle signs a provenance statement for image with a fresh Fulcio leaf
// issued at certTime and a timestamp at tsTime.
func (f *keylessFixture) bundle(t *testing.T, image digest.Digest, identity string, certTime, tsTime time.Time) []byte {
	leaf, key := fulcioLeaf(t, f.fulcioInter, identity, testIssuer, certTime)
	e, sig := dsseEnvelope(t, key, statementFor(image, "https://slsa.dev/provenance/v1"))
	return marshal(t, map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"certificate": map[string]string{"rawBytes": encB64(leaf.Raw)},
			"timestampVerificationData": map[string]any{"rfc3161Timestamps": []any{
				map[string]string{"signedTimestamp": encB64(rfc3161(t, f.tsa, sig, tsTime, true))},
			}},
		},
		"dsseEnvelope": e,
	})
}

func TestKeylessTimestampOnly(t *testing.T) {
	f := newKeyless(t)
	signed := time.Now().Add(-48 * time.Hour)
	trust := f.trust(t, "https://github.com/o/r/.github/workflows/release.yml@refs/tags/*", testIssuer)
	b := f.bundle(t, testImage, testIdentity, signed, signed.Add(time.Minute))

	s := store{}
	r := Verify(testImage, bundleManifest(t, s, testImage, b), s.fetch, []*Trust{trust})
	expectOK(t, r, "keyless")
	if r.Identity != testIdentity || r.PredicateType != "https://slsa.dev/provenance/v1" || r.Kind != KindAttestation {
		t.Fatalf("unexpected result %+v", r)
	}

	t.Run("timestamp after the certificate expired", func(t *testing.T) {
		b := f.bundle(t, testImage, testIdentity, signed, signed.Add(time.Hour))
		expectErr(t, VerifyBundle(testImage, b, []*Trust{trust}), "the certificate was not valid at the signing time")
	})
	t.Run("wrong identity", func(t *testing.T) {
		b := f.bundle(t, testImage, "https://github.com/o/r/.github/workflows/ci.yml@refs/heads/main", signed, signed.Add(time.Minute))
		expectErr(t, VerifyBundle(testImage, b, []*Trust{trust}), "signed by https://github.com/o/r/.github/workflows/ci.yml@refs/heads/main, not")
	})
	t.Run("wrong issuer", func(t *testing.T) {
		tr := f.trust(t, "https://github.com/o/r/*", "https://gitlab.com")
		expectErr(t, VerifyBundle(testImage, b, []*Trust{tr}), "OIDC issuer is https://token.actions.githubusercontent.com, not https://gitlab.com")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		expectErr(t, VerifyBundle(otherDigest, b, []*Trust{trust}), "no subject of the attestation")
	})
	t.Run("other TSA", func(t *testing.T) {
		other := newKeyless(t)
		tr := other.trust(t, "https://github.com/o/r/*", testIssuer)
		tr.SigstoreRoot.CAs = trust.SigstoreRoot.CAs
		expectErr(t, VerifyBundle(testImage, b, []*Trust{tr}), "RFC 3161 timestamp")
	})
	t.Run("other Fulcio", func(t *testing.T) {
		other := newKeyless(t)
		tr := other.trust(t, "https://github.com/o/r/*", testIssuer)
		tr.SigstoreRoot.TSAs = trust.SigstoreRoot.TSAs
		expectErr(t, VerifyBundle(testImage, b, []*Trust{tr}), "does not chain to a certificate authority")
	})
	t.Run("timestamp signer only in the trusted root", func(t *testing.T) {
		leaf, key := fulcioLeaf(t, f.fulcioInter, testIdentity, testIssuer, signed)
		e, sig := dsseEnvelope(t, key, statementFor(testImage, "https://slsa.dev/provenance/v1"))
		b := marshal(t, map[string]any{
			"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
			"verificationMaterial": map[string]any{
				"x509CertificateChain": map[string]any{"certificates": []any{map[string]string{"rawBytes": encB64(leaf.Raw)}}},
				"timestampVerificationData": map[string]any{"rfc3161Timestamps": []any{
					map[string]string{"signedTimestamp": encB64(rfc3161(t, f.tsa, sig, signed.Add(time.Minute), false))},
				}},
			},
			"dsseEnvelope": e,
		})
		expectOK(t, VerifyBundle(testImage, b, []*Trust{trust}), "keyless")
	})
	t.Run("tampered payload", func(t *testing.T) {
		b := edit(t, b, func(m map[string]any) {
			env := obj(m, "dsseEnvelope")
			env["payload"] = encB64(statementFor(testImage, "https://example.com/other"))
		})
		expectErr(t, VerifyBundle(testImage, b, []*Trust{trust}), "does not verify with the bundle's certificate")
	})
	t.Run("trust without identity", func(t *testing.T) {
		tr := &Trust{Name: "x", SigstoreRoot: trust.SigstoreRoot}
		expectErr(t, VerifyBundle(testImage, b, []*Trust{tr}), "no identity")
	})
	t.Run("first matching trust wins", func(t *testing.T) {
		wrong := f.trust(t, "https://github.com/x/*", testIssuer)
		wrong.Name = "wrong"
		r := VerifyBundle(testImage, b, []*Trust{wrong, trust})
		expectOK(t, r, "keyless")
		r = VerifyBundle(testImage, b, []*Trust{wrong, keyTrust("keys", ecKey(t, elliptic.P256()))})
		expectErr(t, r, "not https://github.com/x/*")
	})
}

// notationFixture is a Notation signing CA and leaf.
type notationFixture struct {
	root *testCA
	leaf *x509.Certificate
	key  crypto.Signer
}

func newNotation(t *testing.T, key crypto.Signer, notBefore, notAfter time.Time) *notationFixture {
	f := &notationFixture{root: newRoot(t, "notation root", ecKey(t, elliptic.P384())), key: key}
	f.leaf = makeCert(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "release signer", Organization: []string{"Example"}},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}, key.Public(), f.root)
	return f
}

func (f *notationFixture) trust() *Trust {
	p := x509.NewCertPool()
	p.AddCert(f.root.cert)
	return &Trust{Name: "notation", Roots: p}
}

// sign makes a Notation JWS manifest for target with the given protected
// header fields on top of the usual ones.
func (f *notationFixture) sign(t *testing.T, s store, subject, target digest.Digest, alg string, extra map[string]any) []byte {
	hdr := map[string]any{
		"alg":             alg,
		"cty":             notaryPayloadType,
		"crit":            []string{notaryScheme},
		notaryScheme:      schemeX509,
		notarySigningTime: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	for k, v := range extra {
		if v == nil {
			delete(hdr, k)
		} else {
			hdr[k] = v
		}
	}
	protected := base64.RawURLEncoding.EncodeToString(marshal(t, hdr))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"targetArtifact":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1234}}`, target)))
	input := []byte(protected + "." + payload)
	var sig []byte
	switch k := f.key.(type) {
	case *rsa.PrivateKey:
		h := crypto.SHA256
		if alg == "PS384" {
			h = crypto.SHA384
		}
		var err error
		sig, err = rsa.SignPSS(rand.Reader, k, h, digestOf(h, input), &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			t.Fatal(err)
		}
	case *ecdsa.PrivateKey:
		h := map[string]crypto.Hash{"ES256": crypto.SHA256, "ES384": crypto.SHA384, "ES512": crypto.SHA512}[alg]
		r, ss, err := ecdsa.Sign(rand.Reader, k, digestOf(h, input))
		if err != nil {
			t.Fatal(err)
		}
		n := (k.Curve.Params().BitSize + 7) / 8
		sig = append(r.FillBytes(make([]byte, n)), ss.FillBytes(make([]byte, n))...)
	}
	env := map[string]any{
		"payload":   payload,
		"protected": protected,
		"header": map[string]any{
			"x5c":                         []string{encB64(f.leaf.Raw), encB64(f.root.cert.Raw)},
			"io.cncf.notary.signingAgent": "notation-go/1.3.0",
		},
		"signature": base64.RawURLEncoding.EncodeToString(sig),
	}
	l := s.put(marshal(t, env))
	l.MediaType = mediaTypeJWS
	return manifestOf(t, ocispec.Manifest{
		ArtifactType: mediaTypeNotation,
		Layers:       []ocispec.Descriptor{l},
		Subject:      &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject, Size: 1234},
	})
}

func TestNotationJWS(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	rsaF := newNotation(t, rsaKey, time.Now().Add(-day), time.Now().Add(365*day))
	ecF := newNotation(t, ecKey(t, elliptic.P384()), time.Now().Add(-day), time.Now().Add(365*day))
	s := store{}

	t.Run("PS256", func(t *testing.T) {
		r := Verify(testImage, rsaF.sign(t, s, testImage, testImage, "PS256", nil), s.fetch, []*Trust{rsaF.trust()})
		expectOK(t, r, "notation")
		if r.Format != FormatNotation || r.Kind != KindSignature || r.Identity != "CN=release signer,O=Example" {
			t.Fatalf("unexpected result %+v", r)
		}
	})
	t.Run("ES384 with expiry ahead", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{
			"crit": []string{notaryScheme, notaryExpiry}, notaryExpiry: time.Now().Add(day).Format(time.RFC3339),
		})
		expectOK(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "notation")
	})
	t.Run("wrong root", func(t *testing.T) {
		m := rsaF.sign(t, s, testImage, testImage, "PS256", nil)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "certificate chain")
	})
	t.Run("no Notation trust", func(t *testing.T) {
		m := rsaF.sign(t, s, testImage, testImage, "PS256", nil)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{keyTrust("k", rsaKey)}), "no trust policy with Notation certificates")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		m := rsaF.sign(t, s, testImage, otherDigest, "PS256", nil)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{rsaF.trust()}), "the signature is for "+string(otherDigest))
	})
	t.Run("manifest for another subject", func(t *testing.T) {
		m := rsaF.sign(t, s, otherDigest, testImage, "PS256", nil)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{rsaF.trust()}), "refers to "+string(otherDigest))
	})
	t.Run("tampered payload", func(t *testing.T) {
		m := rsaF.sign(t, s, testImage, testImage, "PS256", nil)
		var mm ocispec.Manifest
		_ = json.Unmarshal(m, &mm)
		env := edit(t, s[mm.Layers[0].Digest], func(e map[string]any) {
			p, _ := base64.RawURLEncoding.DecodeString(e["payload"].(string))
			e["payload"] = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(p), "1234", "1235", 1)))
		})
		l := s.put(env)
		l.MediaType = mediaTypeJWS
		mm.Layers[0] = l
		expectErr(t, Verify(testImage, marshal(t, mm), s.fetch, []*Trust{rsaF.trust()}), "the JWS signature does not verify")
	})
	t.Run("expired", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{
			"crit": []string{notaryScheme, notaryExpiry}, notaryExpiry: time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "the signature expired at")
	})
	t.Run("expiry not critical", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{notaryExpiry: time.Now().Add(day).Format(time.RFC3339)})
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "expiry header is not marked critical")
	})
	t.Run("unknown critical header", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{"crit": []string{notaryScheme, "x-new"}, "x-new": true})
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), `unknown critical header "x-new"`)
	})
	t.Run("algorithm not matching the key", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{"alg": "ES256"})
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "curve does not go with ES256")
	})
	t.Run("expired certificate", func(t *testing.T) {
		old := newNotation(t, ecKey(t, elliptic.P256()), time.Now().Add(-30*day), time.Now().Add(-day))
		m := old.sign(t, s, testImage, testImage, "ES256", nil)
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{old.trust()}), "certificate chain")

		// Under the signingAuthority scheme the authentic signing time
		// counts, when the certificate was valid.
		m = old.sign(t, s, testImage, testImage, "ES256", map[string]any{
			"crit":              []string{notaryScheme, notaryAuthenticTime},
			notaryScheme:        schemeSigningAuthority,
			notaryAuthenticTime: time.Now().Add(-2 * day).Format(time.RFC3339),
			notarySigningTime:   nil,
		})
		expectOK(t, Verify(testImage, m, s.fetch, []*Trust{old.trust()}), "notation")
	})
	t.Run("clock", func(t *testing.T) {
		m := ecF.sign(t, s, testImage, testImage, "ES384", map[string]any{
			"crit": []string{notaryScheme, notaryExpiry}, notaryExpiry: time.Now().Add(day).Format(time.RFC3339),
		})
		defer func(f func() time.Time) { now = f }(now)
		now = func() time.Time { return time.Now().Add(2 * day) }
		expectErr(t, Verify(testImage, m, s.fetch, []*Trust{ecF.trust()}), "the signature expired at")
	})
}

func TestVerifyMalformed(t *testing.T) {
	for name, tc := range map[string]struct {
		subject  digest.Digest
		manifest string
		want     string
	}{
		"bad subject":  {"sha256:xyz", `{}`, "invalid subject digest"},
		"not json":     {testImage, `{`, "not an OCI manifest"},
		"plain image":  {testImage, `{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"` + string(testImage) + `","size":1}]}`, "not a signature manifest"},
		"missing blob": {testImage, `{"schemaVersion":2,"layers":[{"mediaType":"` + mediaTypeSimpleSigning + `","digest":"` + string(testImage) + `","size":1}]}`, "fetching layer"},
		"cose":         {testImage, `{"schemaVersion":2,"artifactType":"` + mediaTypeNotation + `","layers":[{"mediaType":"application/cose","digest":"` + string(testImage) + `","size":1}]}`, "COSE"},
		"bundle junk":  {testImage, `{"schemaVersion":2,"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","layers":[{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","digest":"` + string(digest.FromString("x")) + `","size":1}]}`, "Sigstore bundle"},
	} {
		t.Run(name, func(t *testing.T) {
			s := store{}
			s.put([]byte("x"))
			r := Verify(tc.subject, []byte(tc.manifest), s.fetch, nil)
			expectErr(t, r, tc.want)
		})
	}
}

func TestLoadTrustErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadTrust("x", "", "", "", "", ""); err == nil {
		t.Fatal("no material accepted")
	}
	if _, err := LoadTrust("x", write("k.pub", "nothing"), "", "", "", ""); err == nil {
		t.Fatal("empty key file accepted")
	}
	if _, err := LoadTrust("x", "", write("c.pem", "nothing"), "", "", ""); err == nil {
		t.Fatal("empty certificate file accepted")
	}
	if _, err := LoadTrust("x", "", "", filepath.Join("testdata", "trusted_root-public-good.json"), "", ""); err == nil {
		t.Fatal("trusted root without identity accepted")
	}
	if _, err := LoadTrust("x", "", "", write("r.json", `{"mediaType":"application/json"}`), "*", ""); err == nil {
		t.Fatal("foreign trusted root accepted")
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKIXPublicKey(small.Public())
	if _, err := ParsePublicKeys([]byte("-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n")); err == nil {
		t.Fatal("1024-bit RSA key accepted")
	}
	f := newNotation(t, ecKey(t, elliptic.P256()), time.Now(), time.Now().Add(time.Hour))
	tr, err := LoadTrust("n", "", write("roots.der", string(f.root.cert.Raw)), "", "", "")
	if err != nil || tr.Roots == nil {
		t.Fatalf("DER root: %v", err)
	}
}
