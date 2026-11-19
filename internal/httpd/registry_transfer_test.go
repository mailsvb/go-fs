package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// The signatures, attestations and SBOMs of an image travel with it: a pull
// brings what the source lists as referrers, or keeps under cosign's tags,
// and a push sends them on, with the tags only where the target has no
// referrers API.

const (
	sigType  = "application/vnd.dev.cosign.artifact.sig.v1+json"
	sbomType = "application/spdx+json"
)

// descriptorOf is how a manifest of a test server is named as a subject.
func (s *testServer) descriptorOf(t *testing.T, repo string, d digest.Digest) v1.Descriptor {
	t.Helper()
	res, body := s.reg(t, http.MethodGet, "/v2/"+repo+"/manifests/"+d.String(), nil)
	expectStatus(t, res, body, http.StatusOK)
	return v1.Descriptor{MediaType: res.Header.Get("Content-Type"), Digest: d, Size: int64(len(body))}
}

// pushArtifact pushes an artifact of one blob, under ref, or under its digest
// for "", attached to subject where there is one.
func (s *testServer) pushArtifact(t *testing.T, repo, ref string, subject *v1.Descriptor, artifactType, salt string) v1.Descriptor {
	t.Helper()
	empty := []byte("{}")
	s.pushBlob(t, repo, empty)
	content := []byte("artifact " + salt)
	layer := s.pushBlob(t, repo, content)
	manifest, _ := json.Marshal(manifestDoc{SchemaVersion: 2, MediaType: v1.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       &v1.Descriptor{MediaType: v1.MediaTypeEmptyJSON, Digest: digest.FromBytes(empty), Size: 2},
		Layers:       []v1.Descriptor{{MediaType: artifactType, Digest: layer, Size: int64(len(content))}},
		Subject:      subject})
	d := digest.FromBytes(manifest)
	if ref == "" {
		ref = d.String()
	}
	res, body := s.pushManifest(t, repo, ref, v1.MediaTypeImageManifest, manifest)
	expectStatus(t, res, body, http.StatusCreated)
	return v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: d, Size: int64(len(manifest)),
		ArtifactType: artifactType}
}

// referrersIn are what a repository of a test server records as referring
// to subject.
func referrersIn(t *testing.T, server *testServer, repo string, subject digest.Digest) []digest.Digest {
	t.Helper()
	found, err := server.settings().registry.referrers(repo, subject)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// hasManifest reports whether a repository of a test server holds a manifest.
func hasManifest(server *testServer, repo string, d digest.Digest) bool {
	_, err := server.settings().registry.readRevision(repo, d)
	return err == nil
}

// withoutReferrersAPI puts a registry behind a proxy that answers 404 for
// the referrers API, and leaves OCI-Subject out of what it answers a
// manifest's PUT with, as a registry without the API does. requests counts
// what was asked of the API.
func withoutReferrersAPI(t *testing.T, server *testServer) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	target, _ := url.Parse(server.url("/"))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(res *http.Response) error {
		res.Header.Del("OCI-Subject")
		return nil
	}
	requests := &atomic.Int64{}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/referrers/") {
			requests.Add(1)
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front, requests
}

