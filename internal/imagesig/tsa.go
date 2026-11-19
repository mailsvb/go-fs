package imagesig

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Object identifiers of CMS and RFC 3161.
var (
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
)

var hashByOID = map[string]crypto.Hash{
	"2.16.840.1.101.3.4.2.1": crypto.SHA256,
	"2.16.840.1.101.3.4.2.2": crypto.SHA384,
	"2.16.840.1.101.3.4.2.3": crypto.SHA512,
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type tstInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint struct {
		HashAlgorithm algorithmIdentifier
		HashedMessage []byte
	}
	SerialNumber *big.Int
	GenTime      time.Time `asn1:"generalized"`
	// accuracy, ordering, nonce, tsa and extensions follow; encoding/asn1
	// leaves trailing elements alone.
}

type attribute struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

// timestamp is a parsed RFC 3161 timestamp token.
type timestamp struct {
	genTime  time.Time
	imprint  []byte
	hash     crypto.Hash
	eContent []byte // DER TSTInfo
	signers  []signerInfo
	certs    []*x509.Certificate
}

type signerInfo struct {
	digest      crypto.Hash
	signedAttrs []byte // DER, re-tagged as the SET that is signed
	signature   []byte
	attrDigest  []byte // the messageDigest attribute
	contentType asn1.ObjectIdentifier
}

// mustSequence returns the contents of a DER SEQUENCE, or nil.
func mustSequence(b []byte) []byte {
	var rv asn1.RawValue
	if rest, err := asn1.Unmarshal(b, &rv); err != nil || len(rest) > 0 || rv.Tag != asn1.TagSequence || rv.Class != asn1.ClassUniversal {
		return nil
	}
	return rv.Bytes
}

// children returns the elements inside a constructed ASN.1 value.
func children(b []byte) ([]asn1.RawValue, error) {
	var out []asn1.RawValue
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
		b = rest
	}
	return out, nil
}

func isContext(rv asn1.RawValue, tag int) bool {
	return rv.Class == asn1.ClassContextSpecific && rv.Tag == tag
}

// parseTimestamp parses a DER TimeStampToken: a CMS ContentInfo with
// SignedData around a TSTInfo.
func parseTimestamp(der []byte) (*timestamp, error) {
	// Sigstore bundles keep the whole TimeStampResp: a PKIStatusInfo
	// before the token.
	if top, err := children(mustSequence(der)); err == nil && len(top) == 2 && top[0].Tag == asn1.TagSequence {
		var status int
		f, err := children(top[0].Bytes)
		if err != nil || len(f) == 0 {
			return nil, errors.New("timestamp response: malformed status")
		}
		if _, err := asn1.Unmarshal(f[0].FullBytes, &status); err != nil {
			return nil, fmt.Errorf("timestamp response: %w", err)
		}
		if status != 0 && status != 1 {
			return nil, fmt.Errorf("timestamp response has status %d", status)
		}
		der = top[1].FullBytes
	}
	var ci struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if rest, err := asn1.Unmarshal(der, &ci); err != nil {
		return nil, fmt.Errorf("timestamp token: %w", err)
	} else if len(rest) > 0 {
		return nil, errors.New("timestamp token has trailing data")
	}
	if !ci.ContentType.Equal(oidSignedData) {
		return nil, errors.New("timestamp token is not CMS SignedData")
	}
	// encoding/asn1 hands an explicitly tagged RawValue over with its tag.
	inner := ci.Content.Bytes
	if isContext(ci.Content, 0) {
		inner = mustSequence(ci.Content.Bytes)
	}
	sd, err := children(inner)
	if err != nil || len(sd) < 4 {
		return nil, errors.New("timestamp token: malformed SignedData")
	}
	// version, digestAlgorithms, encapContentInfo, [0] certificates,
	// [1] crls, signerInfos
	encap, err := children(sd[2].Bytes)
	if err != nil || len(encap) != 2 {
		return nil, errors.New("timestamp token: malformed encapsulated content")
	}
	var eType asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(encap[0].FullBytes, &eType); err != nil || !eType.Equal(oidTSTInfo) {
		return nil, errors.New("timestamp token does not hold a TSTInfo")
	}
	var eContent []byte
	if !isContext(encap[1], 0) {
		return nil, errors.New("timestamp token: malformed encapsulated content")
	}
	if _, err := asn1.Unmarshal(encap[1].Bytes, &eContent); err != nil {
		return nil, fmt.Errorf("timestamp token content: %w", err)
	}
	var info tstInfo
	if _, err := asn1.Unmarshal(eContent, &info); err != nil {
		return nil, fmt.Errorf("TSTInfo: %w", err)
	}
	h, ok := hashByOID[info.MessageImprint.HashAlgorithm.Algorithm.String()]
	if !ok {
		return nil, fmt.Errorf("timestamp message imprint uses unsupported hash %s", info.MessageImprint.HashAlgorithm.Algorithm)
	}
	ts := &timestamp{genTime: info.GenTime, imprint: info.MessageImprint.HashedMessage, hash: h, eContent: eContent}
	rest := sd[3:]
	for len(rest) > 1 && rest[0].Class == asn1.ClassContextSpecific {
		if rest[0].Tag == 0 {
			if ts.certs, err = x509.ParseCertificates(rest[0].Bytes); err != nil {
				return nil, fmt.Errorf("timestamp certificates: %w", err)
			}
		}
		rest = rest[1:]
	}
	if len(rest) != 1 {
		return nil, errors.New("timestamp token: malformed SignedData")
	}
	infos, err := children(rest[0].Bytes)
	if err != nil {
		return nil, err
	}
	for _, si := range infos {
		s, err := parseSignerInfo(si.Bytes)
		if err != nil {
			return nil, err
		}
		ts.signers = append(ts.signers, s)
	}
	if len(ts.signers) == 0 {
		return nil, errors.New("timestamp token has no signer")
	}
	return ts, nil
}

