package config

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/testpki"
)

// nbdConfig is a configuration whose NBD server is on with material that
// validates, and its CA.
func nbdConfig(t *testing.T) (Config, *testpki.CA) {
	t.Helper()
	skipWithout(t, features.NBD)
	parent := t.TempDir()
	ca := testpki.NewCA(t, "test CA")
	server := ca.Server(t, "localhost")
	cfg := Default()
	cfg.General.BaseFolderFiles = mkdir(t, parent, "files")
	cfg.General.BaseFolderDisks = mkdir(t, parent, "disks")
	cfg.NBD.Enabled = true
	cfg.NBD.Cert = testpki.Encode(server.CertPEM)
	cfg.NBD.Key = testpki.Encode(server.KeyPEM)
	cfg.NBD.ClientCA = testpki.Encode(ca.CertPEM)
	return cfg, ca
}

func TestValidateAcceptsAnNBDServer(t *testing.T) {
	cfg, ca := nbdConfig(t)
	cfg.NBD.ClientCRL = testpki.Encode(ca.CRL(t, ca.Client(t, "gone").Cert))
	cfg.NBD.AllowedClients = []string{"192.168.1.0/24", "10.0.0.7", "fd00::/8"}
	allow := true
	cfg.Users = []User{{Username: "vm", NBD: true, AllowUserFileRetrieve: &allow}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// nothing but nbd is on, which is a server all the same
	cfg.HTTP.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an nbd-only configuration is refused: %v", err)
	}
	if users := cfg.NBDUsers(); len(users) != 1 || users[0].Username != "vm" {
		t.Errorf("NBDUsers = %+v", users)
	}
	resolved := cfg.Resolved()
	if resolved.NBD.DisksFolder != cfg.General.BaseFolderDisks || resolved.HTTP.DisksFolder != cfg.General.BaseFolderDisks {
		t.Errorf("the disks folder is not handed out: nbd %q, http %q",
			resolved.NBD.DisksFolder, resolved.HTTP.DisksFolder)
	}
	if resolved.HTTP.NBDPort != 10809 {
		t.Errorf("the http server is told the nbd port is %d", resolved.HTTP.NBDPort)
	}
	cfg.NBD.Enabled = false
	if port := cfg.Resolved().HTTP.NBDPort; port != 0 {
		t.Errorf("with nbd off the http server is told port %d", port)
	}
}

func TestValidateRejectsABadNBDServer(t *testing.T) {
	other := testpki.NewCA(t, "other CA")
	cases := []struct {
		name   string
		mutate func(*Config, *testpki.CA)
		want   string
	}{
		{"no disks folder", func(c *Config, _ *testpki.CA) { c.General.BaseFolderDisks = "" }, "nbd.enabled needs general.baseFolderDisks"},
		{"relative disks folder", func(c *Config, _ *testpki.CA) { c.General.BaseFolderDisks = "disks" }, "absolute"},
		{"disks inside the files", func(c *Config, _ *testpki.CA) {
			c.General.BaseFolderDisks = mkdir(t, c.General.BaseFolderFiles, "disks")
		}, "overlaps general.baseFolderFiles"},
		{"files inside the disks", func(c *Config, _ *testpki.CA) {
			c.General.BaseFolderFiles = mkdir(t, c.General.BaseFolderDisks, "files")
		}, "overlaps"},
		{"disks are the trash", func(c *Config, _ *testpki.CA) {
			c.General.BaseFolderTrash = c.General.BaseFolderDisks
		}, "overlaps"},
		{"disks are the registry", func(c *Config, _ *testpki.CA) {
			c.General.BaseFolderRegistry = c.General.BaseFolderDisks
		}, "overlaps"},
		{"port", func(c *Config, _ *testpki.CA) { c.NBD.Port = 0 }, "nbd.port"},
		{"address", func(c *Config, _ *testpki.CA) { c.NBD.Address = "localhost" }, "nbd.address"},
		{"connections", func(c *Config, _ *testpki.CA) { c.NBD.MaxConnections = 0 }, "nbd.maxConnections"},
		{"handshake", func(c *Config, _ *testpki.CA) { c.NBD.HandshakeTimeout = 0 }, "nbd.handshakeTimeout"},
		{"keepalive", func(c *Config, _ *testpki.CA) { c.NBD.KeepAlive = 0 }, "nbd.keepAlive"},
		{"allowed clients", func(c *Config, _ *testpki.CA) { c.NBD.AllowedClients = []string{"lan"} }, "nbd.allowedClients[0]"},
		{"no certificate", func(c *Config, _ *testpki.CA) { c.NBD.Cert, c.NBD.Key = "", "" }, "nbd.cert and nbd.key are required"},
		{"a key of another certificate", func(c *Config, ca *testpki.CA) {
			c.NBD.Key = testpki.Encode(ca.Server(t, "localhost").KeyPEM)
		}, "nbd.cert and nbd.key"},
		{"no client CA", func(c *Config, _ *testpki.CA) { c.NBD.ClientCA = "" }, "nbd.clientCA is required"},
		{"a client CA that is no CA", func(c *Config, ca *testpki.CA) {
			c.NBD.ClientCA = testpki.Encode(ca.Client(t, "vm").CertPEM)
		}, "not a CA's"},
		{"a CRL of another CA", func(c *Config, _ *testpki.CA) {
			c.NBD.ClientCRL = testpki.Encode(other.CRL(t))
		}, "none of the CAs of nbd.clientCA"},
		{"a CRL that is a certificate", func(c *Config, ca *testpki.CA) {
			c.NBD.ClientCRL = testpki.Encode(ca.CertPEM)
		}, "not a certificate revocation list"},
		{"an nbd account that may not read", func(c *Config, _ *testpki.CA) {
			c.Users = []User{{Username: "vm", NBD: true}}
		}, "could never attach a disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, ca := nbdConfig(t)
			tc.mutate(&cfg, ca)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one that says %q", err, tc.want)
			}
		})
	}
}