func TestRegistryPullsTheReferrersOfAnImage(t *testing.T) {
	source := newRegistryServer(t, nil)
	amd := source.pushImage(t, "team/app", "1.0", "linux/amd64", "a")
	source.pushImage(t, "team/app", "1.0", "linux/arm64", "b")
	index := source.descriptorOf(t, "team/app", tagDigest(t, source, "team/app", "1.0"))
	child := source.descriptorOf(t, "team/app", amd.digest)

	sig := source.pushArtifact(t, "team/app", "", &index, sigType, "signature of the index")
	sbom := source.pushArtifact(t, "team/app", "", &child, sbomType, "sbom of amd64")
	sbomSig := source.pushArtifact(t, "team/app", "", &sbom, sigType, "signature of the sbom")
	tooDeep := source.pushArtifact(t, "team/app", "", &sbomSig, sigType, "a signature three levels down")
	// cosign's own, beside the index, with no subject
	attestation := source.pushArtifact(t, "team/app", "sha256-"+index.Digest.Encoded()+".att", nil,
		"application/vnd.dsse.envelope.v1+json", "attestation by tag")

	local := newRegistryServer(t, nil)
	ci := login(t, local, "/", pusher, pusherPassword)
	job := startTransfer(t, local, "/?go-fs=registry-pull", ci,
		map[string]string{"reference": source.url("/team/app:1.0")})
	if job.State != jobDone {
		t.Fatalf("the pull ended %s: %s", job.State, job.Message)
	}
	if !strings.HasSuffix(job.Message, ", with its 4 signatures and attestations.") {
		t.Errorf("the pull says %q", job.Message)
	}
	// the four blobs of the images, the empty configuration and a blob each
	if job.BlobsTotal != 9 || job.BlobsDone != 9 || job.BytesDone != job.BytesTotal {
		t.Errorf("progress of the pull: %+v", job)
	}
	if got := referrersIn(t, local, "team/app", index.Digest); !slices.Equal(got, []digest.Digest{sig.Digest}) {
		t.Errorf("the referrers of the index are %v, want %s", got, sig.Digest)
	}
	if got := referrersIn(t, local, "team/app", amd.digest); !slices.Equal(got, []digest.Digest{sbom.Digest}) {
		t.Errorf("the referrers of the amd64 image are %v, want %s", got, sbom.Digest)
	}
	if got := referrersIn(t, local, "team/app", sbom.Digest); !slices.Equal(got, []digest.Digest{sbomSig.Digest}) {
		t.Errorf("the referrers of the sbom are %v, want %s", got, sbomSig.Digest)
	}
	if hasManifest(local, "team/app", tooDeep.Digest) {
		t.Error("a referrer three levels down was pulled")
	}
	if got := tagDigest(t, local, "team/app", "sha256-"+index.Digest.Encoded()+".att"); got != attestation.Digest {
		t.Errorf("cosign's tag names %q, want %s", got, attestation.Digest)
	}
	// what was pulled is served as the source served it
	res, body := local.reg(t, http.MethodGet, "/v2/team/app/referrers/"+index.Digest.String(), nil)
	expectStatus(t, res, body, http.StatusOK)
	var listed manifestDoc
	_ = json.Unmarshal(body, &listed)
	if len(listed.Manifests) != 1 || listed.Manifests[0].ArtifactType != sigType {
		t.Errorf("the referrers API lists %s", body)
	}

	// one platform: the index is not the one signed, and the sbom is of the
	// other platform
	fresh := newRegistryServer(t, nil)
	ci = login(t, fresh, "/", pusher, pusherPassword)
	job = startTransfer(t, fresh, "/?go-fs=registry-pull", ci,
		map[string]string{"reference": source.url("/team/app:1.0"), "platforms": "linux/arm64"})
	if job.State != jobDone || job.Message != "Pulled team/app:1.0 for linux/arm64." {
		t.Fatalf("the pull ended %s: %s", job.State, job.Message)
	}
	if hasManifest(fresh, "team/app", sig.Digest) || hasManifest(fresh, "team/app", sbom.Digest) {
		t.Error("a referrer of what was left out was pulled")
	}
}

