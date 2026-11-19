package imagesig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Notation's protected header parameters.
const (
	notaryScheme           = "io.cncf.notary.signingScheme"
	notarySigningTime      = "io.cncf.notary.signingTime"
	notaryExpiry           = "io.cncf.notary.expiry"
	notaryAuthenticTime    = "io.cncf.notary.authenticSigningTime"
	notaryPlugin           = "io.cncf.notary.verificationPlugin"
	notaryPluginMin        = "io.cncf.notary.verificationPluginMinVersion"
	notaryPayloadType      = "application/vnd.cncf.notary.payload.v1+json"
	schemeX509             = "notary.x509"
	schemeSigningAuthority = "notary.x509.signingAuthority"
)

// jws is a JWS in flattened JSON serialization, as Notation writes it.
type jws struct {
	Payload   string `json:"payload"`
	Protected string `json:"protected"`
	Header    struct {
		X5C []string `json:"x5c"`
	} `json:"header"`
	Signature string `json:"signature"`
}

type notaryPayload struct {
	TargetArtifact struct {
		MediaType string        `json:"mediaType"`
		Digest    digest.Digest `json:"digest"`
		Size      int64         `json:"size"`
	} `json:"targetArtifact"`
}

// verifyNotation checks a Notation signature manifest. Only JWS envelopes
// are supported. The certificate chain is checked at the authentic signing
// time of the signingAuthority scheme and at the present otherwise, since
// timestamp countersignatures are not read; revocation is not checked.
func verifyNotation(subject digest.Digest, m *ocispec.Manifest, fetch Fetch, trusts []*Trust) Result {
	r := Result{Format: FormatNotation, Kind: KindSignature}
	if len(m.Layers) != 1 {
		r.Err = fmt.Sprintf("a Notation signature has one layer, this one has %d", len(m.Layers))
		return r
	}
	l := m.Layers[0]
	switch l.MediaType {
	case mediaTypeJWS:
	case mediaTypeCOSE:
		r.Err = "COSE Notation signatures are not supported, only JWS"
		return r
	default:
		r.Err = fmt.Sprintf("unknown Notation envelope type %q", l.MediaType)
		return r
	}
	if m.Subject != nil && m.Subject.Digest != subject {
		r.Err = fmt.Sprintf("the signature manifest refers to %s, not %s", m.Subject.Digest, subject)
		return r
	}
	b, err := fetchLayer(fetch, l)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if err := verifyJWS(&r, subject, b, trusts); err != nil {
		r.Err = err.Error()
	}
	return r
}

