package imagesig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// A small PKI for tests: CAs, leaves, RFC 3161 tokens and trusted roots
// made with the standard library.

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

var serial int64 = 1000

func newSerial() *big.Int {
	serial++
	return big.NewInt(serial)
}

func ecKey(t *testing.T, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func makeCert(t *testing.T, tmpl *x509.Certificate, pub crypto.PublicKey, parent *testCA) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = newSerial()
	issuer, signer := tmpl, crypto.Signer(nil)
	if parent != nil {
		issuer, signer = parent.cert, parent.key
	}
	if signer == nil {
		t.Fatal("self-signed certificates need the CA's own key as parent")
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newRoot makes a self-signed CA valid around the test's time.
func newRoot(t *testing.T, cn string, key crypto.Signer) *testCA {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"imagesig test"}},
		NotBefore:             time.Now().Add(-10 * 365 * 24 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key}
}

func newIntermediate(t *testing.T, cn string, parent *testCA, ekus ...x509.ExtKeyUsage) *testCA {
	key := ecKey(t, elliptic.P256())
	c := makeCert(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"imagesig test"}},
		NotBefore:             parent.cert.NotBefore,
		NotAfter:              parent.cert.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		ExtKeyUsage:           ekus,
	}, key.Public(), parent)
	return &testCA{cert: c, key: key}
}

// fulcioLeaf makes a short-lived keyless signing certificate for identity
// with the OIDC issuer extension.
func fulcioLeaf(t *testing.T, ca *testCA, identity, issuer string, notBefore time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key := ecKey(t, elliptic.P256())
	u, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := asn1.MarshalWithParams(issuer, "utf8")
	c := makeCert(t, &x509.Certificate{
		NotBefore:       notBefore,
		NotAfter:        notBefore.Add(10 * time.Minute),
		KeyUsage:        x509.KeyUsageDigitalSignature,
		ExtKeyUsage:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:            []*url.URL{u},
		ExtraExtensions: []pkix.Extension{{Id: oidIssuerV2, Value: iss}},
	}, key.Public(), ca)
	return c, key
}

// der builds a DER element from a tag byte and its contents.
func der(tag byte, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	n := len(body)
	var l []byte
	switch {
	case n < 0x80:
		l = []byte{byte(n)}
	case n < 0x100:
		l = []byte{0x81, byte(n)}
	case n < 0x10000:
		l = []byte{0x82, byte(n >> 8), byte(n)}
	default:
		l = []byte{0x83, byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return append(append([]byte{tag}, l...), body...)
}

func mustMarshal(t *testing.T, v any, params ...string) []byte {
	t.Helper()
	p := ""
	if len(params) > 0 {
		p = params[0]
	}
	b, err := asn1.MarshalWithParams(v, p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var (
	oidSHA256          = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
)

// rfc3161 makes a TimeStampResp over msg, signed by the TSA leaf at genTime.
func rfc3161(t *testing.T, tsa *testCA, msg []byte, genTime time.Time, includeCert bool) []byte {
	t.Helper()
	sum := sha256.Sum256(msg)
	algID := der(0x30, mustMarshal(t, oidSHA256), []byte{0x05, 0x00})
	tst := der(0x30,
		mustMarshal(t, 1),
		mustMarshal(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 2}),
		der(0x30, algID, mustMarshal(t, sum[:])),
		mustMarshal(t, big.NewInt(42)),
		mustMarshal(t, genTime.UTC(), "generalized"),
	)
	tstSum := sha256.Sum256(tst)
	attrs := [][]byte{
		der(0x30, mustMarshal(t, oidContentType), der(0x31, mustMarshal(t, oidTSTInfo))),
		der(0x30, mustMarshal(t, oidMessageDigest), der(0x31, mustMarshal(t, tstSum[:]))),
	}
	signed := der(0x31, attrs...)
	h := sha256.Sum256(signed)
	sig, err := tsa.key.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	signer := der(0x30,
		mustMarshal(t, 1),
		der(0x30, tsa.cert.RawIssuer, mustMarshal(t, tsa.cert.SerialNumber)),
		algID,
		der(0xa0, attrs...),
		der(0x30, mustMarshal(t, oidECDSAWithSHA256)),
		mustMarshal(t, sig),
	)
	var certs []byte
	if includeCert {
		certs = der(0xa0, tsa.cert.Raw)
	}
	sd := der(0x30,
		mustMarshal(t, 3),
		der(0x31, algID),
		der(0x30, mustMarshal(t, oidTSTInfo), der(0xa0, mustMarshal(t, tst))),
		certs,
		der(0x31, signer),
	)
	token := der(0x30, mustMarshal(t, oidSignedData), der(0xa0, sd))
	return der(0x30, der(0x30, mustMarshal(t, 0)), token)
}

// rootJSON writes a trusted_root.json with one Fulcio CA and one TSA, as
// lists of certificates leaf first.
func rootJSON(t *testing.T, fulcio, tsa []*x509.Certificate, start time.Time) []byte {
	t.Helper()
	chain := func(cs []*x509.Certificate) map[string]any {
		var out []map[string]string
		for _, c := range cs {
			out = append(out, map[string]string{"rawBytes": encB64(c.Raw)})
		}
		return map[string]any{"certificates": out}
	}
	j := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"certificateAuthorities": []any{map[string]any{
			"subject": map[string]string{"organization": "imagesig test"}, "uri": "https://fulcio.test",
			"certChain": chain(fulcio), "validFor": map[string]any{"start": start.Format(time.RFC3339)},
		}},
		"timestampAuthorities": []any{map[string]any{
			"subject": map[string]string{"organization": "imagesig test"}, "uri": "https://tsa.test",
			"certChain": chain(tsa), "validFor": map[string]any{"start": start.Format(time.RFC3339)},
		}},
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
