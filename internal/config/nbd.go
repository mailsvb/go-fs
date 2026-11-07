package config

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/features"
)

// The material of the NBD server that is neither a certificate it shows nor
// its key: the CA a client certificate has to come from, and that CA's list
// of revoked certificates. Both are uploaded, never generated, since go-fs
// issues no certificate of its own.
const (
	KindCertificateAuthority = "cacertificate"
	KindCRL                  = "crl"
)

// disksFolder is general.baseFolderDisks to a binary that has the Disks page,
// and nothing to one built without it, which leaves the folder alone.
func (c Config) disksFolder() string {
	if !features.Has(features.NBD) {
		return ""
	}
	return c.General.BaseFolderDisks
}

// validateDisks checks general.baseFolderDisks. An image is a disk a VM is
// running on, so no server but the NBD one may write to it and no listing may
// show it: the folder has to be one of its own. The NBD server serves nothing
// else, so it needs one.
func (c Config) validateDisks() error {
	folder := c.disksFolder()
	if folder == "" {
		if c.nbdOn() {
			return errors.New("nbd.enabled needs general.baseFolderDisks, the folder of the disk images it serves")
		}
		return nil
	}
	if !filepath.IsAbs(folder) {
		return fmt.Errorf("general.baseFolderDisks %q has to be an absolute path", folder)
	}
	if err := checkFolder("general.baseFolderDisks", folder); err != nil {
		return err
	}
	others := []struct{ name, folder string }{
		{"general.baseFolderFiles", c.General.BaseFolderFiles},
		{"general.baseFolderRegistry", c.General.BaseFolderRegistry},
		{"general.baseFolderTemp", c.General.BaseFolderTemp},
		{"general.baseFolderTrash", c.General.BaseFolderTrash},
	}
	for i, user := range c.Users {
		others = append(others, struct{ name, folder string }{
			fmt.Sprintf("users[%d].basefolder", i), user.Basefolder})
	}
	disks := comparablePath(folder)
	for _, other := range others {
		if other.folder == "" {
			continue
		}
		if overlaps(disks, comparablePath(other.folder)) {
			return fmt.Errorf("general.baseFolderDisks %q overlaps %s %q; "+
				"it has to be a folder of its own", folder, other.name, other.folder)
		}
	}
	return nil
}

// validateNBD checks [nbd]. The key material is decoded and matched here, so
// that -check and the admin interface refuse what would stop the server at
// its next start.
func (c Config) validateNBD() error {
	n := c.NBD
	if err := checkPort("nbd.port", n.Port); err != nil {
		return err
	}
	if err := checkAddress("nbd.address", n.Address); err != nil {
		return err
	}
	if n.MaxConnections < 1 {
		return errors.New("nbd.maxConnections has to be at least 1")
	}
	if n.HandshakeTimeout < 1 {
		return errors.New("nbd.handshakeTimeout has to be at least 1")
	}
	// without keepalive a client that vanished would hold its image until
	// the server restarts
	if n.KeepAlive < 1 {
		return errors.New("nbd.keepAlive has to be at least 1")
	}
	for i, entry := range n.AllowedClients {
		if _, err := ParseProxy(entry); err != nil {
			return fmt.Errorf("nbd.allowedClients[%d] %q is neither an address nor a CIDR range", i, entry)
		}
	}
	if n.Cert == "" || n.Key == "" {
		return errors.New("nbd.cert and nbd.key are required: QEMU checks the server's certificate " +
			"against the CA it is given, so it has to be issued by a CA of your own")
	}
	if err := checkPair("nbd.cert", n.Cert, "nbd.key", n.Key); err != nil {
		return err
	}
	if n.ClientCA == "" {
		return errors.New("nbd.clientCA is required: it is the CA a client certificate has to be issued by")
	}
	authorities, err := ParseCertificateAuthorities(n.ClientCA)
	if err != nil {
		return fmt.Errorf("nbd.clientCA: %w", err)
	}
	if n.ClientCRL != "" {
		if _, err := ParseCRL(n.ClientCRL, authorities); err != nil {
			return fmt.Errorf("nbd.clientCRL: %w", err)
		}
	}
	return nil
}

// validateNBDUser checks what an account that sets nbd needs. Its username is
// what a certificate's common name is matched against, and its rights are
// the only ones that decide what it may do with an image.
func validateNBDUser(where string, user User) error {
	if !boolOr(user.AllowUserFileRetrieve, false) {
		return fmt.Errorf("%s %q sets nbd but not allowUserFileRetrieve, "+
			"so it could never attach a disk", where, user.Username)
	}
	return nil
}

// DecodeCertificateAuthority decodes a configured CA bundle: one certificate
// or several, every one of them a CA's. A server certificate in the bundle
// would be trusted as an issuer of client certificates, and a client
// certificate would let itself in, so either is refused.
func DecodeCertificateAuthority(value string) ([]byte, error) {
	decoded, err := DecodeCertificate(value)
	if err != nil {
		return nil, err
	}
	for rest := decoded; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("cannot be parsed: %w", err)
		}
		if !certificate.BasicConstraintsValid || !certificate.IsCA {
			return nil, fmt.Errorf("holds the certificate of %q, which is not a CA's", subjectName(certificate))
		}
	}
	return decoded, nil
}

// ParseCertificateAuthorities is DecodeCertificateAuthority with the
// certificates parsed, which is what the NBD server verifies against.
func ParseCertificateAuthorities(value string) ([]*x509.Certificate, error) {
	decoded, err := DecodeCertificateAuthority(value)
	if err != nil {
		return nil, err
	}
	var authorities []*x509.Certificate
	for rest := decoded; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("cannot be parsed: %w", err)
		}
		authorities = append(authorities, certificate)
	}
	return authorities, nil
}

