package httpd

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// artifact is a signature, attestation or SBOM as one of the tools that make
// them would push it: a manifest with one layer, attached to its image by a
// subject or by a fallback tag.
type artifact struct {
	name         string
	artifactType string
	configType   string
	config       []byte
	layerType    string
	layer        []byte
	layerNotes   map[string]string
	annotations  map[string]string
	// byTag pushes it under sha256-<hex>.<byTag> instead of with a subject
	byTag string
	// kind is what the registry should call it
	kind string
}

// artifactsAsToolsPushThem are the shapes of the signatures and attestations
// of cosign 2 and 3, notation and GitHub's actions/attest-build-provenance.
// The signatures are not real: storing them asks nothing of their content.
// Each layer names its image, so the artifacts of two images differ, as real
// ones do.
var artifactsAsToolsPushThem = []artifact{
	{
		name:       "cosign 2 signature",
		configType: v1.MediaTypeImageConfig,
		config: []byte(`{"architecture":"","created":"0001-01-01T00:00:00Z","history":[{"created":"0001-01-01T00:00:00Z"}],` +
			`"os":"","rootfs":{"type":"layers","diff_ids":["sha256:0000000000000000000000000000000000000000000000000000000000000000"]},"config":{}}`),
		layerType:  mediaTypeCosignSimpleSigning,
		layer:      []byte(`{"critical":{"identity":{"docker-reference":"localhost/app"},"image":{"docker-manifest-digest":"SUBJECT"},"type":"cosign container image signature"},"optional":null}`),
		layerNotes: map[string]string{annotationCosignSignature: "MEUCIQDexample"},
		byTag:      "sig",
		kind:       "cosign signature",
	},
	{
		name:       "cosign 2 attestation",
		configType: v1.MediaTypeImageConfig,
		config:     []byte(`{"architecture":"","os":"","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`),
		layerType:  mediaTypeDSSEEnvelope,
		layer:      []byte(`{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[{"sig":"MEUCIQDexample","keyid":"SUBJECT"}]}`),
		layerNotes: map[string]string{"predicateType": "https://slsa.dev/provenance/v0.2"},
		byTag:      "att",
		kind:       "SLSA provenance",
	},
	{
		name:       "cosign 2 SBOM",
		configType: v1.MediaTypeImageConfig,
		config:     []byte(`{"architecture":"","os":"","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`),
		layerType:  "text/spdx+json",
		layer:      []byte(`{"spdxVersion":"SPDX-2.3","name":"SUBJECT"}`),
		byTag:      "sbom",
		kind:       "SPDX SBOM",
	},
	{
		name:         "cosign 3 signature",
		artifactType: mediaTypeSigstoreBundle,
		layerType:    mediaTypeSigstoreBundle,
		layer:        []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","messageSignature":{}}`),
		annotations: map[string]string{"dev.sigstore.bundle.content": "message-signature",
			annotationBundlePredicate: "https://sigstore.dev/cosign/sign/v1"},
		kind: "Sigstore signature",
	},
	{
		name:         "GitHub build provenance",
		artifactType: mediaTypeSigstoreBundle,
		layerType:    mediaTypeSigstoreBundle,
		layer:        []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","dsseEnvelope":{}}`),
		annotations: map[string]string{"dev.sigstore.bundle.content": "dsse-envelope",
			annotationBundlePredicate: "https://slsa.dev/provenance/v1"},
		kind: "SLSA provenance",
	},
	{
		name:         "notation signature",
		artifactType: artifactTypeNotation,
		layerType:    mediaTypeNotationJWS,
		layer:        []byte(`{"payload":"e30","protected":"e30","signature":"c2ln"}`),
		annotations:  map[string]string{"io.cncf.notary.x509chain.thumbprint#S256": `["abc"]`},
		kind:         "Notation signature",
	},
}

// fallbackTag is the tag cosign 2 pushes an artifact for an image under.
func fallbackTag(subject digest.Digest, suffix string) string {
	return subject.Algorithm().String() + "-" + subject.Encoded() + "." + suffix
}

