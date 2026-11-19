package imagesig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Tests against signatures real tools made; testdata/README.md says where
// each comes from.

const (
	ublueImage  = digest.Digest("sha256:0da9aa01d22a97fbc474d1eec650144bb0f3015ed07b057c2303f37164a4e08b")
	uvImage     = digest.Digest("sha256:d3b601f0c7b118bc1753febf7c41cc80981df678445e0254284eecf84db79a7d")
	cosignBlob  = digest.Digest("sha256:9f9b8cd557cf2e5b3df3373a00baa005a86b02391e3e06dd0ce37b3a1533d3a4")
	otherDigest = digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")

	uvIdentity     = "https://github.com/astral-sh/uv/.github/workflows/publish-docker-image.yml@refs/heads/*"
	githubIssuer   = "https://token.actions.githubusercontent.com"
	cosignIdentity = "keyless@projectsigstore.iam.gserviceaccount.com"
	googleIssuer   = "https://accounts.google.com"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// store is an in-memory blob store.
type store map[digest.Digest][]byte

func (s store) put(b []byte) ocispec.Descriptor {
	d := digest.FromBytes(b)
	s[d] = b
	return ocispec.Descriptor{Digest: d, Size: int64(len(b))}
}

func (s store) fetch(d digest.Digest) ([]byte, error) {
	if b, ok := s[d]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

// bundleManifest wraps a bundle in an OCI 1.1 referrer the way cosign v3
// and actions/attest (push-to-registry) push it.
func bundleManifest(t *testing.T, s store, subject digest.Digest, b []byte) []byte {
	t.Helper()
	l := s.put(b)
	l.MediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	cfg := ocispec.DescriptorEmptyJSON
	m := ocispec.Manifest{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		Config:       cfg,
		Layers:       []ocispec.Descriptor{l},
		Subject:      &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: subject, Size: 1},
	}
	m.SchemaVersion = 2
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func loadTrust(t *testing.T, name, pub, certs, root, identity, issuer string) *Trust {
	t.Helper()
	p := func(f string) string {
		if f == "" {
			return ""
		}
		return filepath.Join("testdata", f)
	}
	tr, err := LoadTrust(name, p(pub), p(certs), p(root), identity, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func expectOK(t *testing.T, r Result, trust string) {
	t.Helper()
	if !r.OK || r.TrustedBy != trust || r.Err != "" {
		t.Fatalf("want OK by %q, got %+v", trust, r)
	}
}

func expectErr(t *testing.T, r Result, contains string) {
	t.Helper()
	if r.OK || r.TrustedBy != "" {
		t.Fatalf("want failure, got %+v", r)
	}
	if !strings.Contains(r.Err, contains) {
		t.Fatalf("want error containing %q, got %+v", contains, r)
	}
}

func otherKeyTrust(t *testing.T) *Trust {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Trust{Name: "other", PublicKeys: []crypto.PublicKey{k.Public()}}
}

func TestRealCosignLegacySig(t *testing.T) {
	manifest := readFile(t, "cosign-ublue-sig-manifest.json")
	payload := readFile(t, "cosign-ublue-payload.json")
	s := store{}
	s.put(payload)
	ublue := loadTrust(t, "ublue", "cosign-ublue.pub", "", "", "", "")

	r := Verify(ublueImage, manifest, s.fetch, []*Trust{otherKeyTrust(t), ublue})
	expectOK(t, r, "ublue")
	if r.Format != FormatCosign || r.Kind != KindSignature || !strings.HasPrefix(r.Identity, "sha256:") {
		t.Fatalf("unexpected result %+v", r)
	}
	if r.Identity != keyFingerprint(ublue.PublicKeys[0]) {
		t.Fatalf("identity %q is not the key's fingerprint", r.Identity)
	}

	t.Run("wrong key", func(t *testing.T) {
		expectErr(t, Verify(ublueImage, manifest, s.fetch, []*Trust{otherKeyTrust(t)}), "not signed by any of the trusted keys")
	})
	t.Run("no trust", func(t *testing.T) {
		expectErr(t, Verify(ublueImage, manifest, s.fetch, nil), "no trust policy with public keys")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		expectErr(t, Verify(otherDigest, manifest, s.fetch, []*Trust{ublue}), "the signature is for sha256:0da9")
	})
	t.Run("tampered payload", func(t *testing.T) {
		// Same image digest, other docker-reference; the manifest is
		// rewritten to name the new layer so only the signature can tell.
		tampered := []byte(strings.Replace(string(payload), "ghcr.io/ublue-os/base-main", "ghcr.io/evil/base-main", 1))
		d := s.put(tampered).Digest
		var m map[string]any
		if err := json.Unmarshal(manifest, &m); err != nil {
			t.Fatal(err)
		}
		l := m["layers"].([]any)[0].(map[string]any)
		l["digest"], l["size"] = d.String(), len(tampered)
		mb, _ := json.Marshal(m)
		expectErr(t, Verify(ublueImage, mb, s.fetch, []*Trust{ublue}), "not signed by any of the trusted keys")
	})
	t.Run("layer not matching its digest", func(t *testing.T) {
		bad := store{digest.FromBytes(payload): []byte(`{"critical":{}}`)}
		expectErr(t, Verify(ublueImage, manifest, bad.fetch, []*Trust{ublue}), "does not match its digest")
	})
}

func TestRealGitHubAttestation(t *testing.T) {
	s := store{}
	raw := readFile(t, "github-uv-bundle.json")
	manifest := bundleManifest(t, s, uvImage, raw)
	good := loadTrust(t, "github", "", "", "trusted_root-public-good.json", uvIdentity, githubIssuer)

	r := Verify(uvImage, manifest, s.fetch, []*Trust{good})
	expectOK(t, r, "github")
	if r.Format != FormatBundle || r.Kind != KindAttestation || r.PredicateType != "https://slsa.dev/provenance/v1" ||
		r.Identity != "https://github.com/astral-sh/uv/.github/workflows/publish-docker-image.yml@refs/heads/main" {
		t.Fatalf("unexpected result %+v", r)
	}
}

// edit applies f to a JSON document decoded into maps and slices.
func edit(t *testing.T, b []byte, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func obj(v any, path ...string) map[string]any {
	m := v.(map[string]any)
	for _, p := range path {
		m = m[p].(map[string]any)
	}
	return m
}

func firstTlog(m map[string]any) map[string]any {
	return obj(m, "verificationMaterial")["tlogEntries"].([]any)[0].(map[string]any)
}

func TestRealGitHubAttestationNegative(t *testing.T) {
	raw := readFile(t, "github-uv-bundle.json")
	good := loadTrust(t, "github", "", "", "trusted_root-public-good.json", uvIdentity, githubIssuer)
	verify := func(subject digest.Digest, b []byte, trusts ...*Trust) Result {
		s := store{}
		return Verify(subject, bundleManifest(t, s, subject, b), s.fetch, trusts)
	}

	t.Run("wrong identity", func(t *testing.T) {
		tr := loadTrust(t, "other", "", "", "trusted_root-public-good.json", "https://github.com/astral-sh/uv/.github/workflows/release.yml@*", githubIssuer)
		r := verify(uvImage, raw, tr)
		expectErr(t, r, "signed by https://github.com/astral-sh/uv/.github/workflows/publish-docker-image.yml@refs/heads/main, not")
		if r.Identity == "" || r.PredicateType == "" {
			t.Fatalf("a refused attestation still names its signer and predicate: %+v", r)
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		tr := loadTrust(t, "other", "", "", "trusted_root-public-good.json", uvIdentity, googleIssuer)
		expectErr(t, verify(uvImage, raw, tr), "OIDC issuer is https://token.actions.githubusercontent.com, not https://accounts.google.com")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		expectErr(t, verify(otherDigest, raw, good), "no subject of the attestation is "+string(otherDigest))
	})
	t.Run("tampered payload", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			env := obj(m, "dsseEnvelope")
			p, _ := decodeB64(env["payload"].(string))
			p = []byte(strings.Replace(string(p), `"buildType"`, `"buildTyp3"`, 1))
			env["payload"] = encB64(p)
		})
		expectErr(t, verify(uvImage, b, good), "does not verify with the bundle's certificate")
	})
	t.Run("tampered inclusion proof", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			h := obj(firstTlog(m), "inclusionProof")["hashes"].([]any)
			h[3] = h[4]
		})
		expectErr(t, verify(uvImage, b, good), "inclusion proof does not lead to the root hash")
	})
	t.Run("tampered checkpoint", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			cp := obj(firstTlog(m), "inclusionProof", "checkpoint")
			cp["envelope"] = strings.Replace(cp["envelope"].(string), "rekor.sigstore.dev - ", "rekor.sigstore.dev -  ", 1)
		})
		expectErr(t, verify(uvImage, b, good), "checkpoint is not signed by")
	})
	t.Run("tampered signed entry timestamp", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			e := firstTlog(m)
			e["integratedTime"] = "1794909422"
		})
		expectErr(t, verify(uvImage, b, good), "signed entry timestamp of https://rekor.sigstore.dev")
	})
	t.Run("entry of another signature", func(t *testing.T) {
		other := readFile(t, "cosign-release-keyless.sigstore.json")
		var o map[string]any
		_ = json.Unmarshal(other, &o)
		b := edit(t, raw, func(m map[string]any) {
			obj(m, "verificationMaterial")["tlogEntries"] = obj(o, "verificationMaterial")["tlogEntries"]
		})
		expectErr(t, verify(uvImage, b, good), "a hashedrekord entry cannot log a DSSE envelope")
	})
	t.Run("no time source", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			delete(obj(m, "verificationMaterial"), "tlogEntries")
		})
		expectErr(t, verify(uvImage, b, good), "no verified signing time")
	})
	t.Run("GitHub root only", func(t *testing.T) {
		tr := loadTrust(t, "gh", "", "", "trusted_root-github.json", uvIdentity, githubIssuer)
		expectErr(t, verify(uvImage, raw, tr), "which the trusted root does not have")
	})
	t.Run("both roots in JSON lines", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "roots.jsonl")
		lines := append(readFile(t, "trusted_root-github.json"), readFile(t, "trusted_root-public-good.json")...)
		if err := os.WriteFile(f, lines, 0o600); err != nil {
			t.Fatal(err)
		}
		tr, err := LoadTrust("both", "", "", f, uvIdentity, githubIssuer)
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.SigstoreRoot.CAs) != 8 || len(tr.SigstoreRoot.TSAs) != 7 || len(tr.SigstoreRoot.Tlogs) != 2 {
			t.Fatalf("merged root: %d CAs, %d TSAs, %d logs", len(tr.SigstoreRoot.CAs), len(tr.SigstoreRoot.TSAs), len(tr.SigstoreRoot.Tlogs))
		}
		expectOK(t, verify(uvImage, raw, tr), "both")
	})
	t.Run("only key trusts", func(t *testing.T) {
		expectErr(t, verify(uvImage, raw, otherKeyTrust(t)), "no trust policy with a Sigstore trusted root")
	})
}

