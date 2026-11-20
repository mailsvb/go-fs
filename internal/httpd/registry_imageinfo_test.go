package httpd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mailsvb/go-fs/internal/config"
)

// pushDescribedImage pushes an image of two layers whose configuration says
// how it runs and how it was built, as docker build writes one, signed in its
// labels under prefix by by.
func (s *testServer) pushDescribedImage(t *testing.T, repo, tag, prefix string, by labelSigner) {
	t.Helper()
	var layers []v1.Descriptor
	var diffIDs []string
	for i := range 2 {
		layer := fmt.Appendf(nil, "layer %d of the described image", i)
		layers = append(layers, v1.Descriptor{MediaType: v1.MediaTypeImageLayerGzip,
			Digest: s.pushBlob(t, repo, layer), Size: int64(len(layer))})
		diffIDs = append(diffIDs, digest.FromBytes(append([]byte("uncompressed "), layer...)).String())
	}
	labels := map[string]string{"maintainer": "osem"}
	for key, value := range by.labels(diffIDs) {
		labels[prefix+strings.TrimPrefix(key, signPrefix)] = value
	}
	configBytes, _ := json.Marshal(map[string]any{
		"os": "linux", "architecture": "amd64", "author": "builder",
		"config": map[string]any{
			"Entrypoint":   []string{"/app/start"},
			"Cmd":          []string{"--serve"},
			"Env":          []string{"PATH=/usr/bin", "MODE=prod"},
			"ExposedPorts": map[string]any{"443/tcp": map[string]any{}, "67/udp": map[string]any{}},
			"Volumes":      map[string]any{"/data": map[string]any{}},
			"WorkingDir":   "/app",
			"User":         "1000",
			"Labels":       labels,
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs},
		"history": []map[string]any{
			{"created": "2026-11-20T01:00:00Z", "created_by": "/bin/sh -c #(nop) ADD file:abc in / "},
			{"created": "2026-11-20T01:01:00Z", "created_by": "/bin/sh -c apk add curl"},
			{"created": "2026-11-20T01:02:00Z", "created_by": "/bin/sh -c #(nop)  EXPOSE 443/tcp", "empty_layer": true},
		},
	})
	configDigest := s.pushBlob(t, repo, configBytes)
	manifest, _ := json.Marshal(manifestDoc{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageManifest,
		Config:        &v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(configBytes))},
		Layers:        layers,
		Annotations:   map[string]string{"org.opencontainers.image.version": "1.2.5127"},
	})
	res, body := s.pushManifest(t, repo, tag, v1.MediaTypeImageManifest, manifest)
	expectStatus(t, res, body, 201)
}

func TestRegistryImageDetailsDescribeTheConfiguration(t *testing.T) {
	by := newLabelSigner(t)
	// labelPrefix written as the whole name of the signature label, as it
	// was configured for osem
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = []string{"^/"}
		cfg.ImageSign = []config.ImageSign{{Name: "osem", LabelPrefix: []string{"org.opencontainers.image.signature"},
			Certificates: string(by.rootPEM), Mode: "enforced"}}
	})
	server.pushDescribedImage(t, "osem", "1.2.5127", "org.opencontainers.image", by)

	details := server.imageDetails(t, "osem", "1.2.5127")
	if len(details.Artifacts) != 1 || details.Artifacts[0].Verified != "yes" || details.Artifacts[0].TrustedBy != "osem" {
		t.Errorf("the signature = %+v", details.Artifacts)
	}
	if len(details.Annotations) != 1 || details.Annotations[0].Value != "1.2.5127" {
		t.Errorf("annotations = %+v", details.Annotations)
	}
	if len(details.Platforms) != 1 || details.Platforms[0].Image == nil {
		t.Fatalf("platforms = %+v", details.Platforms)
	}
	platform := details.Platforms[0]
	image := platform.Image
	if image.OS != "linux" || image.Architecture != "amd64" || image.Author != "builder" ||
		strings.Join(image.Entrypoint, " ") != "/app/start" || strings.Join(image.Cmd, " ") != "--serve" ||
		image.WorkingDir != "/app" || image.User != "1000" {
		t.Errorf("image = %+v", image)
	}
	if strings.Join(image.ExposedPorts, ",") != "443/tcp,67/udp" || strings.Join(image.Volumes, ",") != "/data" ||
		len(image.Env) != 2 || len(image.DiffIDs) != 2 {
		t.Errorf("image = %+v", image)
	}
	marked := map[string]bool{}
	for _, label := range image.Labels {
		marked[label.Key] = label.Imagesign
	}
	if len(image.Labels) != 4 || !marked["org.opencontainers.image.signature"] ||
		!marked["org.opencontainers.image.certchain"] || marked["maintainer"] {
		t.Errorf("labels = %+v", image.Labels)
	}
	if !image.HistoryMatches || len(image.History) != 3 {
		t.Fatalf("history = %+v", image.History)
	}
	steps := image.History
	if steps[0].CreatedBy != "ADD file:abc in /" || steps[0].Layer != platform.Layers[0].Digest ||
		steps[1].CreatedBy != "RUN apk add curl" || steps[1].Layer != platform.Layers[1].Digest ||
		steps[1].Size != platform.Layers[1].Size || !steps[2].EmptyLayer || steps[2].Layer != "" ||
		steps[2].CreatedBy != "EXPOSE 443/tcp" {
		t.Errorf("history = %+v", steps)
	}

	// an entry with a prefix the image carries nothing under names the one
	// it does
	reloadImageSign(t, server, config.ImageSign{Name: "osem", LabelPrefix: []string{"org.example.sign"},
		Certificates: string(by.rootPEM), Mode: "optional"})
	details = server.imageDetails(t, "osem", "1.2.5127")
	if len(details.Artifacts) != 1 || details.Artifacts[0].Signed ||
		!strings.Contains(details.Artifacts[0].Problem, "under org.opencontainers.image, which no labelPrefix lists") {
		t.Errorf("the signature = %+v", details.Artifacts)
	}
}

func TestDescribeConfigHistoryThatDoesNotLineUp(t *testing.T) {
	config := []byte(`{"os":"linux","architecture":"amd64","history":[{"created_by":"/bin/sh -c true"}]}`)
	got, ok := describeConfig(config, []v1.Descriptor{{Digest: "sha256:a"}, {Digest: "sha256:b"}}, nil)
	if !ok || got.HistoryMatches || len(got.History) != 1 || got.History[0].Layer != "" {
		t.Fatalf("history = %+v", got)
	}
	if _, ok := describeConfig([]byte(`{"mediaType":"x"}`), nil, nil); ok {
		t.Fatal("an artifact's configuration is described")
	}
	huge := `{"os":"linux","architecture":"amd64","config":{"Env":[` +
		strings.Repeat(`"A=1",`, maxDetailsEntries+10) + `"B=2"]}}`
	got, _ = describeConfig([]byte(huge), nil, nil)
	if len(got.Env) != maxDetailsEntries || !got.Truncated {
		t.Fatalf("env of %d, truncated %v", len(got.Env), got.Truncated)
	}
}