// A source without the referrers API: what it lists in the index under
// sha256-<hex> is pulled, and so is what is under cosign's tags, which stay.
func TestRegistryPullsReferrersByTagWithoutTheAPI(t *testing.T) {
	source := newRegistryServer(t, nil)
	img := source.pushImage(t, "tool", "latest", "linux/amd64", "t")
	subject := source.descriptorOf(t, "tool", img.digest)
	// cosign's signature under its tag, which names the image as its subject
	sigTag := "sha256-" + img.digest.Encoded() + ".sig"
	sig := source.pushArtifact(t, "tool", sigTag, &subject, sigType, "tagged signature")
	// notation's, which a client of a registry without the API lists in the
	// index under sha256-<hex>
	notation := source.pushArtifact(t, "tool", "", &subject, "application/vnd.cncf.notary.signature", "notation")
	fallback, _ := json.Marshal(manifestDoc{SchemaVersion: 2, MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{notation}})
	res, body := source.pushManifest(t, "tool", "sha256-"+img.digest.Encoded(), v1.MediaTypeImageIndex, fallback)
	expectStatus(t, res, body, http.StatusCreated)
	front, asked := withoutReferrersAPI(t, source)

	local := newRegistryServer(t, nil)
	ci := login(t, local, "/", pusher, pusherPassword)
	job := startTransfer(t, local, "/?go-fs=registry-pull", ci, map[string]string{"reference": front.URL + "/tool"})
	if job.State != jobDone {
		t.Fatalf("the pull ended %s: %s", job.State, job.Message)
	}
	if asked.Load() == 0 {
		t.Error("the referrers API was not asked")
	}
	if got := referrersIn(t, local, "tool", img.digest); !slices.Equal(got, sortedDigests(sig.Digest, notation.Digest)) {
		t.Errorf("the referrers of the image are %v, want %s and %s", got, sig.Digest, notation.Digest)
	}
	if got := tagDigest(t, local, "tool", sigTag); got != sig.Digest {
		t.Errorf("cosign's tag names %q, want %s", got, sig.Digest)
	}
	// this registry has the API, so the index of the fallback is not needed
	if got := tagDigest(t, local, "tool", "sha256-"+img.digest.Encoded()); got != "" {
		t.Errorf("the fallback index was tagged: %s", got)
	}
	if !strings.HasSuffix(job.Message, ", with its 2 signatures and attestations.") {
		t.Errorf("the pull says %q", job.Message)
	}
}

func sortedDigests(list ...digest.Digest) []digest.Digest {
	slices.Sort(list)
	return list
}

// A source with nothing attached to the image, that fails the referrers API
// rather than answering it, is pulled from as before.
func TestRegistryPullsFromASourceWithNoReferrers(t *testing.T) {
	config := []byte(`{"os":"linux","architecture":"amd64","rootfs":{"type":"layers"}}`)
	layer := []byte("the layer")
	manifest, _ := json.Marshal(manifestDoc{SchemaVersion: 2, MediaType: v1.MediaTypeImageManifest,
		Config: &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(config), Size: int64(len(config))},
		Layers: []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip, Digest: digest.FromBytes(layer), Size: int64(len(layer))}}})
	var referrers, tags atomic.Int64
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case path == "/v2/":
		case path == "/v2/some/app/manifests/v1":
			w.Header().Set("Content-Type", v1.MediaTypeImageManifest)
			_, _ = w.Write(manifest)
		case path == "/v2/some/app/blobs/"+digest.FromBytes(config).String():
			_, _ = w.Write(config)
		case path == "/v2/some/app/blobs/"+digest.FromBytes(layer).String():
			_, _ = w.Write(layer)
		case strings.HasPrefix(path, "/v2/some/app/referrers/"):
			referrers.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(path, "/v2/some/app/manifests/sha256-"):
			tags.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	local := newRegistryServer(t, nil)
	ci := login(t, local, "/", pusher, pusherPassword)
	job := startTransfer(t, local, "/?go-fs=registry-pull", ci, map[string]string{"reference": registry.URL + "/some/app:v1"})
	if job.State != jobDone || job.Message != "Pulled some/app:v1 for linux/amd64." {
		t.Fatalf("the pull ended %s: %s", job.State, job.Message)
	}
	if job.BlobsTotal != 2 || job.BlobsDone != 2 {
		t.Errorf("progress of the pull: %+v", job)
	}
	if referrers.Load() != 1 || tags.Load() != 4 {
		t.Errorf("asked the referrers API %d times and for %d tags", referrers.Load(), tags.Load())
	}
}

