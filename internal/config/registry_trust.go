package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mailsvb/go-fs/internal/imagesig"
)

// RegistryTrust is a signer of container images whose signatures the registry
// checks, [[http.registry.trust]]. The registry verifies every signature and
// attestation pushed next to an image against these, and its page says which
// trust an image's signature was verified by, or that none verified it. It
// refuses nothing: a client that cares about a signature still verifies it
// itself, with cosign, notation or a policy of its cluster.
//
// Exactly one of PublicKey, Certificates and TrustedRoot says what a
// signature has to be made with.
type RegistryTrust struct {
	// Name is what the registry page names the trust by. Letters, digits,
	// dots, dashes and underscores; unique, whatever its case.
	Name string `toml:"name"`
	// PublicKey is a PEM file with the public key of a key pair signatures
	// are made with: cosign.pub of cosign generate-key-pair, or any ECDSA,
	// Ed25519 or RSA public key. It verifies cosign signatures and
	// attestations, and Sigstore bundles made with that key.
	PublicKey string `toml:"publicKey,omitempty"`
	// Certificates is a PEM file with the root certificates of the Notary
	// Project signatures that are trusted, as notation cert add puts them
	// in its trust store.
	Certificates string `toml:"certificates,omitempty"`
	// TrustedRoot is a Sigstore trusted_root.json: the certificate
	// authorities, transparency logs and timestamp authorities keyless
	// signatures are checked against. Sigstore's public instance publishes
	// one, cosign trusted-root create writes one, and gh attestation
	// trusted-root prints GitHub's, which its attestations of private
	// repositories need. It is read as it is, never refreshed online.
	TrustedRoot string `toml:"trustedRoot,omitempty"`
	// Identity and Issuer are who a keyless signature has to be made by:
	// the identity its certificate was issued to, such as the workflow
	// https://github.com/acme/app/.github/workflows/release.yml@refs/tags/v1.0,
	// and the OIDC issuer that vouched for it, such as
	// https://token.actions.githubusercontent.com. A * in Identity stands
	// for anything, slashes included. Both are needed with TrustedRoot, and
	// neither means anything without it.
	Identity string `toml:"identity,omitempty"`
	Issuer   string `toml:"issuer,omitempty"`
	// Repositories limits the trust to the repositories it names, where a *
	// stands for anything, slashes included: "prod/*". Empty is every
	// repository.
	Repositories []string `toml:"repositories,omitempty"`
}

// validateRegistryTrust checks [[http.registry.trust]], and reads the
// files, so that a key or a root the registry cannot use is refused with the
// rest of the file rather than found out when a signature is checked.
func (c Config) validateRegistryTrust() error {
	names := map[string]bool{}
	for i, trust := range c.HTTP.RegistryTrust {
		where := fmt.Sprintf("http.registry.trust[%d]", i)
		if trust.Name == "" {
			return fmt.Errorf("%s has no name", where)
		}
		if !repositoryName.MatchString(trust.Name) {
			return fmt.Errorf("%s %q: a name is letters, digits, dots, dashes and underscores", where, trust.Name)
		}
		if names[strings.ToLower(trust.Name)] {
			return fmt.Errorf("%s: %q is configured twice", where, trust.Name)
		}
		names[strings.ToLower(trust.Name)] = true
		where = fmt.Sprintf("%s %q", where, trust.Name)

		set := 0
		for _, file := range []struct{ key, path string }{
			{"publicKey", trust.PublicKey},
			{"certificates", trust.Certificates},
			{"trustedRoot", trust.TrustedRoot},
		} {
			if file.path == "" {
				continue
			}
			set++
			if !filepath.IsAbs(file.path) {
				return fmt.Errorf("%s: %s %q has to be an absolute path", where, file.key, file.path)
			}
			info, err := os.Stat(file.path)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", where, file.key, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s: %s %q is not a file", where, file.key, file.path)
			}
		}
		if set != 1 {
			return fmt.Errorf("%s needs exactly one of publicKey, certificates and trustedRoot", where)
		}
		keyless := trust.TrustedRoot != ""
		if keyless && (trust.Identity == "" || trust.Issuer == "") {
			return fmt.Errorf("%s: a trustedRoot needs the identity and the issuer a signature has to be made by", where)
		}
		if !keyless && (trust.Identity != "" || trust.Issuer != "") {
			return fmt.Errorf("%s: identity and issuer are for a trustedRoot", where)
		}
		for k, pattern := range trust.Repositories {
			if pattern == "" {
				return fmt.Errorf("%s: repositories[%d] is empty", where, k)
			}
		}
		if _, err := imagesig.LoadTrust(trust.Name, trust.PublicKey, trust.Certificates, trust.TrustedRoot,
			trust.Identity, trust.Issuer); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}
