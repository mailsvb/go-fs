package imagesig

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// simpleSigning is cosign's legacy signature payload.
type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// verifyCosign checks the layers of a legacy cosign .sig or .att manifest,
// each a signature of its own; the first that verifies wins.
func verifyCosign(subject digest.Digest, m *ocispec.Manifest, fetch Fetch, trusts []*Trust) Result {
	var first *Result
	for _, l := range m.Layers {
		var r Result
		switch l.MediaType {
		case mediaTypeSimpleSigning:
			r = verifyCosignSig(subject, l, fetch, trusts)
		case mediaTypeDSSE:
			r = verifyCosignAtt(subject, l, fetch, trusts)
		default:
			continue
		}
		if r.OK {
			return r
		}
		if first == nil {
			first = &r
		}
	}
	return *first // Verify only comes here with at least one such layer
}

func verifyCosignSig(subject digest.Digest, l ocispec.Descriptor, fetch Fetch, trusts []*Trust) Result {
	r := Result{Format: FormatCosign, Kind: KindSignature}
	payload, err := fetchLayer(fetch, l)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	var p simpleSigning
	if err := json.Unmarshal(payload, &p); err != nil {
		r.Err = "cosign payload: " + err.Error()
		return r
	}
	if got := p.Critical.Image.DockerManifestDigest; got != subject.String() {
		r.Err = fmt.Sprintf("the signature is for %s, not %s", orNone(got), subject)
		return r
	}
	sigB64, ok := l.Annotations[annotationCosignSignature]
	if !ok || sigB64 == "" {
		r.Err = "the layer carries no " + annotationCosignSignature + " annotation"
		return r
	}
	sig, err := decodeB64(sigB64)
	if err != nil {
		r.Err = "cosign signature: " + err.Error()
		return r
	}
	te := trustErrors{what: "public keys"}
	for _, t := range trusts {
		if len(t.PublicKeys) == 0 {
			continue
		}
		var last error = errBadSignature
		for _, k := range t.PublicKeys {
			if last = verifyMessage(k, payload, sig); last == nil {
				r.OK, r.TrustedBy, r.Identity = true, t.Name, keyFingerprint(k)
				return r
			}
		}
		te.add(t.Name, wrongKey(last))
	}
	r.Err = te.String()
	if _, keyless := l.Annotations[annotationCosignCertificate]; keyless && len(te.errs) > 0 {
		r.Err += " (keyless legacy cosign signatures are not supported)"
	}
	return r
}

func verifyCosignAtt(subject digest.Digest, l ocispec.Descriptor, fetch Fetch, trusts []*Trust) Result {
	r := Result{Format: FormatCosign, Kind: KindAttestation, PredicateType: l.Annotations["predicateType"]}
	b, err := fetchLayer(fetch, l)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		r.Err = "DSSE envelope: " + err.Error()
		return r
	}
	d, err := env.decode()
	if err != nil {
		r.Err = err.Error()
		return r
	}
	pt, err := checkStatement(d.payloadType, d.payload, subject)
	if pt != "" {
		r.PredicateType = pt
	}
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Kind = kindOf(pt)
	te := trustErrors{what: "public keys"}
	for _, t := range trusts {
		if len(t.PublicKeys) == 0 {
			continue
		}
		var last error = errBadSignature
		for _, k := range t.PublicKeys {
			if _, last = d.verify(k); last == nil {
				r.OK, r.TrustedBy, r.Identity = true, t.Name, keyFingerprint(k)
				return r
			}
		}
		te.add(t.Name, wrongKey(last))
	}
	r.Err = te.String()
	return r
}

// wrongKey words a signature that none of a trust's keys verify.
func wrongKey(err error) error {
	if errors.Is(err, errBadSignature) {
		return errors.New("not signed by any of the trusted keys")
	}
	return err
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no digest"
	}
	return s
}