// The Disks page needs its folder whether or not the NBD server is on, and the
// rules of the folder are the same.
func TestTheDisksFolderIsCheckedWithNBDOff(t *testing.T) {
	cfg, _ := nbdConfig(t)
	cfg.NBD.Enabled = false
	cfg.NBD.Cert, cfg.NBD.Key, cfg.NBD.ClientCA = "", "", ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the disks folder alone is refused: %v", err)
	}
	cfg.General.BaseFolderTemp = filepath.Join(cfg.General.BaseFolderDisks, "temp")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "general.baseFolderDisks") {
		t.Fatalf("a temp folder inside the disks is accepted: %v", err)
	}
}

func TestTheCAAndTheCRLAreDescribed(t *testing.T) {
	ca := testpki.NewCA(t, "lab CA")
	if got := Describe(KindCertificateAuthority, testpki.Encode(ca.CertPEM)); !strings.HasPrefix(got, "CA lab CA, expires") {
		t.Errorf("the CA is described as %q", got)
	}
	revoked := ca.Client(t, "vm").Cert
	if got := Describe(KindCRL, testpki.Encode(ca.CRL(t, revoked))); !strings.Contains(got, "lab CA, 1 revoked") {
		t.Errorf("the CRL is described as %q", got)
	}
	if _, err := Decode(KindCRL, testpki.Encode(ca.CertPEM)); err == nil {
		t.Error("a certificate is taken for a CRL")
	}
	if _, err := Decode(KindCertificateAuthority, testpki.Encode(ca.CRL(t))); err == nil {
		t.Error("a CRL is taken for a CA")
	}
}

// openssl ca -gencrl writes a version 1 list unless it is told to number
// them, which crypto/x509's current parser refuses. The fixture is such a list,
// revoking revoked.crt, with the CA that signed it; none of it is secret.
func TestAVersion1CRLIsRead(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", "crl-v1", name))
		if err != nil {
			t.Fatal(err)
		}
		return testpki.Encode(data)
	}
	authorities, err := ParseCertificateAuthorities(read("ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := ParseCRL(read("ca.crl"), authorities)
	if err != nil {
		t.Fatalf("the version 1 list is refused: %v", err)
	}
	revokedPEM, _ := DecodeCertificate(read("revoked.crt"))
	block, _ := pem.Decode(revokedPEM)
	revoked, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Serials) != 1 || list.Serials[0].Cmp(revoked.SerialNumber) != 0 {
		t.Errorf("the list revokes %v, want the serial of revoked.crt", list.Serials)
	}
	if list.Authority.Subject.CommonName != "go-fs test CA" || list.NextUpdate.IsZero() {
		t.Errorf("the list is read as %+v", list)
	}
	if got := Describe(KindCRL, read("ca.crl")); !strings.Contains(got, "go-fs test CA, 1 revoked") {
		t.Errorf("the list is described as %q", got)
	}
	if _, err := ParseCRL(read("ca.crl"), []*x509.Certificate{testpki.NewCA(t, "other").Cert}); err == nil {
		t.Error("a version 1 list of another CA is accepted")
	}
}
