package imagesig

import (
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

// envelope is a DSSE envelope, in cosign's .att layers and in Sigstore
// bundles.
type envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// decoded is an envelope with its payload and signatures decoded.
type decoded struct {
	payloadType string
	payload     []byte
	sigs        [][]byte
}

func (e *envelope) decode() (*decoded, error) {
	p, err := decodeB64(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("DSSE payload: %w", err)
	}
	d := &decoded{payloadType: e.PayloadType, payload: p}
	for _, s := range e.Signatures {
		b, err := decodeB64(s.Sig)
		if err != nil {
			return nil, fmt.Errorf("DSSE signature: %w", err)
		}
		d.sigs = append(d.sigs, b)
	}
	if len(d.sigs) == 0 {
		return nil, errors.New("DSSE envelope has no signature")
	}
	return d, nil
}

// verify checks that one of the envelope's signatures is pub's over the
// pre-authentication encoding, and returns that signature.
func (d *decoded) verify(pub crypto.PublicKey) ([]byte, error) {
	msg := pae(d.payloadType, d.payload)
	err := errBadSignature
	for _, s := range d.sigs {
		if err = verifyMessage(pub, msg, s); err == nil {
			return s, nil
		}
	}
	return nil, err
}

// statement is the part of an in-toto statement Verify looks at.
type statement struct {
	Type    string `json:"_type"`
	Subject []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	PredicateType string `json:"predicateType"`
}

// checkStatement parses an in-toto statement, requires one of its subjects
// to be the image and returns the predicate type.
func checkStatement(payloadType string, payload []byte, subject digest.Digest) (string, error) {
	if payloadType != mediaTypeInToto {
		return "", fmt.Errorf("DSSE payload type %q is not an in-toto statement", payloadType)
	}
	var st statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return "", fmt.Errorf("in-toto statement: %w", err)
	}
	if !strings.HasPrefix(st.Type, "https://in-toto.io/Statement/") {
		return st.PredicateType, fmt.Errorf("in-toto statement has type %q", st.Type)
	}
	alg, hexd := string(subject.Algorithm()), subject.Encoded()
	for _, s := range st.Subject {
		if strings.EqualFold(s.Digest[alg], hexd) {
			return st.PredicateType, nil
		}
	}
	return st.PredicateType, fmt.Errorf("no subject of the attestation is %s", subject)
}

// kindOf tells signatures from attestations: cosign v3 signs images with an
// in-toto statement of its own predicate type.
func kindOf(predicateType string) string {
	if predicateType == predicateCosignSign {
		return KindSignature
	}
	return KindAttestation
}
