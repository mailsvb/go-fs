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
	"time"

	"go-fs/internal/config"
)

// Build decodes the configured key pair, or generates one. cert and key are the
// material itself, base64 of the PEM as it is held in the configuration file;
// certName and keyName are the configuration keys the messages talk about,
// "ftps.cert" and "ftps.key" for the FTP server.
//
// The Node implementation shipped a fixed certificate whose private key is
// published with the package, which means it offers no confidentiality at all.
// Generating a certificate at startup keeps the server usable without any setup
// while making it obvious that it proves no identity.
func Build(cert, key, certName, keyName string, logger *slog.Logger) (*tls.Config, error) {
	if cert != "" && key != "" {
		certificate, err := Pair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("%s and %s: %w", certName, keyName, err)
		}
		return &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}, nil
	}

	certPEM, keyPEM, err := SelfSignedPEM()
	if err != nil {
		return nil, fmt.Errorf("cannot generate a certificate for %s: %w", certName, err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("cannot generate a certificate for %s: %w", certName, err)
	}
	logger.Warn(fmt.Sprintf("no %s and %s configured, generated a temporary "+
		"self-signed certificate; it changes on every restart and proves no identity",
		certName, keyName))
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
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
