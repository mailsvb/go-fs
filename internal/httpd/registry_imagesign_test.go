package httpd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mailsvb/go-fs/internal/config"
)

const signPrefix = "org.mitel.imagesign"

// labelSigner is an Ed25519 CA, an intermediate and a signing certificate, as
// the PKCS #12 of image-sign.sh holds them.
type labelSigner struct {
	rootPEM []byte
	chain   []byte // the leaf's PEM and the intermediate's
	key     ed25519.PrivateKey
}

func newLabelSigner(t *testing.T) labelSigner {
	t.Helper()
	serial := int64(1)
	issue := func(cn string, ca bool, pub ed25519.PublicKey, parent *x509.Certificate, parentKey ed25519.PrivateKey) *x509.Certificate {
		serial++
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			IsCA: ca, BasicConstraintsValid: ca,
		}
		if ca {
			tmpl.KeyUsage = x509.KeyUsageCertSign
		}
		if parent == nil {
			parent = tmpl
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	key := func() (ed25519.PublicKey, ed25519.PrivateKey) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return pub, priv
	}
	rootPub, rootKey := key()
	root := issue("oci-signing-ca", true, rootPub, nil, rootKey)
	interPub, interKey := key()
	inter := issue("oci-signing-intermediate", true, interPub, root, rootKey)
	leafPub, leafKey := key()
	leaf := issue("image-signer", false, leafPub, inter, interKey)
	encode := func(c *x509.Certificate) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return labelSigner{rootPEM: encode(root), chain: append(encode(leaf), encode(inter)...), key: leafKey}
}

// labels sign diffIDs as image-sign.sh does.
func (s labelSigner) labels(diffIDs []string) map[string]string {
	sum := sha256.Sum256([]byte(strings.Join(diffIDs, "\n") + "\n"))
	sig := ed25519.Sign(s.key, []byte(hex.EncodeToString(sum[:])))
	return map[string]string{
		signPrefix + ".signature": base64.StdEncoding.EncodeToString(sig),
		signPrefix + ".certchain": base64.StdEncoding.EncodeToString(s.chain),
		signPrefix + ".alg":       "ed25519",
	}
}

// signing is how a test image is signed.
type signing int

const (
	unsigned signing = iota
	signedImage
	tampered // signed for other layers than its own
)

// buildSignedImage uploads the configuration and the layer of an image and
// returns its manifest, not yet pushed, with the labels sign gives it.
func (s *testServer) buildSignedImage(t *testing.T, repo, platform, salt string, by labelSigner, sign signing) image {
	t.Helper()
	layer := fmt.Appendf(nil, "layer of %s %s", platform, salt)
	layerDigest := s.pushBlob(t, repo, layer)
	diffIDs := []string{digest.FromBytes(append([]byte("uncompressed "), layer...)).String()}
	var labels map[string]string
	switch sign {
	case signedImage:
		labels = by.labels(diffIDs)
	case tampered:
		labels = by.labels(append([]string{"sha256:" + strings.Repeat("0", 64)}, diffIDs...))
	}
	parts := strings.Split(platform, "/")
	configBytes, _ := json.Marshal(map[string]any{"os": parts[0], "architecture": parts[1],
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs}, "config": map[string]any{"Labels": labels}})
	configDigest := s.pushBlob(t, repo, configBytes)
	manifest, _ := json.Marshal(manifestDoc{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageManifest,
		Config:        &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(configBytes))},
		Layers:        []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip, Digest: layerDigest, Size: int64(len(layer))}},
	})
	return image{manifest: manifest, mediaType: v1.MediaTypeImageManifest, digest: digest.FromBytes(manifest),
		config: configDigest, layer: layerDigest}
}

func (s *testServer) pushSigned(t *testing.T, repo, ref, platform, salt string, by labelSigner, sign signing) (image, *http.Response, []byte) {
	t.Helper()
	img := s.buildSignedImage(t, repo, platform, salt, by, sign)
	res, body := s.pushManifest(t, repo, ref, img.mediaType, img.manifest)
	return img, res, body
}

