package imagesig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pbInt is a protobuf-JSON int64, which is written as a string but may come
// as a number.
type pbInt int64

func (p *pbInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*p = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*p = pbInt(n)
	return err
}

// tlogEntry is a Rekor transparency log entry as a Sigstore bundle carries
// it.
type tlogEntry struct {
	LogIndex pbInt `json:"logIndex"`
	LogID    struct {
		KeyID string `json:"keyId"`
	} `json:"logId"`
	KindVersion struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
	} `json:"kindVersion"`
	IntegratedTime   pbInt `json:"integratedTime"`
	InclusionPromise *struct {
		SignedEntryTimestamp string `json:"signedEntryTimestamp"`
	} `json:"inclusionPromise"`
	InclusionProof *struct {
		LogIndex   pbInt    `json:"logIndex"`
		RootHash   string   `json:"rootHash"`
		TreeSize   pbInt    `json:"treeSize"`
		Hashes     []string `json:"hashes"`
		Checkpoint struct {
			Envelope string `json:"envelope"`
		} `json:"checkpoint"`
	} `json:"inclusionProof"`
	CanonicalizedBody string `json:"canonicalizedBody"`
}

// verifyTlogEntry checks that the entry is in a log of the trusted root,
// that its body is the signature at hand (match), and that the log
// vouches for it: with a signed entry timestamp (the inclusion promise),
// which also makes the integrated time a verified signing time, and with an
// inclusion proof up to a checkpoint the log signed. It returns the
// verified time, or the zero time when only the inclusion proof is there.
func verifyTlogEntry(e *tlogEntry, root *TrustedRoot, match func(kind, version string, body []byte) error) (time.Time, error) {
	body, err := base64.StdEncoding.DecodeString(e.CanonicalizedBody)
	if err != nil || len(body) == 0 {
		return time.Time{}, errors.New("tlog entry has no canonicalized body")
	}
	id, err := decodeB64(e.LogID.KeyID)
	if err != nil {
		return time.Time{}, fmt.Errorf("tlog entry log ID: %w", err)
	}
	log := root.tlog(id)
	if log == nil {
		return time.Time{}, fmt.Errorf("tlog entry is from log %x, which the trusted root does not have", id)
	}
	if err := match(e.KindVersion.Kind, e.KindVersion.Version, body); err != nil {
		return time.Time{}, fmt.Errorf("tlog entry: %w", err)
	}
	if e.InclusionPromise == nil && e.InclusionProof == nil {
		return time.Time{}, errors.New("tlog entry has neither an inclusion promise nor an inclusion proof")
	}
	var verified time.Time
	if e.InclusionPromise != nil {
		set, err := decodeB64(e.InclusionPromise.SignedEntryTimestamp)
		if err != nil {
			return time.Time{}, fmt.Errorf("signed entry timestamp: %w", err)
		}
		payload, _ := json.Marshal(map[string]any{
			"body":           base64.StdEncoding.EncodeToString(body),
			"integratedTime": int64(e.IntegratedTime),
			"logID":          hex.EncodeToString(id),
			"logIndex":       int64(e.LogIndex),
		})
		if err := verifyMessage(log.Key, payload, set); err != nil {
			return time.Time{}, fmt.Errorf("signed entry timestamp of %s: %w", log.BaseURL, err)
		}
		verified = time.Unix(int64(e.IntegratedTime), 0).UTC()
		if !log.validAt(verified) {
			return time.Time{}, fmt.Errorf("the key of %s was not in service at %s", log.BaseURL, verified.Format(time.RFC3339))
		}
	}
	if p := e.InclusionProof; p != nil {
		if err := verifyInclusion(body, int64(p.LogIndex), int64(p.TreeSize), p.RootHash, p.Hashes); err != nil {
			return time.Time{}, err
		}
		if err := verifyCheckpoint(p.Checkpoint.Envelope, log, int64(p.TreeSize), p.RootHash); err != nil {
			return time.Time{}, err
		}
	}
	return verified, nil
}