func TestRegistryPushesTheReferrersOfAnImage(t *testing.T) {
	local := newRegistryServer(t, nil)
	img := local.pushImage(t, "team/app", "1.0", "linux/amd64", "p")
	subject := local.descriptorOf(t, "team/app", img.digest)
	sigTag := "sha256-" + img.digest.Encoded() + ".sig"
	attTag := "sha256-" + img.digest.Encoded() + ".att"
	// a signature that is a referrer and under cosign's tag as well
	sig := local.pushArtifact(t, "team/app", sigTag, &subject, sigType, "signature")
	// an SBOM that is only a referrer, and a signature of it
	sbom := local.pushArtifact(t, "team/app", "", &subject, sbomType, "sbom")
	sbomSig := local.pushArtifact(t, "team/app", "", &sbom, sigType, "signature of the sbom")
	// an attestation that is only under cosign's tag
	att := local.pushArtifact(t, "team/app", attTag, nil, "application/vnd.dsse.envelope.v1+json", "attestation")
	ci := login(t, local, "/", pusher, pusherPassword)
	login := func(reference string) map[string]string {
		return map[string]string{"reference": reference, "username": pusher, "password": pusherPassword}
	}

	// a target with the API takes the referrers as they are, and only the
	// tag that is the one link to its manifest
	remote := newRegistryServer(t, nil)
	job := startTransfer(t, local, "/?go-fs=registry-push&repository=team/app&tag=1.0", ci,
		login(remote.url("/mirror/app:1.0")))
	if job.State != jobDone || !strings.HasSuffix(job.Message, ", with its 4 signatures and attestations.") {
		t.Fatalf("the push ended %s: %s", job.State, job.Message)
	}
	if job.BlobsTotal != 7 || job.BlobsDone != 7 {
		t.Errorf("progress of the push: %+v", job)
	}
	if got := referrersIn(t, remote, "mirror/app", img.digest); !slices.Equal(got, sortedDigests(sig.Digest, sbom.Digest)) {
		t.Errorf("the referrers of the image are %v", got)
	}
	if got := referrersIn(t, remote, "mirror/app", sbom.Digest); !slices.Equal(got, []digest.Digest{sbomSig.Digest}) {
		t.Errorf("the referrers of the sbom are %v", got)
	}
	if got := tagDigest(t, remote, "mirror/app", attTag); got != att.Digest {
		t.Errorf("the attestation's tag names %q", got)
	}
	if got := tagDigest(t, remote, "mirror/app", sigTag); got != "" {
		t.Errorf("the signature's tag went to a target with the API: %s", got)
	}

	// a target without it gets the tags, and the index under sha256-<hex>
	// for what has none
	plain := newRegistryServer(t, nil)
	front, _ := withoutReferrersAPI(t, plain)
	job = startTransfer(t, local, "/?go-fs=registry-push&repository=team/app&tag=1.0", ci,
		login(front.URL+"/mirror/app:1.0"))
	if job.State != jobDone {
		t.Fatalf("the push ended %s: %s", job.State, job.Message)
	}
	if got := tagDigest(t, plain, "mirror/app", sigTag); got != sig.Digest {
		t.Errorf("the signature's tag names %q", got)
	}
	if got := tagDigest(t, plain, "mirror/app", attTag); got != att.Digest {
		t.Errorf("the attestation's tag names %q", got)
	}
	for subject, want := range map[digest.Digest]digest.Digest{img.digest: sbom.Digest, sbom.Digest: sbomSig.Digest} {
		_, index := plain.pullManifest(t, "mirror/app", "sha256-"+subject.Encoded())
		if len(index.Manifests) != 1 || index.Manifests[0].Digest != want {
			t.Errorf("the fallback index of %s lists %+v, want %s", subject, index.Manifests, want)
		}
	}

	// pushing again adds nothing to the index
	job = startTransfer(t, local, "/?go-fs=registry-push&repository=team/app&tag=1.0", ci,
		login(front.URL+"/mirror/app:1.0"))
	if job.State != jobDone {
		t.Fatalf("the second push ended %s: %s", job.State, job.Message)
	}
	if _, index := plain.pullManifest(t, "mirror/app", "sha256-"+img.digest.Encoded()); len(index.Manifests) != 1 {
		t.Errorf("the fallback index lists %+v", index.Manifests)
	}
}

func TestParseFallbackTag(t *testing.T) {
	d := digest.FromString("x")
	for tag, want := range map[string]digest.Digest{
		"sha256-" + d.Encoded() + ".sig":  d,
		"sha256-" + d.Encoded() + ".att":  d,
		"sha256-" + d.Encoded() + ".sbom": d,
		"sha256-" + d.Encoded():           "",
		"sha256-abc.sig":                  "",
		"v1.sig":                          "",
	} {
		got, ok := parseFallbackTag(tag)
		if got != want || ok != (want != "") {
			t.Errorf("parseFallbackTag(%q) = %s, %v", tag, got, ok)
		}
	}
}
