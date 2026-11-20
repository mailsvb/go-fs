package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/testpki"
)

func ed25519PEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestImageSign(t *testing.T) {
	folder := t.TempDir()
	ca := testpki.NewCA(t, "oci-signing-ca")
	key := ed25519PEM(t)
	pasted := string(ca.CertPEM) + key
	good := []ImageSign{
		{Name: "mitel", LabelPrefix: []string{"org.mitel.imagesign", "com.example.sign"}, Certificates: pasted,
			Mode: "Enforced", Repositories: []string{"prod/*"}},
		{Name: "other", LabelPrefix: []string{"org.example.imagesign"}, Certificates: testpki.Encode(ca.CertPEM)},
	}
	valid := servingConfig(folder)
	valid.HTTP.ImageSign = good
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid entry is refused: %v", err)
	}
	if mode := good[0].ImageSignMode(); mode != ImageSignEnforced {
		t.Errorf("mode %q", mode)
	}
	if mode := good[1].ImageSignMode(); mode != ImageSignOptional {
		t.Errorf("no mode is %q, want optional", mode)
	}
	if got := Describe(KindImageSignTrust, pasted); !strings.Contains(got, "CA oci-signing-ca") || !strings.Contains(got, "Ed25519 key sha256:") {
		t.Errorf("describe = %q", got)
	}
	if trust, err := LoadImageSign(good[0]); err != nil || trust.Roots == nil || len(trust.Keys) != 1 || len(trust.Prefixes) != 2 {
		t.Errorf("load = %+v, %v", trust, err)
	}

	cases := []struct {
		name   string
		change func(*ImageSign)
		want   string
	}{
		{"no name", func(s *ImageSign) { s.Name = "" }, "has no name"},
		{"a space in the name", func(s *ImageSign) { s.Name = "my signer" }, "letters, digits"},
		{"the name of the other", func(s *ImageSign) { s.Name = "OTHER" }, "configured twice"},
		{"no prefix", func(s *ImageSign) { s.LabelPrefix = nil }, "needs a labelPrefix"},
		{"a prefix without a dot", func(s *ImageSign) { s.LabelPrefix = []string{"imagesign"} }, "no reverse-DNS name"},
		{"a prefix with a space", func(s *ImageSign) { s.LabelPrefix = []string{"org.mitel image"} }, "no reverse-DNS name"},
		{"a prefix twice", func(s *ImageSign) { s.LabelPrefix = []string{"a.b", "A.B"} }, "listed twice"},
		{"no certificates", func(s *ImageSign) { s.Certificates = " " }, "needs certificates"},
		{"no certificates, the chain verified", func(s *ImageSign) { s.Certificates, s.VerifyChain = "", ptr(true) }, "needs certificates"},
		{"garbage certificates without the chain", func(s *ImageSign) { s.Certificates, s.VerifyChain = "junk", ptr(false) }, "certificates"},
		{"garbage certificates", func(s *ImageSign) { s.Certificates = "not a certificate" }, "certificates"},
		{"a leaf for a CA", func(s *ImageSign) { s.Certificates = string(ca.Server(t, "x").CertPEM) }, "not a CA's"},
		{"an unknown mode", func(s *ImageSign) { s.Mode = "strict" }, "none of optional, mandatory and enforced"},
		{"an empty repository", func(s *ImageSign) { s.Repositories = []string{""} }, "repositories[0] is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := servingConfig(folder)
			entry := good[0]
			c.change(&entry)
			cfg.HTTP.ImageSign = []ImageSign{entry, good[1]}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if err != nil && !strings.Contains(err.Error(), "http.registry.imagesign[") {
				t.Errorf("the error does not say where: %v", err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestImageSignWithoutChain(t *testing.T) {
	folder := t.TempDir()
	key := ed25519PEM(t)
	ca := testpki.NewCA(t, "oci-signing-ca")
	for _, entry := range []ImageSign{
		{Name: "any", LabelPrefix: []string{"org.mitel.imagesign"}, VerifyChain: ptr(false), Mode: "enforced"},
		{Name: "keyed", LabelPrefix: []string{"org.mitel.imagesign"}, VerifyChain: ptr(false), Certificates: key + string(ca.CertPEM)},
	} {
		cfg := servingConfig(folder)
		cfg.HTTP.ImageSign = []ImageSign{entry}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", entry.Name, err)
		}
		trust, err := LoadImageSign(entry)
		if err != nil || trust.VerifyChain || entry.ChainVerified() {
			t.Fatalf("%s: load = %+v, %v", entry.Name, trust, err)
		}
		if entry.Certificates != "" && len(trust.Keys) != 1 {
			t.Errorf("%s: keys = %d", entry.Name, len(trust.Keys))
		}
	}
	if !(ImageSign{}).ChainVerified() || !(ImageSign{VerifyChain: ptr(true)}).ChainVerified() {
		t.Error("the chain is verified unless verifyChain is false")
	}
	trust, err := LoadImageSign(ImageSign{Name: "x", LabelPrefix: []string{"a.b"}, Certificates: key})
	if err != nil || !trust.VerifyChain {
		t.Errorf("unset verifyChain: %+v, %v", trust, err)
	}
}

func TestImageSignRoundTrips(t *testing.T) {
	ca := testpki.NewCA(t, "oci-signing-ca")
	file := []byte("[general]\nbaseFolderFiles = \"/x\"\n\n[http.registry]\nanonymousRead = false\n\n" +
		"[[http.registry.imagesign]]\nname = \"mitel\"\nlabelPrefix = [\"org.mitel.imagesign\"]\n" +
		"certificates = '''\n" + string(ca.CertPEM) + "'''\nmode = \"mandatory\"\nrepositories = [\"prod/*\"]\n" +
		"\n[[http.registry.imagesign]]\nname = \"bare\"\nlabelPrefix = [\"org.example.sign\"]\nverifyChain = false\n")
	cfg, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.RegistryAnonymousRead {
		t.Error("http.registry.anonymousRead = false is not read")
	}
	signers := cfg.HTTP.ImageSign
	if len(signers) != 2 || signers[1].ChainVerified() || !signers[0].ChainVerified() {
		t.Fatalf("verifyChain = %+v", signers)
	}
	signers = signers[:1]
	if len(signers) != 1 || signers[0].Name != "mitel" || signers[0].ImageSignMode() != ImageSignMandatory ||
		!strings.HasPrefix(signers[0].Certificates, "-----BEGIN CERTIFICATE-----") || len(signers[0].Repositories) != 1 {
		t.Fatalf("imagesign = %+v", signers)
	}
	if _, err := LoadImageSign(signers[0]); err != nil {
		t.Fatalf("the PEM of a multi-line string: %v", err)
	}
}