func imageSignEntry(by labelSigner, mode string, repositories ...string) config.ImageSign {
	return config.ImageSign{Name: "mitel", LabelPrefix: []string{"com.example.other", signPrefix},
		Certificates: string(by.rootPEM), Mode: mode, Repositories: repositories}
}

// reloadImageSign replaces the imagesign entries of a registry server.
func reloadImageSign(t *testing.T, server *testServer, entries ...config.ImageSign) {
	t.Helper()
	set := server.settings()
	cfg := set.cfg
	cfg.ImageSign = entries
	users := []config.User{fullUser("john", "doe"), registryUser(pusher, pusherPassword)}
	if err := server.Reload(cfg, set.https, set.ssh, users, nil, nil); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

func expectDenied(t *testing.T, what string, res *http.Response, body []byte, mention string) {
	t.Helper()
	if res.StatusCode != http.StatusForbidden || errorCodeOf(t, body) != "DENIED" {
		t.Fatalf("%s answered %d: %s", what, res.StatusCode, body)
	}
	if !strings.Contains(string(body), mention) {
		t.Errorf("%s does not say %q: %s", what, mention, body)
	}
}

func TestImageSignModesOnPush(t *testing.T) {
	by := newLabelSigner(t)
	for _, c := range []struct {
		mode                 string
		missingOK, invalidOK bool
	}{
		{"", true, true},
		{"optional", true, true},
		{"mandatory", true, false},
		{"Enforced", false, false},
	} {
		t.Run("mode "+c.mode, func(t *testing.T) {
			server := newRegistryServer(t, func(cfg *httpConfig) {
				cfg.ImageSign = []config.ImageSign{imageSignEntry(by, c.mode, "prod/*")}
			})
			_, res, body := server.pushSigned(t, "prod/app", "signed", "linux/amd64", "1", by, signedImage)
			expectStatus(t, res, body, http.StatusCreated)
			if server.logs.find("registry image signature verified") == nil {
				t.Error("the signature was not logged as verified")
			}

			_, res, body = server.pushSigned(t, "prod/app", "unsigned", "linux/amd64", "2", by, unsigned)
			if c.missingOK {
				expectStatus(t, res, body, http.StatusCreated)
				if server.logs.find("registry image is not signed") == nil {
					t.Error("the unsigned image was not logged")
				}
			} else {
				expectDenied(t, "an unsigned push", res, body, "the image is not signed for linux/amd64")
			}

			_, res, body = server.pushSigned(t, "prod/app", "tampered", "linux/amd64", "3", by, tampered)
			if c.invalidOK {
				expectStatus(t, res, body, http.StatusCreated)
			} else {
				expectDenied(t, "a tampered push", res, body, "does not match the layers")
			}
			if !c.missingOK && tagDigest(t, server, "prod/app", "unsigned") != "" {
				t.Error("a refused image was tagged")
			}

			// no entry is for another repository
			_, res, body = server.pushSigned(t, "dev/app", "unsigned", "linux/amd64", "4", by, unsigned)
			expectStatus(t, res, body, http.StatusCreated)
		})
	}
}

// With verifyChain off an enforced entry needs no certificates: an image
// signed under a CA nobody configured is taken, as long as its signature
// matches its layers, and one that does not is still refused.
func TestImageSignWithoutChainOnPush(t *testing.T) {
	by := newLabelSigner(t)
	off := false
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.ImageSign = []config.ImageSign{{Name: "mitel", LabelPrefix: []string{signPrefix},
			Mode: config.ImageSignEnforced, VerifyChain: &off}}
	})
	_, res, body := server.pushSigned(t, "prod/app", "signed", "linux/amd64", "1", by, signedImage)
	expectStatus(t, res, body, http.StatusCreated)
	if server.logs.find("registry image signature verified") == nil {
		t.Error("the signature was not logged as verified")
	}
	_, res, body = server.pushSigned(t, "prod/app", "tampered", "linux/amd64", "2", by, tampered)
	expectDenied(t, "a tampered push", res, body, "does not match the layers")
	_, res, body = server.pushSigned(t, "prod/app", "unsigned", "linux/amd64", "3", by, unsigned)
	expectDenied(t, "an unsigned push", res, body, "the image is not signed for linux/amd64")

	// the same entry with the chain verified refuses a signer of another CA
	other := newLabelSigner(t)
	reloadImageSign(t, server, imageSignEntry(other, config.ImageSignEnforced))
	_, res, body = server.pushSigned(t, "prod/app", "chained", "linux/amd64", "4", by, signedImage)
	expectDenied(t, "a push signed under another CA", res, body, "unknown authority")
}