func parseSignerInfo(b []byte) (signerInfo, error) {
	var s signerInfo
	f, err := children(b)
	if err != nil || len(f) < 5 {
		return s, errors.New("timestamp token: malformed SignerInfo")
	}
	// version, sid, digestAlgorithm, [0] signedAttrs, signatureAlgorithm,
	// signature, [1] unsignedAttrs
	var da algorithmIdentifier
	if _, err := asn1.Unmarshal(f[2].FullBytes, &da); err != nil {
		return s, err
	}
	var ok bool
	if s.digest, ok = hashByOID[da.Algorithm.String()]; !ok {
		return s, fmt.Errorf("timestamp signer uses unsupported digest %s", da.Algorithm)
	}
	if !isContext(f[3], 0) || len(f) < 6 {
		return s, errors.New("timestamp signer has no signed attributes")
	}
	s.signedAttrs = append([]byte(nil), f[3].FullBytes...)
	s.signedAttrs[0] = 0x31 // [0] IMPLICIT becomes SET OF for signing
	attrs, err := children(f[3].Bytes)
	if err != nil {
		return s, err
	}
	for _, a := range attrs {
		var at attribute
		if _, err := asn1.Unmarshal(a.FullBytes, &at); err != nil {
			return s, err
		}
		vals, err := children(at.Values.Bytes)
		if err != nil || len(vals) != 1 {
			continue
		}
		switch {
		case at.Type.Equal(oidContentType):
			_, _ = asn1.Unmarshal(vals[0].FullBytes, &s.contentType)
		case at.Type.Equal(oidMessageDigest):
			_, _ = asn1.Unmarshal(vals[0].FullBytes, &s.attrDigest)
		}
	}
	var sa algorithmIdentifier
	if _, err := asn1.Unmarshal(f[4].FullBytes, &sa); err != nil {
		return s, err
	}
	if _, err := asn1.Unmarshal(f[5].FullBytes, &s.signature); err != nil {
		return s, err
	}
	return s, nil
}

// verifyTimestamp checks an RFC 3161 token over sig against the timestamp
// authorities of the trusted root and returns the time it vouches for.
func verifyTimestamp(der, sig []byte, root *TrustedRoot) (time.Time, error) {
	ts, err := parseTimestamp(der)
	if err != nil {
		return time.Time{}, err
	}
	if !bytes.Equal(digestOf(ts.hash, sig), ts.imprint) {
		return time.Time{}, errors.New("the timestamp is not over the signature")
	}
	if len(root.TSAs) == 0 {
		return time.Time{}, errors.New("the trusted root has no timestamp authority")
	}
	var last error = errors.New("no timestamp authority of the trusted root signed the timestamp")
	for i := range root.TSAs {
		tsa := &root.TSAs[i]
		if !tsa.validAt(ts.genTime) {
			continue
		}
		// The signing certificate is in the token or is the leaf of the
		// authority's chain.
		candidates := append(append([]*x509.Certificate(nil), ts.certs...), tsa.Chain[0])
		for _, s := range ts.signers {
			if !s.contentType.Equal(oidTSTInfo) || !bytes.Equal(s.attrDigest, digestOf(s.digest, ts.eContent)) {
				last = errors.New("the timestamp's signed attributes do not match its content")
				continue
			}
			for _, c := range candidates {
				if err := verifyDigest(c.PublicKey, s.digest, digestOf(s.digest, s.signedAttrs), s.signature); err != nil {
					continue
				}
				if err := chainTSA(c, ts.certs, tsa, ts.genTime); err != nil {
					last = err
					continue
				}
				return ts.genTime, nil
			}
		}
	}
	return time.Time{}, last
}

// chainTSA checks that the timestamp's signing certificate chains to the
// authority's root and may sign timestamps.
func chainTSA(c *x509.Certificate, extra []*x509.Certificate, tsa *CertAuthority, at time.Time) error {
	roots, inter := tsa.roots()
	if len(tsa.Chain) == 1 && c.Equal(tsa.Chain[0]) {
		if !within(at, c.NotBefore, c.NotAfter) {
			return errors.New("the timestamp certificate was not valid at the timestamp")
		}
		return nil
	}
	pool := inter.Clone()
	for _, e := range extra {
		pool.AddCert(e)
	}
	_, err := c.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: pool,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	})
	if err != nil {
		return fmt.Errorf("timestamp certificate: %w", err)
	}
	return nil
}