// DecodeCRL decodes a configured certificate revocation list, PEM of the kind
// openssl ca -gencrl writes, of either version: openssl writes a version 1
// list unless it is told to number them.
func DecodeCRL(value string) ([]byte, error) {
	decoded, err := DecodePEM(value)
	if err != nil {
		return nil, err
	}
	if _, err := readCRL(decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// RevocationList is what go-fs takes from a CRL: the serials it revokes, of
// the CA that signed it.
type RevocationList struct {
	// Authority is the CA of nbd.clientCA that signed it.
	Authority *x509.Certificate
	Serials   []*big.Int
	// ThisUpdate is when it was issued, and NextUpdate when the CA said it
	// would issue the next one, zero when it did not say.
	ThisUpdate, NextUpdate time.Time
}

// ParseCRL decodes a configured revocation list and checks that one of
// authorities signed it: a list from another CA revokes nothing of this one,
// which is a mistake worth hearing about rather than a list to ignore.
func ParseCRL(value string, authorities []*x509.Certificate) (*RevocationList, error) {
	decoded, err := DecodePEM(value)
	if err != nil {
		return nil, err
	}
	list, err := readCRL(decoded)
	if err != nil {
		return nil, err
	}
	for _, authority := range authorities {
		if list.signedBy(authority) {
			this, next := list.times()
			return &RevocationList{Authority: authority, Serials: list.serials(), ThisUpdate: this, NextUpdate: next}, nil
		}
	}
	return nil, fmt.Errorf("is signed by %q, which is none of the CAs of nbd.clientCA", list.issuer())
}

// crl is a parsed revocation list. crypto/x509 reads version 2 lists alone
// with its current parser; its older one reads both, and is what a version 1
// list falls back to.
type crl struct {
	current *x509.RevocationList
	legacy  *pkix.CertificateList
}

func readCRL(pemBytes []byte) (crl, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return crl{}, errors.New("does not decode to a PEM block")
	}
	if block.Type != "X509 CRL" {
		return crl{}, fmt.Errorf("holds %s, not a certificate revocation list", describeBlock(block.Type))
	}
	current, err := x509.ParseRevocationList(block.Bytes)
	if err == nil {
		return crl{current: current}, nil
	}
	//lint:ignore SA1019 the only parser of crypto/x509 that reads a version 1 list
	legacy, legacyErr := x509.ParseDERCRL(block.Bytes)
	if legacyErr != nil {
		return crl{}, fmt.Errorf("cannot be parsed: %w", err)
	}
	return crl{legacy: legacy}, nil
}

func (l crl) signedBy(authority *x509.Certificate) bool {
	if l.current != nil {
		return l.current.CheckSignatureFrom(authority) == nil
	}
	//lint:ignore SA1019 the check that goes with the parser of a version 1 list
	return authority.CheckCRLSignature(l.legacy) == nil
}

func (l crl) serials() []*big.Int {
	var serials []*big.Int
	if l.current != nil {
		for _, entry := range l.current.RevokedCertificateEntries {
			serials = append(serials, entry.SerialNumber)
		}
		return serials
	}
	for _, entry := range l.legacy.TBSCertList.RevokedCertificates {
		serials = append(serials, entry.SerialNumber)
	}
	return serials
}

func (l crl) times() (this, next time.Time) {
	if l.current != nil {
		return l.current.ThisUpdate, l.current.NextUpdate
	}
	return l.legacy.TBSCertList.ThisUpdate, l.legacy.TBSCertList.NextUpdate
}

func (l crl) issuer() string {
	var name pkix.Name
	if l.current != nil {
		name = l.current.Issuer
	} else {
		name.FillFromRDNSequence(&l.legacy.TBSCertList.Issuer)
	}
	if name.CommonName != "" {
		return name.CommonName
	}
	return name.String()
}

func describeCertificateAuthority(value string) string {
	authorities, err := ParseCertificateAuthorities(value)
	if err != nil {
		return "this value " + err.Error()
	}
	names := make([]string, 0, len(authorities))
	for _, authority := range authorities {
		name := fmt.Sprintf("%s, expires %s", subjectName(authority), authority.NotAfter.Format(time.DateOnly))
		if time.Now().After(authority.NotAfter) {
			name += " (expired)"
		}
		names = append(names, name)
	}
	if len(names) == 1 {
		return "CA " + names[0]
	}
	return fmt.Sprintf("%d CAs: %s", len(names), strings.Join(names, "; "))
}

func describeCRL(value string) string {
	decoded, err := DecodePEM(value)
	if err != nil {
		return "this value " + err.Error()
	}
	list, err := readCRL(decoded)
	if err != nil {
		return "this value " + err.Error()
	}
	this, next := list.times()
	summary := fmt.Sprintf("revocation list of %s, %d revoked, issued %s",
		list.issuer(), len(list.serials()), this.Format(time.DateOnly))
	if !next.IsZero() {
		summary += ", next update " + next.Format(time.DateOnly)
		if time.Now().After(next) {
			summary += " (overdue)"
		}
	}
	return summary
}

// subjectName is what a certificate is called in a message.
func subjectName(certificate *x509.Certificate) string {
	if certificate.Subject.CommonName != "" {
		return certificate.Subject.CommonName
	}
	return certificate.Subject.String()
}