// pushArtifact pushes an artifact for the image subject and returns its
// manifest's digest.
func (s *testServer) pushArtifact(t *testing.T, repo string, subject image, a artifact) digest.Digest {
	t.Helper()
	config, configType := a.config, a.configType
	if config == nil {
		config, configType = []byte("{}"), v1.MediaTypeEmptyJSON
	}
	configDigest := s.pushBlob(t, repo, config)
	layer := []byte(strings.ReplaceAll(string(a.layer), "SUBJECT", subject.digest.String()))
	layerDigest := s.pushBlob(t, repo, layer)
	doc := manifestDoc{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageManifest,
		ArtifactType:  a.artifactType,
		Config:        &v1.Descriptor{MediaType: configType, Digest: configDigest, Size: int64(len(config))},
		Layers: []v1.Descriptor{{MediaType: a.layerType, Digest: layerDigest, Size: int64(len(layer)),
			Annotations: a.layerNotes}},
		Annotations: a.annotations,
	}
	ref := ""
	if a.byTag != "" {
		ref = fallbackTag(subject.digest, a.byTag)
	} else {
		doc.Subject = &v1.Descriptor{MediaType: subject.mediaType, Digest: subject.digest,
			Size: int64(len(subject.manifest))}
	}
	manifest, _ := json.Marshal(doc)
	d := digest.FromBytes(manifest)
	if ref == "" {
		ref = d.String()
	}
	res, body := s.pushManifest(t, repo, ref, v1.MediaTypeImageManifest, manifest)
	expectStatus(t, res, body, http.StatusCreated)
	if a.byTag == "" && res.Header.Get("OCI-Subject") != subject.digest.String() {
		t.Errorf("%s: OCI-Subject = %q", a.name, res.Header.Get("OCI-Subject"))
	}
	if kind := artifactKindOf(doc); kind != a.kind {
		t.Errorf("%s is taken for %q, want %q", a.name, kind, a.kind)
	}
	return d
}

// pushArtifacts pushes every artifact of artifactsAsToolsPushThem for subject
// and returns their digests by name.
func (s *testServer) pushArtifacts(t *testing.T, repo string, subject image) map[string]digest.Digest {
	t.Helper()
	pushed := map[string]digest.Digest{}
	for _, a := range artifactsAsToolsPushThem {
		pushed[a.name] = s.pushArtifact(t, repo, subject, a)
	}
	return pushed
}

func (s *testServer) referrerDigests(t *testing.T, repo string, subject digest.Digest, artifactType string) []digest.Digest {
	t.Helper()
	path := "/v2/" + repo + "/referrers/" + subject.String()
	if artifactType != "" {
		path += "?artifactType=" + artifactType
	}
	res, body := s.reg(t, http.MethodGet, path, nil)
	expectStatus(t, res, body, http.StatusOK)
	var index v1.Index
	if err := json.Unmarshal(body, &index); err != nil {
		t.Fatal(err)
	}
	var found []digest.Digest
	for _, entry := range index.Manifests {
		found = append(found, entry.Digest)
	}
	slices.Sort(found)
	return found
}

