package httpd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mailsvb/go-fs/internal/config"
)

// cosignKey makes a key pair as cosign generate-key-pair does, and writes the
// public half where a trust can name it.
func cosignKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return key, path
}

// pushCosignSignature signs an image as cosign 2 sign --key does and pushes
// the signature under its .sig tag.
func (s *testServer) pushCosignSignature(t *testing.T, repo string, subject digest.Digest, key *ecdsa.PrivateKey) digest.Digest {
	t.Helper()
	payload := []byte(`{"critical":{"identity":{"docker-reference":"localhost/` + repo + `"},"image":{"docker-manifest-digest":"` +
		subject.String() + `"},"type":"cosign container image signature"},"optional":null}`)
	sum := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"architecture":"","os":"","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`)
	configDigest := s.pushBlob(t, repo, config)
	payloadDigest := s.pushBlob(t, repo, payload)
	manifest, _ := json.Marshal(manifestDoc{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageManifest,
		Config:        &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(config))},
		Layers: []v1.Descriptor{{MediaType: mediaTypeCosignSimpleSigning, Digest: payloadDigest, Size: int64(len(payload)),
			Annotations: map[string]string{annotationCosignSignature: base64.StdEncoding.EncodeToString(signature)}}},
	})
	res, body := s.pushManifest(t, repo, fallbackTag(subject, "sig"), v1.MediaTypeImageManifest, manifest)
	expectStatus(t, res, body, http.StatusCreated)
	return digest.FromBytes(manifest)
}

func (s *testServer) imageDetails(t *testing.T, repo, tag string) registryImageJSON {
	t.Helper()
	res, body := pageRequest(t, s, http.MethodGet, "/?go-fs=registry-image&repository="+repo+"&tag="+tag, nil,
		"Accept", "application/json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var details registryImageJSON
	if err := json.Unmarshal([]byte(body), &details); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestRegistryVerifiesSignaturesAgainstItsTrusts(t *testing.T) {
	trusted, trustedFile := cosignKey(t)
	stranger, _ := cosignKey(t)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = []string{"^/"}
		cfg.RegistryTrust = []config.RegistryTrust{{Name: "release", PublicKey: trustedFile,
			Repositories: []string{"acme/*"}}}
	})
	signed := server.pushImage(t, "acme/app", "signed", "linux/amd64", "1")
	server.pushCosignSignature(t, "acme/app", signed.digest, trusted)
	forged := server.pushImage(t, "acme/app", "forged", "linux/amd64", "2")
	server.pushCosignSignature(t, "acme/app", forged.digest, stranger)
	elsewhere := server.pushImage(t, "other", "latest", "linux/amd64", "3")
	server.pushCosignSignature(t, "other", elsewhere.digest, trusted)

	if entry := server.logs.find("registry signature verified"); entry == nil {
		t.Error("the trusted signature was not logged as verified")
	}
	if entry := server.logs.find("registry signature not verified"); entry == nil {
		t.Error("the forged signature was not logged")
	}

	res, body := pageRequest(t, server, http.MethodGet, "/?go-fs=registry", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{
		`<span class="badge artifacts verified" title="verified by release · cosign signature">verified</span>`,
		`<span class="badge artifacts unverified" title="no trusted signer · cosign signature">not verified</span>`,
		// no trust is for the repository other, so its signature is only
		// said to be there
		`<span class="badge artifacts" title="cosign signature">signed</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}

	good := server.imageDetails(t, "acme/app", "signed").Artifacts
	if len(good) != 1 || good[0].Verified != "yes" || good[0].TrustedBy != "release" ||
		!strings.HasPrefix(good[0].Signer, "sha256:") {
		t.Errorf("the trusted signature = %+v", good)
	}
	bad := server.imageDetails(t, "acme/app", "forged").Artifacts
	if len(bad) != 1 || bad[0].Verified != "no" || bad[0].Problem == "" {
		t.Errorf("the forged signature = %+v", bad)
	}
	if other := server.imageDetails(t, "other", "latest").Artifacts; len(other) != 1 || other[0].Verified != "" {
		t.Errorf("the signature no trust is for = %+v", other)
	}
}

func TestGlobPattern(t *testing.T) {
	for pattern, cases := range map[string]map[string]bool{
		"acme/*":   {"acme/app": true, "acme/team/app": true, "acme": false, "other/acme/app": false},
		"app":      {"app": true, "apps": false},
		"a.b/*-ci": {"a.b/x-ci": true, "axb/x-ci": false},
	} {
		for name, want := range cases {
			if got := globPattern(pattern).MatchString(name); got != want {
				t.Errorf("%s matches %s: %v, want %v", pattern, name, got, want)
			}
		}
	}
}