// verifyInclusion checks an RFC 9162 Merkle audit path from the entry's
// leaf to rootHash.
func verifyInclusion(body []byte, index, size int64, rootHashB64 string, hashesB64 []string) error {
	root, err := decodeB64(rootHashB64)
	if err != nil {
		return fmt.Errorf("inclusion proof root hash: %w", err)
	}
	if index < 0 || size <= 0 || index >= size {
		return fmt.Errorf("inclusion proof index %d is outside a tree of %d", index, size)
	}
	r := hashLeaf(body)
	fn, sn := uint64(index), uint64(size-1)
	for _, hb := range hashesB64 {
		p, err := decodeB64(hb)
		if err != nil || len(p) != sha256.Size {
			return errors.New("inclusion proof has a malformed hash")
		}
		if sn == 0 {
			return errors.New("inclusion proof is too long")
		}
		if fn&1 == 1 || fn == sn {
			r = hashChildren(p, r)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			r = hashChildren(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return errors.New("inclusion proof is too short")
	}
	if !bytes.Equal(r, root) {
		return errors.New("inclusion proof does not lead to the root hash")
	}
	return nil
}

func hashLeaf(b []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil)
}

func hashChildren(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

// verifyCheckpoint checks a signed note (C2SP signed-note, as Rekor writes
// its checkpoints) against the log's key and that it commits to the tree
// the inclusion proof ends in.
func verifyCheckpoint(env string, log *TransparencyLog, size int64, rootHashB64 string) error {
	if env == "" {
		return errors.New("inclusion proof has no checkpoint")
	}
	i := strings.Index(env, "\n\n")
	if i < 0 {
		return errors.New("checkpoint is not a signed note")
	}
	text, sigs := env[:i+1], env[i+2:]
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 3 {
		return errors.New("checkpoint is too short")
	}
	if n, err := strconv.ParseInt(lines[1], 10, 64); err != nil || n != size {
		return fmt.Errorf("checkpoint is for a tree of %s, the proof for %d", lines[1], size)
	}
	want, err := decodeB64(rootHashB64)
	if err != nil {
		return err
	}
	if got, err := decodeB64(lines[2]); err != nil || !bytes.Equal(got, want) {
		return errors.New("checkpoint commits to another root hash than the inclusion proof")
	}
	for _, line := range strings.Split(sigs, "\n") {
		// "— <name> <base64(key hint || signature)>"
		if !strings.HasPrefix(line, "— ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(f[2])
		if err != nil || len(raw) < 5 {
			continue
		}
		sig := raw[4:]
		var ok bool
		if k, isEd := log.Key.(ed25519.PublicKey); isEd {
			ok = ed25519.Verify(k, []byte(text), sig)
		} else {
			ok = verifyMessage(log.Key, []byte(text), sig) == nil
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("checkpoint is not signed by %s", log.BaseURL)
}

// rekorBody is what Verify reads of a Rekor entry body, across the kinds
// cosign and GitHub write: hashedrekord and dsse 0.0.1 (Rekor v1), their
// 0.0.2 versions (Rekor v2) and intoto 0.0.2.
type rekorBody struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Spec       struct {
		// hashedrekord 0.0.1
		Data struct {
			Hash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"hash"`
		} `json:"data"`
		Signature struct {
			Content   string `json:"content"`
			PublicKey struct {
				Content string `json:"content"`
			} `json:"publicKey"`
		} `json:"signature"`
		// dsse 0.0.1
		PayloadHash struct {
			Algorithm string `json:"algorithm"`
			Value     string `json:"value"`
		} `json:"payloadHash"`
		Signatures []struct {
			Signature string `json:"signature"`
			Verifier  string `json:"verifier"`
		} `json:"signatures"`
		// intoto 0.0.2
		Content struct {
			PayloadHash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"payloadHash"`
		} `json:"content"`
		// Rekor v2
		HashedRekordV002 *struct {
			Data struct {
				Algorithm string `json:"algorithm"`
				Digest    string `json:"digest"`
			} `json:"data"`
			Signature struct {
				Content string `json:"content"`
			} `json:"signature"`
		} `json:"hashedRekordV002"`
		DSSEV002 *struct {
			PayloadHash struct {
				Algorithm string `json:"algorithm"`
				Digest    string `json:"digest"`
			} `json:"payloadHash"`
			Signatures []struct {
				Content string `json:"content"`
			} `json:"signatures"`
		} `json:"dsseV002"`
	} `json:"spec"`
}

// matchMessage returns the check that a Rekor body is the hashedrekord of
// a message signature over dgst (hex SHA-256).
func matchMessage(dgstHex string, sig []byte) func(kind, version string, body []byte) error {
	return func(kind, version string, b []byte) error {
		var body rekorBody
		if err := json.Unmarshal(b, &body); err != nil {
			return err
		}
		if body.Kind != "hashedrekord" || kind != "hashedrekord" {
			return fmt.Errorf("a %s entry cannot log a message signature", body.Kind)
		}
		if v2 := body.Spec.HashedRekordV002; v2 != nil {
			d, err := decodeB64(v2.Data.Digest)
			if err != nil || hex.EncodeToString(d) != dgstHex {
				return errors.New("the logged digest is not the artifact's")
			}
			if s, err := decodeB64(v2.Signature.Content); err != nil || !bytes.Equal(s, sig) {
				return errors.New("the logged signature is not the bundle's")
			}
			return nil
		}
		if !strings.EqualFold(body.Spec.Data.Hash.Value, dgstHex) {
			return errors.New("the logged digest is not the artifact's")
		}
		if s, err := decodeB64(body.Spec.Signature.Content); err != nil || !bytes.Equal(s, sig) {
			return errors.New("the logged signature is not the bundle's")
		}
		return nil
	}
}

// matchDSSE returns the check that a Rekor body logs a DSSE envelope with
// this payload and signature.
func matchDSSE(payload, sig []byte) func(kind, version string, body []byte) error {
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	return func(kind, version string, b []byte) error {
		var body rekorBody
		if err := json.Unmarshal(b, &body); err != nil {
			return err
		}
		switch body.Kind {
		case "dsse":
			if v2 := body.Spec.DSSEV002; v2 != nil {
				d, err := decodeB64(v2.PayloadHash.Digest)
				if err != nil || hex.EncodeToString(d) != want {
					return errors.New("the logged payload hash is not the envelope's")
				}
				for _, s := range v2.Signatures {
					if c, err := decodeB64(s.Content); err == nil && bytes.Equal(c, sig) {
						return nil
					}
				}
				return errors.New("the logged signatures do not include the envelope's")
			}
			if !strings.EqualFold(body.Spec.PayloadHash.Value, want) {
				return errors.New("the logged payload hash is not the envelope's")
			}
			for _, s := range body.Spec.Signatures {
				if c, err := decodeB64(s.Signature); err == nil && bytes.Equal(c, sig) {
					return nil
				}
			}
			return errors.New("the logged signatures do not include the envelope's")
		case "intoto":
			if !strings.EqualFold(body.Spec.Content.PayloadHash.Value, want) {
				return errors.New("the logged payload hash is not the envelope's")
			}
			return nil
		}
		return fmt.Errorf("a %s entry cannot log a DSSE envelope", body.Kind)
	}
}
