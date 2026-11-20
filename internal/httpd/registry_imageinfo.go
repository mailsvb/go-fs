package httpd

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mailsvb/go-fs/internal/imagesig"
)

// What the details of a tag show of the configuration of each of its images:
// how it runs, its labels and how it was built, as `docker image inspect`
// and the pages of other registries show them. The configuration is read
// when the details are asked for, not for the listing.

// Bounds on what the details carry of a configuration, so that one built to
// be large does not make an answer that is.
const (
	maxDetailsEntries = 500
	maxDetailsHistory = 1000
	maxDetailsString  = 64 << 10
)

type registryLabelJSON struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Imagesign marks a label that holds a signature an imagesign entry of
	// the repository looks for.
	Imagesign bool `json:"imagesign,omitempty"`
}

// registryHistoryJSON is one step of the build of an image. A step that made
// a layer names it, when the steps line up with the layers.
type registryHistoryJSON struct {
	Created    string `json:"created,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	Comment    string `json:"comment,omitempty"`
	EmptyLayer bool   `json:"emptyLayer,omitempty"`
	Layer      string `json:"layer,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

// registryConfigJSON is what the configuration of one image says.
type registryConfigJSON struct {
	OS           string                `json:"os,omitempty"`
	Architecture string                `json:"architecture,omitempty"`
	Variant      string                `json:"variant,omitempty"`
	OSVersion    string                `json:"osVersion,omitempty"`
	Author       string                `json:"author,omitempty"`
	Entrypoint   []string              `json:"entrypoint,omitempty"`
	Cmd          []string              `json:"cmd,omitempty"`
	WorkingDir   string                `json:"workingDir,omitempty"`
	User         string                `json:"user,omitempty"`
	StopSignal   string                `json:"stopSignal,omitempty"`
	Healthcheck  []string              `json:"healthcheck,omitempty"`
	Env          []string              `json:"env,omitempty"`
	ExposedPorts []string              `json:"exposedPorts,omitempty"`
	Volumes      []string              `json:"volumes,omitempty"`
	Labels       []registryLabelJSON   `json:"labels,omitempty"`
	History      []registryHistoryJSON `json:"history,omitempty"`
	// HistoryMatches says that the steps that made a layer are as many as
	// the layers, so each names its own.
	HistoryMatches bool     `json:"historyMatches,omitempty"`
	DiffIDs        []string `json:"diffIds,omitempty"`
	// Truncated says that a list or a value was cut short.
	Truncated bool `json:"truncated,omitempty"`
}

// rawImageConfig is the configuration of an image as OCI and Docker write it.
type rawImageConfig struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
	OSVersion    string `json:"os.version"`
	Author       string `json:"author"`
	Config       struct {
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		StopSignal   string              `json:"StopSignal"`
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Labels       map[string]string   `json:"Labels"`
		Healthcheck  *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	History []struct {
		Created    string `json:"created"`
		CreatedBy  string `json:"created_by"`
		Comment    string `json:"comment"`
		EmptyLayer bool   `json:"empty_layer"`
	} `json:"history"`
}

// describeConfig reads the configuration of an image for its details. layers
// are the layers of its manifest, which the steps of its history are paired
// with, and trusts those of the imagesign entries of its repository. A
// configuration that is not an image's describes nothing.
func describeConfig(data []byte, layers []v1.Descriptor, trusts []*imagesig.LabelTrust) (*registryConfigJSON, bool) {
	var raw rawImageConfig
	if json.Unmarshal(data, &raw) != nil || raw.OS == "" || raw.Architecture == "" {
		return nil, false
	}
	out := &registryConfigJSON{}
	clip := func(s string) string {
		if len(s) > maxDetailsString {
			out.Truncated = true
			return s[:maxDetailsString]
		}
		return s
	}
	clipAll := func(list []string, most int) []string {
		if len(list) > most {
			out.Truncated = true
			list = list[:most]
		}
		clipped := make([]string, 0, len(list))
		for _, s := range list {
			clipped = append(clipped, clip(s))
		}
		return clipped
	}
	out.OS, out.Architecture, out.Variant = clip(raw.OS), clip(raw.Architecture), clip(raw.Variant)
	out.OSVersion, out.Author = clip(raw.OSVersion), clip(raw.Author)
	c := raw.Config
	out.Entrypoint = clipAll(c.Entrypoint, maxDetailsEntries)
	out.Cmd = clipAll(c.Cmd, maxDetailsEntries)
	out.WorkingDir, out.User, out.StopSignal = clip(c.WorkingDir), clip(c.User), clip(c.StopSignal)
	if c.Healthcheck != nil {
		out.Healthcheck = clipAll(c.Healthcheck.Test, maxDetailsEntries)
	}
	out.Env = clipAll(c.Env, maxDetailsEntries)
	out.ExposedPorts = clipAll(slices.Sorted(maps.Keys(c.ExposedPorts)), maxDetailsEntries)
	out.Volumes = clipAll(slices.Sorted(maps.Keys(c.Volumes)), maxDetailsEntries)
	out.DiffIDs = clipAll(raw.RootFS.DiffIDs, maxDetailsHistory)

	signature := imagesig.SignatureLabels(c.Labels, trusts)
	keys := slices.Sorted(maps.Keys(c.Labels))
	if len(keys) > maxDetailsEntries {
		out.Truncated = true
		keys = keys[:maxDetailsEntries]
	}
	for _, key := range keys {
		out.Labels = append(out.Labels, registryLabelJSON{Key: clip(key), Value: clip(c.Labels[key]), Imagesign: signature[key]})
	}

	history := raw.History
	if len(history) > maxDetailsHistory {
		out.Truncated = true
		history = history[:maxDetailsHistory]
	}
	made := 0
	for _, step := range raw.History {
		if !step.EmptyLayer {
			made++
		}
	}
	out.HistoryMatches = len(raw.History) > 0 && made == len(layers) && len(history) == len(raw.History)
	next := 0
	for _, step := range history {
		entry := registryHistoryJSON{CreatedBy: clip(buildStep(step.CreatedBy)), Comment: clip(step.Comment),
			EmptyLayer: step.EmptyLayer}
		if created, err := time.Parse(time.RFC3339Nano, step.Created); err == nil {
			entry.Created = created.UTC().Format(time.RFC3339)
		}
		if out.HistoryMatches && !step.EmptyLayer {
			entry.Layer, entry.Size = layers[next].Digest.String(), layers[next].Size
			next++
		}
		out.History = append(out.History, entry)
	}
	return out, true
}

// buildStep is the command of a step of the history as a Dockerfile says it:
// docker build writes a metadata step as /bin/sh -c #(nop) LABEL …, and a
// RUN as /bin/sh -c ….
func buildStep(createdBy string) string {
	step := strings.TrimSpace(createdBy)
	if rest, ok := strings.CutPrefix(step, "/bin/sh -c #(nop) "); ok {
		return strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(step, "/bin/sh -c "); ok {
		return "RUN " + strings.TrimSpace(rest)
	}
	return step
}

// annotationsJSON lists the annotations of a manifest, sorted by key.
func annotationsJSON(annotations map[string]string) []registryLabelJSON {
	if len(annotations) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(annotations))
	if len(keys) > maxDetailsEntries {
		keys = keys[:maxDetailsEntries]
	}
	found := make([]registryLabelJSON, 0, len(keys))
	for _, key := range keys {
		value := annotations[key]
		if len(value) > maxDetailsString {
			value = value[:maxDetailsString]
		}
		found = append(found, registryLabelJSON{Key: key, Value: value})
	}
	return found
}