func TestImageSignModesOnPull(t *testing.T) {
	by := newLabelSigner(t)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.ImageSign = []config.ImageSign{imageSignEntry(by, "optional")}
	})
	signed, res, body := server.pushSigned(t, "prod/app", "signed", "linux/amd64", "1", by, signedImage)
	expectStatus(t, res, body, http.StatusCreated)
	plain, res, body := server.pushSigned(t, "prod/app", "unsigned", "linux/amd64", "2", by, unsigned)
	expectStatus(t, res, body, http.StatusCreated)
	forged, res, body := server.pushSigned(t, "prod/app", "tampered", "linux/amd64", "3", by, tampered)
	expectStatus(t, res, body, http.StatusCreated)
	// optional hands out everything
	for _, ref := range []string{"signed", "unsigned", "tampered"} {
		server.pullManifest(t, "prod/app", ref)
	}

	for _, c := range []struct {
		mode               string
		unsigned, tampered int
	}{
		{"mandatory", http.StatusOK, http.StatusForbidden},
		{"enforced", http.StatusForbidden, http.StatusForbidden},
		{"optional", http.StatusOK, http.StatusOK},
	} {
		reloadImageSign(t, server, imageSignEntry(by, c.mode))
		for _, p := range []struct {
			name string
			img  image
			want int
		}{
			{"signed", signed, http.StatusOK},
			{"unsigned", plain, c.unsigned},
			{"tampered", forged, c.tampered},
		} {
			for _, ref := range []string{p.name, p.img.digest.String()} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					res, body := server.reg(t, method, "/v2/prod/app/manifests/"+ref, nil)
					if res.StatusCode != p.want {
						t.Errorf("%s: %s %s answered %d, want %d: %s", c.mode, method, ref, res.StatusCode, p.want, body)
					}
					if res.StatusCode == http.StatusForbidden && method == http.MethodGet {
						expectDenied(t, c.mode+" pull of "+p.name, res, body, "imagesign mitel ("+c.mode+")")
					}
				}
			}
		}
	}
	if server.logs.find("registry pull refused for the image's signature") == nil {
		t.Error("a refused pull was not logged")
	}
}

func TestImageSignChecksEachPlatform(t *testing.T) {
	by := newLabelSigner(t)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.ImageSign = []config.ImageSign{imageSignEntry(by, "optional")}
	})
	// image-sign.sh signs one platform of an image
	amd, res, body := server.pushSigned(t, "prod/app", "1.0", "linux/amd64", "a", by, signedImage)
	expectStatus(t, res, body, http.StatusCreated)
	arm, res, body := server.pushSigned(t, "prod/app", "1.0", "linux/arm64", "b", by, unsigned)
	expectStatus(t, res, body, http.StatusCreated)
	index := tagDigest(t, server, "prod/app", "1.0")
	if index == amd.digest || index == arm.digest {
		t.Fatal("the two platforms were not merged into an index")
	}
	// an artifact is no image, and nothing checks its labels
	subject := server.descriptorOf(t, "prod/app", amd.digest)
	server.pushPlainArtifact(t, "prod/app", "", &subject, sigType, "a signature")

	reloadImageSign(t, server, imageSignEntry(by, "enforced"))
	server.pullManifest(t, "prod/app", "1.0")
	server.pullManifest(t, "prod/app", amd.digest.String())
	res, data := server.reg(t, http.MethodGet, "/v2/prod/app/manifests/"+arm.digest.String(), nil)
	expectDenied(t, "the unsigned platform", res, data, "for linux/arm64")

	// and artifacts are taken under enforced as well
	server.pushPlainArtifact(t, "prod/app", "", &subject, sigType, "another signature")
}

