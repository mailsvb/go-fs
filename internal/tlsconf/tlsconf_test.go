package tlsconf

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

func capture() (*slog.Logger, *bytes.Buffer) {
	var out bytes.Buffer
	return slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})), &out
}

// certificateExpiring makes a PEM pair whose certificate runs out at notAfter.
func certificateExpiring(t *testing.T, notAfter time.Time) (cert, key string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "expiry test"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"files.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return config.Encode(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		config.Encode(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func TestBuildDescribesTheCertificate(t *testing.T) {
	certPEM, keyPEM, err := SelfSignedPEM()
	if err != nil {
		t.Fatal(err)
	}
	logger, out := capture()
	if _, err := Build(config.Encode(certPEM), config.Encode(keyPEM), "ftps.cert", "ftps.key", nil, logger); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	if !strings.Contains(log, `msg="tls certificate loaded"`) || !strings.Contains(log, "setting=ftps.cert") {
		t.Errorf("a loaded certificate has to be described:\n%s", log)
	}
	if !strings.Contains(log, "localhost") || !strings.Contains(log, "notAfter=") {
		t.Errorf("the record has to carry the names and the expiry:\n%s", log)
	}
	if strings.Contains(log, "level=WARN") {
		t.Errorf("a fresh certificate must not warn:\n%s", log)
	}
}

func TestBuildWarnsAboutExpiry(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"expires soon", time.Now().Add(10 * 24 * time.Hour), "expires soon"},
		{"expired", time.Now().Add(-time.Hour), "has expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, key := certificateExpiring(t, tc.notAfter)
			logger, out := capture()
			if _, err := Build(cert, key, "https.cert", "https.key", nil, logger); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "level=WARN") || !strings.Contains(out.String(), tc.want) {
				t.Errorf("want a warning saying %q:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestBuildWithoutMaterialWarns(t *testing.T) {
	logger, out := capture()
	if _, err := Build("", "", "ftps.cert", "ftps.key", nil, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "generated a temporary") {
		t.Errorf("a generated certificate has to be reported:\n%s", out.String())
	}
}

func TestBuildKeepsTheGeneratedCertificate(t *testing.T) {
	logger, out := capture()
	st, err := store.Open(t.TempDir()+"/go-fs.db", logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys := st.Bucket("keys")

	first, err := Build("", "", "ftps.cert", "ftps.key", keys, logger)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build("", "", "ftps.cert", "ftps.key", keys, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificates[0].Certificate[0], second.Certificates[0].Certificate[0]) {
		t.Error("a second build generated another certificate rather than the one kept")
	}
	if strings.Contains(out.String(), "generated a temporary") {
		t.Errorf("a kept certificate is reported as temporary:\n%s", out.String())
	}
	other, err := Build("", "", "https.cert", "https.key", keys, logger)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificates[0].Certificate[0], other.Certificates[0].Certificate[0]) {
		t.Error("https was given the certificate of ftps")
	}

	// one that runs out soon is replaced, and the new one kept
	cert, key := certificateExpiring(t, time.Now().Add(expiryWarning/2))
	certPEM, _ := config.DecodeCertificate(cert)
	keyPEM, _ := config.DecodePrivateKey(key)
	if err := keys.Put("tls.ftps", keptPair{Cert: certPEM, Key: keyPEM}); err != nil {
		t.Fatal(err)
	}
	renewed, err := Build("", "", "ftps.cert", "ftps.key", keys, logger)
	if err != nil {
		t.Fatal(err)
	}
	if leaf := renewed.Certificates[0].Leaf; leaf.Subject.CommonName != "go-fs self-signed" {
		t.Errorf("a kept certificate that runs out soon was served: %s", leaf.Subject)
	}
	var kept keptPair
	if found, _ := keys.Get("tls.ftps", &kept); !found || bytes.Equal(kept.Cert, certPEM) {
		t.Error("the certificate that replaced one running out was not kept")
	}
}