func verifyJWS(r *Result, subject digest.Digest, raw []byte, trusts []*Trust) error {
	var env jws
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("JWS envelope: %w", err)
	}
	protected, err := base64.RawURLEncoding.DecodeString(env.Protected)
	if err != nil {
		return fmt.Errorf("JWS protected header: %w", err)
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(protected, &hdr); err != nil {
		return fmt.Errorf("JWS protected header: %w", err)
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(hdr[k], &s)
		return s
	}
	var crit []string
	if c, ok := hdr["crit"]; ok {
		if err := json.Unmarshal(c, &crit); err != nil {
			return errors.New("JWS crit header is not a list of strings")
		}
	}
	isCrit := map[string]bool{}
	for _, c := range crit {
		switch c {
		case notaryScheme, notaryExpiry, notaryAuthenticTime:
		case notaryPlugin, notaryPluginMin:
			return fmt.Errorf("the signature needs verification plugin %q, which is not supported", str(notaryPlugin))
		default:
			return fmt.Errorf("unknown critical header %q", c)
		}
		if _, ok := hdr[c]; !ok {
			return fmt.Errorf("critical header %q is missing", c)
		}
		isCrit[c] = true
	}
	if cty := str("cty"); cty != notaryPayloadType {
		return fmt.Errorf("JWS content type %q is not a Notation payload", cty)
	}
	scheme := str(notaryScheme)
	if !isCrit[notaryScheme] {
		return errors.New("the signing scheme header is not marked critical")
	}
	if _, ok := hdr[notaryExpiry]; ok && !isCrit[notaryExpiry] {
		return errors.New("the expiry header is not marked critical")
	}

	payload, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		return fmt.Errorf("JWS payload: %w", err)
	}
	var p notaryPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("notation payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("JWS signature: %w", err)
	}
	if len(env.Header.X5C) == 0 {
		return errors.New("the signature carries no certificate chain")
	}
	var chain []*x509.Certificate
	for _, c := range env.Header.X5C {
		der, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			return fmt.Errorf("x5c: %w", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("x5c: %w", err)
		}
		chain = append(chain, cert)
	}
	leaf := chain[0]
	r.Identity = leaf.Subject.String()

	signingInput := env.Protected + "." + env.Payload
	if err := verifyJWSSignature(str("alg"), leaf.PublicKey, []byte(signingInput), sig); err != nil {
		return err
	}
	if p.TargetArtifact.Digest != subject {
		return fmt.Errorf("the signature is for %s, not %s", orNone(p.TargetArtifact.Digest.String()), subject)
	}

	present := now()
	if _, ok := hdr[notaryExpiry]; ok {
		exp, err := time.Parse(time.RFC3339, str(notaryExpiry))
		if err != nil {
			return fmt.Errorf("expiry header: %w", err)
		}
		if present.After(exp) {
			return fmt.Errorf("the signature expired at %s", exp.UTC().Format(time.RFC3339))
		}
	}
	at := present
	switch scheme {
	case schemeX509:
		if st := str(notarySigningTime); st != "" {
			if _, err := time.Parse(time.RFC3339, st); err != nil {
				return fmt.Errorf("signing time header: %w", err)
			}
		}
	case schemeSigningAuthority:
		if !isCrit[notaryAuthenticTime] {
			return errors.New("the authentic signing time header is not marked critical")
		}
		if at, err = time.Parse(time.RFC3339, str(notaryAuthenticTime)); err != nil {
			return fmt.Errorf("authentic signing time header: %w", err)
		}
	default:
		return fmt.Errorf("unknown signing scheme %q", scheme)
	}

	te := trustErrors{what: "Notation certificates"}
	for _, t := range trusts {
		if t.Roots == nil {
			continue
		}
		inter := x509.NewCertPool()
		for _, c := range chain[1:] {
			inter.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots:         t.Roots,
			Intermediates: inter,
			CurrentTime:   at,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		})
		if err != nil {
			te.add(t.Name, fmt.Errorf("certificate chain: %w", err))
			continue
		}
		r.OK, r.TrustedBy = true, t.Name
		return nil
	}
	return errors.New(te.String())
}

// verifyJWSSignature checks a JWS signature of the algorithms Notation
// allows: RSASSA-PSS and ECDSA, with the key size the algorithm implies.
func verifyJWSSignature(alg string, pub crypto.PublicKey, input, sig []byte) error {
	var h crypto.Hash
	switch alg {
	case "PS256", "ES256":
		h = crypto.SHA256
	case "PS384", "ES384":
		h = crypto.SHA384
	case "PS512", "ES512":
		h = crypto.SHA512
	default:
		return fmt.Errorf("JWS algorithm %q is not one Notation allows", alg)
	}
	dg := digestOf(h, input)
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if !strings.HasPrefix(alg, "PS") {
			return fmt.Errorf("an RSA certificate cannot make %s signatures", alg)
		}
		if k.N.BitLen() < 2048 {
			return errors.New("the RSA key is too small")
		}
		if rsa.VerifyPSS(k, h, dg, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) != nil {
			return errors.New("the JWS signature does not verify")
		}
		return nil
	case *ecdsa.PublicKey:
		want := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg]
		if want == nil || k.Curve != want {
			return fmt.Errorf("the certificate's curve does not go with %s", alg)
		}
		n := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return errors.New("the JWS signature does not verify")
		}
		rr, ss := new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(k, dg, rr, ss) {
			return errors.New("the JWS signature does not verify")
		}
		return nil
	}
	return fmt.Errorf("unsupported certificate key %T", pub)
}