func TestImageSignStrictestModeWins(t *testing.T) {
	by := newLabelSigner(t)
	lax := imageSignEntry(by, "optional")
	strict := imageSignEntry(by, "enforced", "prod/*")
	strict.Name = "release"
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.ImageSign = []config.ImageSign{lax, strict}
	})
	_, res, body := server.pushSigned(t, "prod/app", "unsigned", "linux/amd64", "1", by, unsigned)
	expectDenied(t, "under two entries", res, body, "imagesign mitel, release (enforced)")
	_, res, body = server.pushSigned(t, "dev/app", "unsigned", "linux/amd64", "2", by, unsigned)
	expectStatus(t, res, body, http.StatusCreated)

	// a signature one of the entries verifies is enough
	other := newLabelSigner(t)
	strict.Certificates = string(other.rootPEM)
	reloadImageSign(t, server, lax, strict)
	_, res, body = server.pushSigned(t, "prod/app", "signed", "linux/amd64", "3", by, signedImage)
	expectStatus(t, res, body, http.StatusCreated)
}

func TestImageSignRefusesJobs(t *testing.T) {
	by := newLabelSigner(t)
	local := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.ImageSign = []config.ImageSign{imageSignEntry(by, "enforced", "prod/*")}
		cfg.PathsWithoutAuth = []string{publicExcept("private")}
	})
	ci := login(t, local, "/", pusher, pusherPassword)

	// a pull from another registry, into the repository of its reference
	prodSource := newRegistryServer(t, nil)
	prodSource.pushSigned(t, "prod/app", "1.0", "linux/amd64", "a", by, unsigned)
	job := startTransfer(t, local, "/?go-fs=registry-pull", ci,
		map[string]string{"reference": prodSource.url("/prod/app:1.0")})
	if job.State == jobDone || !strings.Contains(job.Message, "imagesign mitel (enforced)") {
		t.Fatalf("the pull of an unsigned image ended %s: %s", job.State, job.Message)
	}
	if tagDigest(t, local, "prod/app", "1.0") != "" {
		t.Error("the refused pull was tagged")
	}

	// an import of an archive
	data, _ := appArchive(t)
	local.write(t, "images/app.tar", string(data))
	job = startImport(t, local, "/images/?go-fs=registry-import", ci, map[string]string{"file": "app.tar"})
	res, body := confirmImport(t, local, ci, job.ID, map[string]any{"index": 0, "targets": []string{"prod/imported:v1"}})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("confirming answered %d: %s", res.StatusCode, body)
	}
	job = waitForJob(t, local, job.ID, ci)
	if job.State == jobDone || !strings.Contains(job.Message, "imagesign mitel (enforced)") {
		t.Fatalf("the import of an unsigned image ended %s: %s", job.State, job.Message)
	}
	if tagDigest(t, local, "prod/imported", "v1") != "" {
		t.Error("the refused import was tagged")
	}

	// a push to another registry of an image stored before the entry was
	// strict
	reloadImageSign(t, local, imageSignEntry(by, "optional", "prod/*"))
	local.pushSigned(t, "prod/kept", "1.0", "linux/amd64", "k", by, unsigned)
	reloadImageSign(t, local, imageSignEntry(by, "enforced", "prod/*"))
	remote := newRegistryServer(t, nil)
	job = startTransfer(t, local, "/?go-fs=registry-push&repository=prod/kept&tag=1.0", ci,
		map[string]string{"reference": remote.url("/mirror/kept:1.0"), "username": pusher, "password": pusherPassword})
	if job.State == jobDone || !strings.Contains(job.Message, "imagesign mitel (enforced)") {
		t.Fatalf("the push of an unsigned image ended %s: %s", job.State, job.Message)
	}
	if tagDigest(t, remote, "mirror/kept", "1.0") != "" {
		t.Error("the refused push reached the other registry")
	}
}

