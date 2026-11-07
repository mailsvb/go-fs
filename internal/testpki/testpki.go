// Package testpki makes the certificates the tests of the NBD server and its
// configuration need: a CA, a server certificate and client certificates it
// issues, and revocation lists. It is imported by tests alone, so none of it
// ends up in a binary.
package testpki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// CA is a certificate authority of a test.
type CA struct {
	Cert    *x509.Certificate
	Key     crypto.Signer
	CertPEM []byte
}

// Pair is a certificate an authority issued, with its key.
type Pair struct {
	Cert    *x509.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// TLS is the pair as crypto/tls takes it.
func (p Pair) TLS(t testing.TB) tls.Certificate {
	t.Helper()
	certificate, err := tls.X509KeyPair(p.CertPEM, p.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

// Options change what a certificate is issued as.
type Options struct {
	// NotBefore and NotAfter default to an hour ago and a day from now.
	NotBefore, NotAfter time.Time
	// Usage defaults to client authentication for Client and server
	// authentication for Server.
	Usage []x509.ExtKeyUsage
}

// NewCA makes a self-signed CA named name.
func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	key := newKey(t)
	template := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Cert: cert, Key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Server issues a server certificate for hosts, names or addresses.
func (ca *CA) Server(t testing.TB, hosts ...string) Pair {
	t.Helper()
	return ca.issue(t, "server", hosts, Options{}, x509.ExtKeyUsageServerAuth)
}

// Client issues a client certificate whose common name is name.
func (ca *CA) Client(t testing.TB, name string) Pair {
	t.Helper()
	return ca.ClientWith(t, name, Options{})
}

// ClientWith is Client with options.
func (ca *CA) ClientWith(t testing.TB, name string, options Options) Pair {
	t.Helper()
	return ca.issue(t, name, nil, options, x509.ExtKeyUsageClientAuth)
}

func (ca *CA) issue(t testing.TB, name string, hosts []string, options Options, usage x509.ExtKeyUsage) Pair {
	t.Helper()
	key := newKey(t)
	template := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	if !options.NotBefore.IsZero() {
		template.NotBefore = options.NotBefore
	}
	if !options.NotAfter.IsZero() {
		template.NotAfter = options.NotAfter
	}
	if options.Usage != nil {
		template.ExtKeyUsage = options.Usage
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return Pair{
		Cert:    cert,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

// CRL is a revocation list of the authority that revokes revoked.
func (ca *CA) CRL(t testing.TB, revoked ...*x509.Certificate) []byte {
	t.Helper()
	template := &x509.RevocationList{
		Number:     serial(t),
		ThisUpdate: time.Now().Add(-time.Minute),
		NextUpdate: time.Now().Add(24 * time.Hour),
	}
	for _, cert := range revoked {
		template.RevokedCertificateEntries = append(template.RevokedCertificateEntries,
			x509.RevocationListEntry{SerialNumber: cert.SerialNumber, RevocationTime: time.Now()})
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, ca.Cert, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// Encode is PEM as the configuration file holds it.
func Encode(pemBytes []byte) string { return base64.StdEncoding.EncodeToString(pemBytes) }

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
