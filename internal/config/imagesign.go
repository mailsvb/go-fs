package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/imagesig"
)

// ImageSign is a signer of container images that signs them in their labels,
// [[http.services.imagesign]]: the image's configuration carries an Ed25519
// signature over its layers and the certificate it was made with, under
// <prefix>.signature and <prefix>.certchain, as image-sign.sh of
// container-image-validation writes them. The registry checks such an image
// as it is pushed and as it is pulled, and its mode says what it refuses.
type ImageSign struct {
	// Name is what the registry page and the log name the signer by.
	// Letters, digits, dots, dashes and underscores; unique, whatever its
	// case.
	Name string `toml:"name"`
	// LabelPrefix are the reverse-DNS prefixes the signature labels may be
	// under, such as org.mitel.imagesign for org.mitel.imagesign.signature
	// and org.mitel.imagesign.certchain.
	LabelPrefix []string `toml:"labelPrefix"`
	// Certificates are what a signature is checked against, as PEM pasted
	// in, several blocks one after the other: CA certificates, root or
	// intermediate, that the signing certificate has to chain to, and
	// Ed25519 public keys, PUBLIC KEY blocks, that are trusted as they are,
	// whoever issued the certificate that carries them and whenever it
	// expires. Base64 of the PEM works too.
	Certificates string `toml:"certificates"`
	// Mode is what the registry refuses. optional refuses nothing and only
	// shows whether an image is signed. mandatory refuses to take or hand
	// out an image whose signature is there but does not verify, and lets an
	// image without one through. enforced refuses every image that does not
	// carry a signature that verifies. Empty is optional.
	Mode string `toml:"mode" choices:"optional,mandatory,enforced"`
	// Repositories limits the signer to the repositories it names, where a
	// * stands for anything, slashes included: "prod/*". Empty is every
	// repository.
	Repositories []string `toml:"repositories,omitempty"`
}

// The modes of [[http.services.imagesign]].
const (
	ImageSignOptional  = "optional"
	ImageSignMandatory = "mandatory"
	ImageSignEnforced  = "enforced"
)

// ImageSignMode is the mode of an entry as the registry applies it: in lower
// case, and optional where none is set.
func (s ImageSign) ImageSignMode() string {
	mode := strings.ToLower(strings.TrimSpace(s.Mode))
	if mode == "" {
		return ImageSignOptional
	}
	return mode
}

// KindImageSignTrust is the key material of http.services.imagesign's
// certificates: CA certificates and Ed25519 public keys, in PEM.
const KindImageSignTrust = "imagesigntrust"

// labelPrefixPattern is a reverse-DNS name, as a label key is one.
var labelPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

// LoadImageSign reads an entry into what the registry checks against.
func LoadImageSign(s ImageSign) (*imagesig.LabelTrust, error) {
	material, err := DecodeImageSignTrust(s.Certificates)
	if err != nil {
		return nil, fmt.Errorf("certificates %w", err)
	}
	return imagesig.NewLabelTrust(s.Name, s.LabelPrefix, material)
}

// DecodeImageSignTrust checks the certificates of an entry and returns their
// PEM.
func DecodeImageSignTrust(value string) ([]byte, error) {
	decoded, err := DecodePEM(value)
	if err != nil {
		return nil, err
	}
	if _, _, err := imagesig.ParseLabelTrustMaterial(decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// describeImageSignTrust is the line the web interface shows under the
// certificates of an entry.
func describeImageSignTrust(value string) string {
	decoded, err := DecodePEM(value)
	if err != nil {
		return "this value " + err.Error()
	}
	roots, keys, err := imagesig.ParseLabelTrustMaterial(decoded)
	if err != nil {
		return "this value " + err.Error()
	}
	var parts []string
	for _, root := range roots {
		name := fmt.Sprintf("CA %s, expires %s", subjectName(root), root.NotAfter.Format(time.DateOnly))
		if time.Now().After(root.NotAfter) {
			name += " (expired)"
		}
		parts = append(parts, name)
	}
	for _, key := range keys {
		parts = append(parts, "Ed25519 key "+imagesig.KeyFingerprint(key))
	}
	return strings.Join(parts, "; ")
}

// validateImageSign checks [[http.services.imagesign]], reading the
// certificates, so that a signer the registry cannot use is refused with the
// rest of the file.
func (c Config) validateImageSign() error {
	names := map[string]bool{}
	for i, entry := range c.HTTP.ImageSign {
		where := fmt.Sprintf("http.services.imagesign[%d]", i)
		if entry.Name == "" {
			return fmt.Errorf("%s has no name", where)
		}
		if !repositoryName.MatchString(entry.Name) {
			return fmt.Errorf("%s %q: a name is letters, digits, dots, dashes and underscores", where, entry.Name)
		}
		if names[strings.ToLower(entry.Name)] {
			return fmt.Errorf("%s: %q is configured twice", where, entry.Name)
		}
		names[strings.ToLower(entry.Name)] = true
		where = fmt.Sprintf("%s %q", where, entry.Name)

		if len(entry.LabelPrefix) == 0 {
			return fmt.Errorf("%s needs a labelPrefix, such as org.mitel.imagesign", where)
		}
		prefixes := map[string]bool{}
		for k, prefix := range entry.LabelPrefix {
			if !labelPrefixPattern.MatchString(prefix) {
				return fmt.Errorf("%s: labelPrefix[%d] %q is no reverse-DNS name, such as org.mitel.imagesign", where, k, prefix)
			}
			if prefixes[strings.ToLower(prefix)] {
				return fmt.Errorf("%s: labelPrefix %q is listed twice", where, prefix)
			}
			prefixes[strings.ToLower(prefix)] = true
		}
		if strings.TrimSpace(entry.Certificates) == "" {
			return fmt.Errorf("%s needs certificates: the PEM of the CA certificates or Ed25519 public keys signatures are checked against", where)
		}
		if _, err := DecodeImageSignTrust(entry.Certificates); err != nil {
			return fmt.Errorf("%s: certificates %w", where, err)
		}
		switch entry.ImageSignMode() {
		case ImageSignOptional, ImageSignMandatory, ImageSignEnforced:
		default:
			return fmt.Errorf("%s: mode %q is none of optional, mandatory and enforced", where, entry.Mode)
		}
		for k, pattern := range entry.Repositories {
			if pattern == "" {
				return fmt.Errorf("%s: repositories[%d] is empty", where, k)
			}
		}
	}
	return nil
}