func TestImageSignOnTheRegistryPage(t *testing.T) {
	by := newLabelSigner(t)
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = []string{"^/"}
		cfg.ImageSign = []config.ImageSign{imageSignEntry(by, "optional", "prod/*")}
	})
	server.pushSigned(t, "prod/app", "signed", "linux/amd64", "1", by, signedImage)
	server.pushSigned(t, "prod/app", "unsigned", "linux/amd64", "2", by, unsigned)
	server.pushSigned(t, "prod/app", "tampered", "linux/amd64", "3", by, tampered)
	server.pushSigned(t, "prod/app", "multi", "linux/amd64", "4", by, signedImage)
	server.pushSigned(t, "prod/app", "multi", "linux/arm64", "5", by, unsigned)
	server.pushSigned(t, "dev/app", "latest", "linux/amd64", "6", by, unsigned)

	res, body := pageRequest(t, server, http.MethodGet, "/?go-fs=registry", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{
		`<span class="badge imagesign signed" title="imagesign mitel (optional) · signed by mitel (image-signer sha256:`,
		`<span class="badge imagesign warning" title="imagesign mitel (optional) · not signed">⚠ unsigned</span>`,
		`title="imagesign mitel (optional) · the signature does not verify: the signature does not match the layers of the image">⚠ bad signature</span>`,
		`title="imagesign mitel (optional) · linux/amd64: signed by mitel`,
		`linux/arm64: not signed">⚠ partly signed</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	// the page's script names the classes too, so the spans are counted
	if got := strings.Count(body, `<span class="badge imagesign`); got != 4 {
		t.Errorf("%d imagesign badges, want 4: dev/app has no entry", got)
	}

	signed := server.imageDetails(t, "prod/app", "signed").Artifacts
	if len(signed) != 1 || !signed[0].InConfig || signed[0].Verified != "yes" || signed[0].TrustedBy != "mitel" ||
		signed[0].Platform != "" || signed[0].Mode != "optional" || signed[0].Refused {
		t.Errorf("the signed image = %+v", signed)
	}
	forged := server.imageDetails(t, "prod/app", "tampered").Artifacts
	if len(forged) != 1 || forged[0].Verified != "no" || !forged[0].Signed || forged[0].Problem == "" {
		t.Errorf("the tampered image = %+v", forged)
	}
	multi := server.imageDetails(t, "prod/app", "multi").Artifacts
	if len(multi) != 2 {
		t.Fatalf("the index = %+v", multi)
	}
	for _, item := range multi {
		switch item.Platform {
		case "linux/amd64":
			if item.Verified != "yes" {
				t.Errorf("amd64 = %+v", item)
			}
		case "linux/arm64":
			if item.Signed || item.Verified != "" || !strings.Contains(item.Problem, "no signature labels") {
				t.Errorf("arm64 = %+v", item)
			}
		default:
			t.Errorf("an item for %q", item.Platform)
		}
	}

	// under enforced the badge says the image is refused
	reloadImageSign(t, server, imageSignEntry(by, "enforced", "prod/*"))
	_, body = pageRequest(t, server, http.MethodGet, "/?go-fs=registry", nil)
	if !strings.Contains(body, `imagesign mitel (enforced) · not signed · the registry refuses to hand it out">⚠ unsigned</span>`) {
		t.Error("the page does not say that an unsigned image is refused")
	}
	if got := server.imageDetails(t, "prod/app", "unsigned").Artifacts; len(got) != 1 || !got[0].Refused {
		t.Errorf("the unsigned image under enforced = %+v", got)
	}
}