func (s *testServer) tagList(t *testing.T, repo string) []string {
	t.Helper()
	res, body := s.reg(t, http.MethodGet, "/v2/"+repo+"/tags/list", nil)
	expectStatus(t, res, body, http.StatusOK)
	var list struct {
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(body, &list)
	return list.Tags
}

func TestRegistryStoresSignaturesAsToolsPushThem(t *testing.T) {
	server := newRegistryServer(t, nil)
	img := server.pushImage(t, "app", "latest", "linux/amd64", "1")
	pushed := server.pushArtifacts(t, "app", img)

	var referrers []digest.Digest
	for _, a := range artifactsAsToolsPushThem {
		if a.byTag == "" {
			referrers = append(referrers, pushed[a.name])
		}
	}
	slices.Sort(referrers)
	if got := server.referrerDigests(t, "app", img.digest, ""); !slices.Equal(got, referrers) {
		t.Errorf("referrers = %v, want %v", got, referrers)
	}
	for artifactType, names := range map[string][]string{
		mediaTypeSigstoreBundle: {"cosign 3 signature", "GitHub build provenance"},
		artifactTypeNotation:    {"notation signature"},
	} {
		var want []digest.Digest
		for _, name := range names {
			want = append(want, pushed[name])
		}
		slices.Sort(want)
		if got := server.referrerDigests(t, "app", img.digest, artifactType); !slices.Equal(got, want) {
			t.Errorf("referrers of type %s = %v, want %v", artifactType, got, want)
		}
	}

	// the fallback tags are tags like any other, and do not merge into the
	// image's tag: what they name has no platform
	tags := server.tagList(t, "app")
	for _, suffix := range []string{"sig", "att", "sbom"} {
		if !slices.Contains(tags, fallbackTag(img.digest, suffix)) {
			t.Errorf("tag .%s missing from %v", suffix, tags)
		}
	}
	res, _ := server.pullManifest(t, "app", fallbackTag(img.digest, "sig"))
	if got := res.Header.Get("Docker-Content-Digest"); got != pushed["cosign 2 signature"].String() {
		t.Errorf("the .sig tag serves %s", got)
	}
	res, _ = server.pullManifest(t, "app", "latest")
	if got := res.Header.Get("Docker-Content-Digest"); got != img.digest.String() {
		t.Errorf("latest serves %s, not the image", got)
	}
}

func TestFallbackTagSubject(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	cases := map[string]string{
		"sha256-" + hex + ".sig":  "sig",
		"sha256-" + hex + ".att":  "att",
		"sha256-" + hex + ".sbom": "sbom",
		"sha256-" + hex:           "",
	}
	for tag, suffix := range cases {
		d, got, ok := fallbackTagSubject(tag)
		if !ok || got != suffix || d.String() != "sha256:"+hex {
			t.Errorf("%s: %s %q %v", tag, d, got, ok)
		}
	}
	for _, tag := range []string{"latest", "sha256-" + hex + ".txt", "sha256-" + hex[:10] + ".sig",
		"md5-" + hex + ".sig", "sha256-" + strings.ToUpper(hex) + ".sig"} {
		if _, _, ok := fallbackTagSubject(tag); ok {
			t.Errorf("%s taken for a fallback tag", tag)
		}
	}
}

func TestRegistryGarbageCollectionDropsTheSignaturesOfADeletedImage(t *testing.T) {
	server := newRegistryServer(t, nil)
	img := server.pushImage(t, "app", "latest", "linux/amd64", "1")
	pushed := server.pushArtifacts(t, "app", img)
	// a signature of the provenance: it goes once the provenance has gone
	provenance := image{digest: pushed["GitHub build provenance"], mediaType: v1.MediaTypeImageManifest}
	second := server.pushArtifact(t, "app", provenance, artifactsAsToolsPushThem[5])
	kept := server.pushImage(t, "app", "other", "linux/arm64", "2")
	keptSignatures := server.pushArtifacts(t, "app", kept)
	res, body := server.reg(t, http.MethodDelete, "/v2/app/manifests/"+img.digest.String(), nil, asPusher)
	expectStatus(t, res, body, http.StatusAccepted)
	ageRegistry(t, server)

	server.sweepRegistry()

	for name, d := range pushed {
		res, body := server.reg(t, http.MethodGet, "/v2/app/manifests/"+d.String(), nil)
		expectStatus(t, res, body, http.StatusNotFound)
		if server.blobStored(d) {
			t.Errorf("the %s of the deleted image was kept", name)
		}
	}
	if server.blobStored(second) {
		t.Error("the signature of the deleted provenance was kept")
	}
	tags := server.tagList(t, "app")
	for _, suffix := range []string{"sig", "att", "sbom"} {
		if slices.Contains(tags, fallbackTag(img.digest, suffix)) {
			t.Errorf("tag .%s of the deleted image kept: %v", suffix, tags)
		}
		if !slices.Contains(tags, fallbackTag(kept.digest, suffix)) {
			t.Errorf("tag .%s of the kept image removed: %v", suffix, tags)
		}
	}
	if got := server.referrerDigests(t, "app", img.digest, ""); len(got) != 0 {
		t.Errorf("the deleted image still has referrers %v", got)
	}
	for name, d := range keptSignatures {
		if !server.blobStored(d) {
			t.Errorf("the %s of the kept image was collected", name)
		}
	}
	if len(server.referrerDigests(t, "app", kept.digest, "")) != 3 {
		t.Error("the kept image lost referrers")
	}
	if server.logs.find("registry cleanup removed an artifact whose image is gone") == nil ||
		server.logs.find("registry cleanup removed a tag whose image is gone") == nil {
		t.Error("the removals were not recorded")
	}
}

func TestRegistryGarbageCollectionKeepsArtifactsItCannotCallDangling(t *testing.T) {
	server := newRegistryServer(t, nil)
	// an image in one repository, its signature in another, as cosign stores
	// it with COSIGN_REPOSITORY set
	img := server.pushImage(t, "app", "latest", "linux/amd64", "1")
	elsewhere := server.pushArtifact(t, "signatures", img, artifactsAsToolsPushThem[0])
	// an untagged image, which may still be pulled by its digest
	untagged := server.pushImage(t, "app", "temporary", "linux/arm64", "2")
	untaggedSignature := server.pushArtifact(t, "app", untagged, artifactsAsToolsPushThem[3])
	res, body := server.reg(t, http.MethodDelete, "/v2/app/manifests/temporary", nil, asPusher)
	expectStatus(t, res, body, http.StatusAccepted)
	// an artifact for an image that is gone, but tagged: someone wants it
	gone := server.buildImage(t, "app", "linux/amd64", "3", false)
	tagged := server.pushArtifact(t, "app", gone, artifactsAsToolsPushThem[5])
	_, raw := server.reg(t, http.MethodGet, "/v2/app/manifests/"+tagged.String(), nil)
	res, body = server.pushManifest(t, "app", "keep-me", v1.MediaTypeImageManifest, raw)
	expectStatus(t, res, body, http.StatusCreated)
	ageRegistry(t, server)
	// and one whose image has not arrived yet
	early := server.pushArtifact(t, "app", gone, artifactsAsToolsPushThem[4])

	server.sweepRegistry()

	for name, d := range map[string]digest.Digest{"signature in another repository": elsewhere,
		"signature of an untagged image": untaggedSignature, "tagged artifact": tagged,
		"artifact before its image": early} {
		if !server.blobStored(d) {
			t.Errorf("the %s was collected", name)
		}
	}
	if !slices.Contains(server.tagList(t, "signatures"), fallbackTag(img.digest, "sig")) {
		t.Error("the tag of the signature in another repository was removed")
	}
}

func TestRegistryPageShowsTheSignaturesWithTheirImage(t *testing.T) {
	server := newRegistryServer(t, func(cfg *httpConfig) { cfg.PathsWithoutAuth = []string{"^/"} })
	indexDigest := pushMultiArch(t, server, "team/app", "1.0")
	index := image{digest: indexDigest, mediaType: v1.MediaTypeImageIndex,
		manifest: manifestOf(t, server, "team/app", indexDigest)}
	server.pushArtifacts(t, "team/app", index)
	var doc manifestDoc
	_ = json.Unmarshal(index.manifest, &doc)
	child := image{digest: doc.Manifests[0].Digest, mediaType: doc.Manifests[0].MediaType}
	server.pushArtifact(t, "team/app", child, artifactsAsToolsPushThem[5])

	res, body := pageRequest(t, server, http.MethodGet, "/?go-fs=registry", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if got := strings.Count(body, "data-tag="); got != 1 {
		t.Errorf("%d tag rows, want the image's alone: its fallback tags are listed with it", got)
	}
	want := `<span class="badge artifacts" title="Notation signature, SLSA provenance, SPDX SBOM, Sigstore signature, cosign signature">signed</span>`
	if !strings.Contains(body, want) {
		t.Errorf("the page lacks %s", want)
	}

	res, body = pageRequest(t, server, http.MethodGet, "/?go-fs=registry-image&repository=team/app&tag=1.0", nil,
		"Accept", "application/json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var details registryImageJSON
	if err := json.Unmarshal([]byte(body), &details); err != nil {
		t.Fatal(err)
	}
	if len(details.Artifacts) != len(artifactsAsToolsPushThem)+1 {
		t.Fatalf("artifacts = %+v", details.Artifacts)
	}
	forPlatform, tagged := 0, 0
	for _, artifact := range details.Artifacts {
		if artifact.Platform != "" {
			forPlatform++
			if artifact.Subject != child.digest.String() || artifact.Kind != "Notation signature" {
				t.Errorf("platform artifact = %+v", artifact)
			}
		}
		if artifact.Tag != "" {
			tagged++
		}
		if artifact.Kind == "SPDX SBOM" && artifact.Signed {
			t.Errorf("the SBOM cosign attached is taken for signed: %+v", artifact)
		}
		if artifact.Pushed == "" {
			t.Errorf("artifact without its push time: %+v", artifact)
		}
	}
	if forPlatform != 1 || tagged != 3 {
		t.Errorf("%d for a platform, %d by tag, want 1 and 3: %+v", forPlatform, tagged, details.Artifacts)
	}
}
