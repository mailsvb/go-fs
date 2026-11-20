package imagesig

import (
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestLayerHashGolden(t *testing.T) {
	// the value of container-image-validation's test, which is what
	// printf 'sha256:aaa\nsha256:bbb\nsha256:ccc\n' | sha256sum prints
	const want = "756f64cce9d0d0e6f99bb94e69cd82374878eacca3e957c9c15500325efe843e"
	if got := LayerHash([]string{"sha256:aaa", "sha256:bbb", "sha256:ccc"}); got != want {
		t.Fatalf("LayerHash = %s, want %s", got, want)
	}
	if got := LayerHash([]string{"sha256:aaa", "", "sha256:bbb", "  ", "sha256:ccc"}); got != want {
		t.Fatalf("empty diff_ids are not dropped: %s", got)
	}
}

// labelPKI is an Ed25519 root, an intermediate and a signing leaf, as
// image-sign.sh's PKCS #12 holds them.
type labelPKI struct {
	root, intermediate *testCA
	leaf               *x509.Certificate
	leafKey            ed25519.PrivateKey
}

func edKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func newLabelPKI(t *testing.T, leafNotAfter time.Time) *labelPKI {
	t.Helper()
	_, rootKey := edKey(t)
	root := newRoot(t, "oci-signing-ca", rootKey)
	_, interKey := edKey(t)
	inter := &testCA{key: interKey, cert: makeCert(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "oci-signing-intermediate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, interKey.Public(), root)}
	leafPub, leafKey := edKey(t)
	leaf := makeCert(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "image-signer"},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  leafNotAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}, leafPub, inter)
	return &labelPKI{root: root, intermediate: inter, leaf: leaf, leafKey: leafKey}
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func pubPEM(t *testing.T, k any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// labels signs diffIDs as image-sign.sh does.
func (p *labelPKI) labels(prefix string, diffIDs []string) map[string]string {
	chain := append(certPEM(p.leaf), certPEM(p.intermediate.cert)...)
	sig := ed25519.Sign(p.leafKey, []byte(LayerHash(diffIDs)))
	return map[string]string{
		prefix + ".signature": base64.StdEncoding.EncodeToString(sig),
		prefix + ".certchain": base64.StdEncoding.EncodeToString(chain),
		prefix + ".alg":       "ed25519",
	}
}

func imageConfig(t *testing.T, diffIDs []string, labels map[string]string) []byte {
	t.Helper()
	cfg := map[string]any{
		"architecture": "amd64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs},
		"config": map[string]any{"Labels": labels},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func labelTrust(t *testing.T, name string, prefixes []string, material ...[]byte) *LabelTrust {
	t.Helper()
	return newTestLabelTrust(t, name, prefixes, true, material...)
}

// signatureOnly is a trust that builds no chain.
func signatureOnly(t *testing.T, name string, prefixes []string, material ...[]byte) *LabelTrust {
	t.Helper()
	return newTestLabelTrust(t, name, prefixes, false, material...)
}

func newTestLabelTrust(t *testing.T, name string, prefixes []string, verifyChain bool, material ...[]byte) *LabelTrust {
	t.Helper()
	var all []byte
	for _, m := range material {
		all = append(all, m...)
	}
	trust, err := NewLabelTrust(name, prefixes, all, verifyChain)
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

func TestVerifyLabels(t *testing.T) {
	const prefix = "org.mitel.imagesign"
	diffIDs := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}
	pki := newLabelPKI(t, time.Now().Add(30*24*time.Hour))
	other := newLabelPKI(t, time.Now().Add(30*24*time.Hour))
	byRoot := labelTrust(t, "mitel", []string{prefix}, certPEM(pki.root.cert))
	byKey := labelTrust(t, "pinned", []string{prefix}, pubPEM(t, pki.leaf.PublicKey))
	byOther := labelTrust(t, "other", []string{prefix}, certPEM(other.root.cert), pubPEM(t, other.leaf.PublicKey))

	signed := pki.labels(prefix, diffIDs)
	with := func(change func(map[string]string)) map[string]string {
		labels := map[string]string{}
		for k, v := range signed {
			labels[k] = v
		}
		change(labels)
		return labels
	}

	cases := []struct {
		name    string
		diffIDs []string
		labels  map[string]string
		trusts  []*LabelTrust
		want    LabelOutcome
		by      string
		err     string
	}{
		{"chains to the root", diffIDs, signed, []*LabelTrust{byRoot}, LabelValid, "mitel", ""},
		{"pinned key", diffIDs, signed, []*LabelTrust{byKey}, LabelValid, "pinned", ""},
		{"second trust", diffIDs, signed, []*LabelTrust{byOther, byRoot}, LabelValid, "mitel", ""},
		{"second prefix", diffIDs, pki.labels("com.example.sign", diffIDs),
			[]*LabelTrust{labelTrust(t, "two", []string{prefix, "com.example.sign"}, certPEM(pki.root.cert))},
			LabelValid, "two", ""},
		{"no labels", diffIDs, map[string]string{"maintainer": "x"}, []*LabelTrust{byRoot}, LabelMissing, "", "no signature labels"},
		{"labels of another prefix", diffIDs, pki.labels("com.example.sign", diffIDs), []*LabelTrust{byRoot}, LabelMissing, "",
			"it has them under com.example.sign, which no labelPrefix lists"},
		{"prefix written as the signature label", diffIDs, pki.labels("org.opencontainers.image", diffIDs),
			[]*LabelTrust{labelTrust(t, "osem", []string{"org.opencontainers.image.signature"}, certPEM(pki.root.cert))},
			LabelValid, "osem", ""},
		{"prefix written as the certchain label", diffIDs, pki.labels("org.opencontainers.image", diffIDs),
			[]*LabelTrust{labelTrust(t, "osem", []string{"org.opencontainers.image.CertChain"}, certPEM(pki.root.cert))},
			LabelValid, "osem", ""},
		{"no certchain", diffIDs, with(func(l map[string]string) { delete(l, prefix+".certchain") }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "certchain is missing"},
		{"no signature", diffIDs, with(func(l map[string]string) { delete(l, prefix+".signature") }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "signature is missing"},
		{"tampered layers", append([]string{"sha256:" + strings.Repeat("c", 64)}, diffIDs...), signed,
			[]*LabelTrust{byRoot}, LabelInvalid, "", "does not match the layers"},
		{"untrusted root", diffIDs, signed, []*LabelTrust{byOther}, LabelInvalid, "", "unknown authority"},
		{"key not pinned", diffIDs, signed,
			[]*LabelTrust{labelTrust(t, "k", []string{prefix}, pubPEM(t, other.leaf.PublicKey))},
			LabelInvalid, "", "none of the trusted keys"},
		{"wrong alg", diffIDs, with(func(l map[string]string) { l[prefix+".alg"] = "rsa" }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "not ed25519"},
		{"bad signature base64", diffIDs, with(func(l map[string]string) { l[prefix+".signature"] = "%%%" }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "not base64"},
		{"bad chain base64", diffIDs, with(func(l map[string]string) { l[prefix+".certchain"] = "%%%" }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "not base64"},
		{"empty chain", diffIDs, with(func(l map[string]string) { l[prefix+".certchain"] = "aGVsbG8=" }),
			[]*LabelTrust{byRoot}, LabelInvalid, "", "holds no certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VerifyLabels(imageConfig(t, c.diffIDs, c.labels), c.trusts)
			if got.Outcome != c.want {
				t.Fatalf("outcome %v (%s), want %v", got.Outcome, got.Err, c.want)
			}
			if got.TrustedBy != c.by {
				t.Fatalf("trusted by %q, want %q", got.TrustedBy, c.by)
			}
			if c.err != "" && !strings.Contains(got.Err, c.err) {
				t.Fatalf("error %q, want it to say %q", got.Err, c.err)
			}
			if got.Outcome == LabelValid && !strings.HasPrefix(got.Signer, "image-signer sha256:") {
				t.Fatalf("signer %q", got.Signer)
			}
		})
	}
}

func TestVerifyLabelsExpiredLeaf(t *testing.T) {
	const prefix = "org.mitel.imagesign"
	diffIDs := []string{"sha256:" + strings.Repeat("a", 64)}
	pki := newLabelPKI(t, time.Now().Add(24*time.Hour))
	config := imageConfig(t, diffIDs, pki.labels(prefix, diffIDs))
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Now().Add(48 * time.Hour) }

	got := VerifyLabels(config, []*LabelTrust{labelTrust(t, "ca", []string{prefix}, certPEM(pki.root.cert))})
	if got.Outcome != LabelInvalid || !strings.Contains(got.Err, "expired") {
		t.Fatalf("an expired signing certificate: %v %s", got.Outcome, got.Err)
	}
	// a pinned key is trusted whatever its certificate says
	got = VerifyLabels(config, []*LabelTrust{labelTrust(t, "key", []string{prefix}, pubPEM(t, pki.leaf.PublicKey))})
	if got.Outcome != LabelValid {
		t.Fatalf("a pinned key with an expired certificate: %v %s", got.Outcome, got.Err)
	}
}

func TestVerifyLabelsWithoutChain(t *testing.T) {
	const prefix = "org.mitel.imagesign"
	diffIDs := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}
	tampered := append([]string{"sha256:" + strings.Repeat("c", 64)}, diffIDs...)
	// signed by a certificate that expired yesterday, under a CA nobody
	// configured
	pki := newLabelPKI(t, time.Now().Add(-time.Minute))
	other := newLabelPKI(t, time.Now().Add(24*time.Hour))
	signed := pki.labels(prefix, diffIDs)
	bare := map[string]string{}
	for k, v := range signed {
		if k != prefix+".certchain" {
			bare[k] = v
		}
	}

	anyKey := signatureOnly(t, "any", []string{prefix})
	byKey := signatureOnly(t, "pinned", []string{prefix}, pubPEM(t, pki.leaf.PublicKey))
	byOtherKey := signatureOnly(t, "other", []string{prefix}, pubPEM(t, other.leaf.PublicKey))
	// a CA plays no part: the image is not issued by it, and is taken
	byOtherCA := signatureOnly(t, "ca", []string{prefix}, certPEM(other.root.cert))

	cases := []struct {
		name    string
		diffIDs []string
		labels  map[string]string
		trust   *LabelTrust
		want    LabelOutcome
		signer  string
		err     string
	}{
		{"no certificates, certchain", diffIDs, signed, anyKey, LabelValid, "image-signer sha256:", ""},
		{"a CA that did not issue it", diffIDs, signed, byOtherCA, LabelValid, "image-signer sha256:", ""},
		{"pinned key, certchain", diffIDs, signed, byKey, LabelValid, "image-signer sha256:", ""},
		{"pinned key, no certchain", diffIDs, bare, byKey, LabelValid, "Ed25519 key sha256:", ""},
		{"another key, certchain", diffIDs, signed, byOtherKey, LabelInvalid, "", "none of the trusted keys"},
		{"another key, no certchain", diffIDs, bare, byOtherKey, LabelInvalid, "", "under none of the trusted keys"},
		{"no key, no certchain", diffIDs, bare, anyKey, LabelInvalid, "", "no Ed25519 public key is configured"},
		{"tampered, certchain", tampered, signed, anyKey, LabelInvalid, "", "does not match the layers"},
		{"tampered, pinned key, certchain", tampered, signed, byKey, LabelInvalid, "", "does not match the layers"},
		{"tampered, pinned key, no certchain", tampered, bare, byKey, LabelInvalid, "", "under none of the trusted keys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VerifyLabels(imageConfig(t, c.diffIDs, c.labels), []*LabelTrust{c.trust})
			if got.Outcome != c.want {
				t.Fatalf("outcome %v (%s), want %v", got.Outcome, got.Err, c.want)
			}
			if c.signer != "" && !strings.HasPrefix(got.Signer, c.signer) {
				t.Fatalf("signer %q, want it to start with %q", got.Signer, c.signer)
			}
			if c.err != "" && !strings.Contains(got.Err, c.err) {
				t.Fatalf("error %q, want it to say %q", got.Err, c.err)
			}
		})
	}

	// the same image with the chain verified is refused for its expired
	// certificate, and for its missing certchain
	if got := VerifyLabels(imageConfig(t, diffIDs, signed), []*LabelTrust{labelTrust(t, "ca", []string{prefix}, certPEM(pki.root.cert))}); got.Outcome != LabelInvalid {
		t.Fatalf("an expired certificate with the chain verified: %v", got.Outcome)
	}
	if got := VerifyLabels(imageConfig(t, diffIDs, bare), []*LabelTrust{labelTrust(t, "key", []string{prefix}, pubPEM(t, pki.leaf.PublicKey))}); got.Outcome != LabelInvalid ||
		!strings.Contains(got.Err, "certchain is missing") {
		t.Fatalf("no certchain with the chain verified: %v %s", got.Outcome, got.Err)
	}
}

func TestNewLabelTrustEmptyMaterial(t *testing.T) {
	if _, err := NewLabelTrust("x", []string{"a.b"}, nil, true); err == nil {
		t.Fatal("no material with the chain verified is taken")
	}
	trust, err := NewLabelTrust("x", []string{"a.b"}, []byte("  \n"), false)
	if err != nil || trust.Roots != nil || len(trust.Keys) != 0 || trust.VerifyChain {
		t.Fatalf("no material without the chain: %+v %v", trust, err)
	}
	if _, err := NewLabelTrust("x", []string{"a.b"}, []byte("junk"), false); err == nil {
		t.Fatal("material that is no PEM is taken without the chain")
	}
}

func TestVerifyLabelsNonEd25519Leaf(t *testing.T) {
	const prefix = "org.mitel.imagesign"
	pki := newLabelPKI(t, time.Now().Add(24*time.Hour))
	ec := ecKey(t, elliptic.P256())
	leaf := makeCert(t, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "ecdsa-signer"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}, ec.Public(), pki.intermediate)
	labels := map[string]string{
		prefix + ".signature": "AAAA",
		prefix + ".certchain": base64.StdEncoding.EncodeToString(certPEM(leaf)),
	}
	got := VerifyLabels(imageConfig(t, nil, labels), []*LabelTrust{labelTrust(t, "ca", []string{prefix}, certPEM(pki.root.cert))})
	if got.Outcome != LabelInvalid || !strings.Contains(got.Err, "not an Ed25519 key") {
		t.Fatalf("an ECDSA signing certificate: %v %s", got.Outcome, got.Err)
	}
}

func TestVerifyLabelsHostileConfig(t *testing.T) {
	for _, verifyChain := range []bool{true, false} {
		trust := &LabelTrust{Name: "x", Prefixes: []string{"a.b"}, VerifyChain: verifyChain}
		for _, config := range []string{"", "null", "[]", `{"config":{"Labels":{"a.b.signature":"x"}}}`,
			`{"config":{"Labels":{"a.b.signature":"AAAA","a.b.certchain":"AAAA"}}}`, `{"rootfs":7}`} {
			got := VerifyLabels([]byte(config), []*LabelTrust{trust})
			if got.Outcome == LabelValid {
				t.Fatalf("%q verified, verifyChain %v", config, verifyChain)
			}
		}
	}
}

func TestParseLabelTrustMaterial(t *testing.T) {
	pki := newLabelPKI(t, time.Now().Add(time.Hour))
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	both := append(append(certPEM(pki.root.cert), certPEM(pki.intermediate.cert)...), pubPEM(t, pki.leaf.PublicKey)...)
	roots, keys, err := ParseLabelTrustMaterial(both)
	if err != nil || len(roots) != 2 || len(keys) != 1 {
		t.Fatalf("two CAs and a key: %d %d %v", len(roots), len(keys), err)
	}
	for _, c := range []struct {
		name, material, err string
	}{
		{"empty", "", "holds no PEM"},
		{"text", "not a certificate", "holds no PEM"},
		{"a leaf", string(certPEM(pki.leaf)), "not a CA's"},
		{"an RSA key", string(pubPEM(t, &rsaKey.PublicKey)), "RSA"},
		{"a private key", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "neither a certificate"},
		{"cut off", string(certPEM(pki.root.cert)) + "-----BEGIN CERTIFICATE-----\nAAAA\n", "cut off"},
	} {
		if _, _, err := ParseLabelTrustMaterial([]byte(c.material)); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
}