func TestRealCosignKeylessTimestamp(t *testing.T) {
	// cosign's own v3 release bundle: a message signature over the binary,
	// a Rekor entry with promise and inclusion proof, and an RFC 3161
	// timestamp from timestamp.sigstore.dev.
	raw := readFile(t, "cosign-release-keyless.sigstore.json")
	good := loadTrust(t, "sigstore", "", "", "trusted_root-public-good.json", cosignIdentity, googleIssuer)
	verify := func(subject digest.Digest, b []byte, trusts ...*Trust) Result {
		s := store{}
		return Verify(subject, bundleManifest(t, s, subject, b), s.fetch, trusts)
	}

	r := verify(cosignBlob, raw, good)
	expectOK(t, r, "sigstore")
	if r.Kind != KindSignature || r.Identity != cosignIdentity {
		t.Fatalf("unexpected result %+v", r)
	}
	// VerifyBundle is the same without the manifest around it.
	expectOK(t, VerifyBundle(cosignBlob, raw, []*Trust{good}), "sigstore")

	t.Run("digest mismatch", func(t *testing.T) {
		expectErr(t, verify(otherDigest, raw, good), "the signature is for sha256:9f9b8cd5")
	})
	t.Run("timestamp only", func(t *testing.T) {
		// What GitHub writes for private repositories: no tlog at all.
		b := edit(t, raw, func(m map[string]any) {
			delete(obj(m, "verificationMaterial"), "tlogEntries")
		})
		expectOK(t, verify(cosignBlob, b, good), "sigstore")
	})
	t.Run("tlog only", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			delete(obj(m, "verificationMaterial"), "timestampVerificationData")
		})
		expectOK(t, verify(cosignBlob, b, good), "sigstore")
	})
	t.Run("inclusion proof without promise or timestamp", func(t *testing.T) {
		// The integrated time is not signed without the promise.
		b := edit(t, raw, func(m map[string]any) {
			delete(obj(m, "verificationMaterial"), "timestampVerificationData")
			delete(firstTlog(m), "inclusionPromise")
		})
		expectErr(t, verify(cosignBlob, b, good), "no verified signing time")
	})
	t.Run("tlog entry without proof or promise", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			delete(firstTlog(m), "inclusionPromise")
			delete(firstTlog(m), "inclusionProof")
		})
		expectErr(t, verify(cosignBlob, b, good), "neither an inclusion promise nor an inclusion proof")
	})
	t.Run("timestamp of another signature", func(t *testing.T) {
		kms := readFile(t, "cosign-release-kms.sigstore.json")
		var o map[string]any
		_ = json.Unmarshal(kms, &o)
		b := edit(t, raw, func(m map[string]any) {
			obj(m, "verificationMaterial")["timestampVerificationData"] = obj(o, "verificationMaterial")["timestampVerificationData"]
			delete(obj(m, "verificationMaterial"), "tlogEntries")
		})
		expectErr(t, verify(cosignBlob, b, good), "the timestamp is not over the signature")
	})
	t.Run("tampered timestamp", func(t *testing.T) {
		b := edit(t, raw, func(m map[string]any) {
			ts := obj(m, "verificationMaterial", "timestampVerificationData")["rfc3161Timestamps"].([]any)[0].(map[string]any)
			der, _ := decodeB64(ts["signedTimestamp"].(string))
			der[len(der)-10] ^= 1 // inside the signature
			ts["signedTimestamp"] = encB64(der)
			delete(obj(m, "verificationMaterial"), "tlogEntries")
		})
		expectErr(t, verify(cosignBlob, b, good), "RFC 3161 timestamp")
	})
	t.Run("wrong identity", func(t *testing.T) {
		tr := loadTrust(t, "other", "", "", "trusted_root-public-good.json", "*@example.com", "")
		expectErr(t, verify(cosignBlob, raw, tr), "signed by keyless@projectsigstore.iam.gserviceaccount.com, not *@example.com")
	})
	t.Run("root without the timestamp authority", func(t *testing.T) {
		tr := loadTrust(t, "sigstore", "", "", "trusted_root-public-good.json", cosignIdentity, googleIssuer)
		tr.SigstoreRoot.TSAs = nil
		expectErr(t, verify(cosignBlob, raw, tr), "the trusted root has no timestamp authority")
	})
}

