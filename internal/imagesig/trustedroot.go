package imagesig

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// TrustedRoot is a parsed Sigstore trusted root
// (application/vnd.dev.sigstore.trustedroot+json): the Fulcio certificate
// authorities that issue keyless signing certificates, the Rekor logs whose
// entries and checkpoints prove when something was signed, and the RFC 3161
// timestamp authorities that can prove it instead. CT logs are not used.
type TrustedRoot struct {
	CAs   []CertAuthority
	Tlogs []TransparencyLog
	TSAs  []CertAuthority
}

// CertAuthority is a Fulcio CA or a timestamp authority: its chain, leaf
// first and root last, and the time it was in service.
type CertAuthority struct {
	URI       string
	Chain     []*x509.Certificate
	Start     time.Time
	End       time.Time // zero while in service
	pool      *x509.CertPool
	interPool *x509.CertPool
}

// TransparencyLog is a Rekor log: its key and the time it was in service.
type TransparencyLog struct {
	BaseURL string
	LogID   []byte // SHA-256 of the key's PKIX DER, how entries name their log
	Key     crypto.PublicKey
	Start   time.Time
	End     time.Time
}

func (c *CertAuthority) validAt(t time.Time) bool   { return within(t, c.Start, c.End) }
func (l *TransparencyLog) validAt(t time.Time) bool { return within(t, l.Start, l.End) }

func within(t, start, end time.Time) bool {
	return !t.Before(start) && (end.IsZero() || !t.After(end))
}

// roots is the CA's root certificate as a pool and the rest of its chain
// as intermediates. ParseTrustedRoot builds them once, so that concurrent
// verifications only read them.
func (c *CertAuthority) roots() (*x509.CertPool, *x509.CertPool) {
	if c.pool != nil {
		return c.pool, c.interPool
	}
	return buildPools(c.Chain)
}

func buildPools(chain []*x509.Certificate) (*x509.CertPool, *x509.CertPool) {
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	for i, cert := range chain {
		if i == len(chain)-1 {
			roots.AddCert(cert)
		} else {
			inter.AddCert(cert)
		}
	}
	return roots, inter
}

type jsonValidFor struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end"`
}

type jsonAuthority struct {
	URI       string `json:"uri"`
	CertChain struct {
		Certificates []struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificates"`
	} `json:"certChain"`
	ValidFor jsonValidFor `json:"validFor"`
}

type jsonTrustedRoot struct {
	MediaType string          `json:"mediaType"`
	CAs       []jsonAuthority `json:"certificateAuthorities"`
	TSAs      []jsonAuthority `json:"timestampAuthorities"`
	Tlogs     []struct {
		BaseURL   string `json:"baseUrl"`
		PublicKey struct {
			RawBytes   string       `json:"rawBytes"`
			KeyDetails string       `json:"keyDetails"`
			ValidFor   jsonValidFor `json:"validFor"`
		} `json:"publicKey"`
		LogID struct {
			KeyID string `json:"keyId"`
		} `json:"logId"`
	} `json:"tlogs"`
}

// ParseTrustedRoot parses a Sigstore trusted_root.json. Several roots one
// after the other (the JSON lines `gh attestation trusted-root` prints, with
// the public-good root and GitHub's) are merged into one.
func ParseTrustedRoot(b []byte) (*TrustedRoot, error) {
	tr := &TrustedRoot{}
	dec := json.NewDecoder(bytes.NewReader(b))
	n := 0
	for {
		var j jsonTrustedRoot
		err := dec.Decode(&j)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n++
		if j.MediaType != "" && !strings.HasPrefix(j.MediaType, "application/vnd.dev.sigstore.trustedroot") {
			return nil, fmt.Errorf("media type %q is not a Sigstore trusted root", j.MediaType)
		}
		for _, a := range j.CAs {
			ca, err := parseAuthority(a)
			if err != nil {
				return nil, fmt.Errorf("certificate authority %s: %w", a.URI, err)
			}
			tr.CAs = append(tr.CAs, ca)
		}
		for _, a := range j.TSAs {
			ca, err := parseAuthority(a)
			if err != nil {
				return nil, fmt.Errorf("timestamp authority %s: %w", a.URI, err)
			}
			tr.TSAs = append(tr.TSAs, ca)
		}
		for _, l := range j.Tlogs {
			der, err := decodeB64(l.PublicKey.RawBytes)
			if err != nil {
				return nil, fmt.Errorf("tlog %s key: %w", l.BaseURL, err)
			}
			k, err := x509.ParsePKIXPublicKey(der)
			if err != nil {
				return nil, fmt.Errorf("tlog %s key: %w", l.BaseURL, err)
			}
			id, err := decodeB64(l.LogID.KeyID)
			if err != nil {
				return nil, fmt.Errorf("tlog %s log ID: %w", l.BaseURL, err)
			}
			tl := TransparencyLog{BaseURL: l.BaseURL, LogID: id, Key: k, Start: l.PublicKey.ValidFor.Start}
			if l.PublicKey.ValidFor.End != nil {
				tl.End = *l.PublicKey.ValidFor.End
			}
			tr.Tlogs = append(tr.Tlogs, tl)
		}
	}
	if n == 0 {
		return nil, errors.New("no trusted root found")
	}
	if len(tr.CAs) == 0 && len(tr.TSAs) == 0 && len(tr.Tlogs) == 0 {
		return nil, errors.New("the trusted root holds no certificate authority, log or timestamp authority")
	}
	return tr, nil
}

func parseAuthority(a jsonAuthority) (CertAuthority, error) {
	ca := CertAuthority{URI: a.URI, Start: a.ValidFor.Start}
	if a.ValidFor.End != nil {
		ca.End = *a.ValidFor.End
	}
	for _, c := range a.CertChain.Certificates {
		der, err := decodeB64(c.RawBytes)
		if err != nil {
			return ca, err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return ca, err
		}
		ca.Chain = append(ca.Chain, cert)
	}
	if len(ca.Chain) == 0 {
		return ca, errors.New("no certificates")
	}
	ca.pool, ca.interPool = buildPools(ca.Chain)
	return ca, nil
}

func (tr *TrustedRoot) tlog(id []byte) *TransparencyLog {
	for i := range tr.Tlogs {
		if bytes.Equal(tr.Tlogs[i].LogID, id) {
			return &tr.Tlogs[i]
		}
	}
	return nil
}
