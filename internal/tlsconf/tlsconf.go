// Package tlsconf builds the TLS configuration of the servers that offer one.
package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

// Build decodes the configured key pair, or generates one. cert and key are the
// material itself, base64 of the PEM as it is held in the configuration file;
// certName and keyName are the configuration keys the messages talk about,
// "ftps.cert" and "ftps.key" for the FTP server.
//
// The Node implementation shipped a fixed certificate whose private key is
// published with the package, which means it offers no confidentiality at all.
// Generating a certificate keeps the server usable without any setup while
// making it obvious that it proves no identity. A generated one is kept in
// kept, under "tls." and the section of certName, so that a client that was
// told to trust it is not asked again after a restart; with kept nil it lasts
// this run only.
func Build(cert, key, certName, keyName string, kept *store.Bucket, logger *slog.Logger) (*tls.Config, error) {
	if cert != "" && key != "" {
		certificate, err := Pair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("%s and %s: %w", certName, keyName, err)
		}
		describe(certificate, certName, logger)
		return &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}, nil
	}

	certificate, err := selfSigned(kept, "tls."+strings.TrimSuffix(certName, ".cert"), time.Now(), logger)
	if err != nil {
		return nil, fmt.Errorf("cannot generate a certificate for %s: %w", certName, err)
	}
	if kept == nil {
		logger.Warn(fmt.Sprintf("no %s and %s configured, generated a temporary "+
			"self-signed certificate; it changes on every restart and proves no identity",
			certName, keyName))
	} else {
		logger.Warn(fmt.Sprintf("no %s and %s configured, serving a self-signed "+
			"certificate kept in the persistent data file; it proves no identity",
			certName, keyName), "notAfter", certificate.Leaf.NotAfter.Format(time.RFC3339))
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// keptPair is a generated certificate and its key as kept, in PEM.
type keptPair struct {
	Cert []byte `json:"cert"`
	Key  []byte `json:"key"`
}

// selfSigned is the certificate kept at name in kept, or, when there is none,
// it cannot be read or it runs out within expiryWarning, a new one, which is
// kept in its place. Where it cannot be kept it is used for this run, and
// the log says so.
func selfSigned(kept *store.Bucket, name string, now time.Time, logger *slog.Logger) (tls.Certificate, error) {
	var pair keptPair
	found, err := kept.Get(name, &pair)
	if err != nil {
		logger.Warn("cannot read the kept certificate, so a new one is generated", "key", name, "error", err)
	}
	if found && err == nil {
		certificate, err := tls.X509KeyPair(pair.Cert, pair.Key)
		if err == nil && certificate.Leaf != nil && certificate.Leaf.NotAfter.Sub(now) >= expiryWarning {
			return certificate, nil
		}
	}

	certPEM, keyPEM, err := SelfSignedPEM()
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := kept.Put(name, keptPair{Cert: certPEM, Key: keyPEM}); err != nil {
		logger.Warn("cannot keep the generated certificate, so it changes on every restart",
			"key", name, "error", err)
	}
	return certificate, nil
}

// expiryWarning is how long before a certificate runs out the log starts
// saying so. A certificate that has expired is the one TLS failure that
// arrives with no change to the configuration, so it is worth a warning
// while there is still time to renew.
const expiryWarning = 30 * 24 * time.Hour

// describe says which certificate a listener serves, so that a client's
// complaint about the name or the chain can be checked against what is
// actually loaded, and warns about one that is expiring.
func describe(certificate tls.Certificate, certName string, logger *slog.Logger) {
	leaf := certificate.Leaf
	if leaf == nil {
		parsed, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return
		}
		leaf = parsed
	}
	logger.Info("tls certificate loaded", "setting", certName,
		"subject", leaf.Subject.String(), "issuer", leaf.Issuer.String(),
		"dnsNames", leaf.DNSNames, "ipAddresses", leaf.IPAddresses,
		"notBefore", leaf.NotBefore.Format(time.RFC3339),
		"notAfter", leaf.NotAfter.Format(time.RFC3339),
		"chain", len(certificate.Certificate))
	now := time.Now()
	switch {
	case now.After(leaf.NotAfter):
		logger.Warn("the tls certificate has expired, clients will refuse it",
			"setting", certName, "notAfter", leaf.NotAfter.Format(time.RFC3339))
	case now.Before(leaf.NotBefore):
		logger.Warn("the tls certificate is not valid yet, clients will refuse it",
			"setting", certName, "notBefore", leaf.NotBefore.Format(time.RFC3339))
	case leaf.NotAfter.Sub(now) < expiryWarning:
		logger.Warn("the tls certificate expires soon",
			"setting", certName, "notAfter", leaf.NotAfter.Format(time.RFC3339),
			"remaining", leaf.NotAfter.Sub(now).Round(time.Hour))
	}
}

// Pair decodes a configured certificate and private key into the certificate a
// listener serves. It is where a pair that does not belong together is caught:
// tls.X509KeyPair checks that the key matches the certificate.
func Pair(cert, key string) (tls.Certificate, error) {
	certPEM, err := config.DecodeCertificate(cert)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := config.DecodePrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// SelfSignedPEM creates a certificate and its key, as the PEM they are stored
// and served as. It is the fallback of a listener with nothing configured, and
// what the web interface stores when Generate is pressed — the difference being
// that a stored one survives a restart.
func SelfSignedPEM() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	hostname, _ := os.Hostname()
	names := []string{"localhost"}
	if hostname != "" && hostname != "localhost" {
		names = append(names, hostname)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "go-fs self-signed"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}