func TestRealCosignKeyBundle(t *testing.T) {
	// cosign's v3 release bundle signed with its KMS release key.
	raw := readFile(t, "cosign-release-kms.sigstore.json")
	release := loadTrust(t, "cosign-release", "cosign-release.pub", "", "", "", "")
	s := store{}
	r := Verify(cosignBlob, bundleManifest(t, s, cosignBlob, raw), s.fetch, []*Trust{release})
	expectOK(t, r, "cosign-release")
	// The bundle's hint is the key's fingerprint.
	if r.Identity != keyFingerprint(release.PublicKeys[0]) || r.Identity != "sha256:adb808ab071482427b7a135b99f640580553757c46454b49d6daf227c19aef8f" {
		t.Fatalf("identity %q", r.Identity)
	}
	r = Verify(cosignBlob, bundleManifest(t, s, cosignBlob, raw), s.fetch, []*Trust{otherKeyTrust(t)})
	expectErr(t, r, "not signed by any of the trusted keys")
	if r.Identity != "sha256:adb808ab071482427b7a135b99f640580553757c46454b49d6daf227c19aef8f" {
		t.Fatalf("a refused bundle names the key of its hint, got %q", r.Identity)
	}
}

func TestRealNotationCOSE(t *testing.T) {
	m := readFile(t, "notation-mcr-cose-manifest.json")
	r := Verify("sha256:3d16d8799f71a78f5bddb3b7cda4a57df1f40ecfa58bac89933010596aa86880", m, nil, nil)
	if r.Format != FormatNotation || r.Kind != KindSignature {
		t.Fatalf("unexpected result %+v", r)
	}
	expectErr(t, r, "COSE Notation signatures are not supported")
}

func TestConcurrentVerify(t *testing.T) {
	// The registry verifies pushes in parallel with shared trusts.
	trusts := []*Trust{
		loadTrust(t, "github", "", "", "trusted_root-public-good.json", uvIdentity, githubIssuer),
		loadTrust(t, "sigstore", "", "", "trusted_root-public-good.json", cosignIdentity, googleIssuer),
	}
	uv, rel := readFile(t, "github-uv-bundle.json"), readFile(t, "cosign-release-keyless.sigstore.json")
	done := make(chan Result)
	for i := 0; i < 8; i++ {
		go func() { done <- VerifyBundle(uvImage, uv, trusts) }()
		go func() { done <- VerifyBundle(cosignBlob, rel, trusts) }()
	}
	for i := 0; i < 16; i++ {
		if r := <-done; !r.OK {
			t.Errorf("%+v", r)
		}
	}
}
