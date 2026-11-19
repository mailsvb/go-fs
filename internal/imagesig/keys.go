package imagesig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	_ "crypto/sha256" // register SHA-224/256 for crypto.Hash
	_ "crypto/sha512" // register SHA-384/512 for crypto.Hash
)

// errBadSignature is the error of a signature that does not verify.
var errBadSignature = errors.New("signature does not verify")

// checkKey refuses key types and sizes nobody should be trusting.
func checkKey(k crypto.PublicKey) error {
	switch k := k.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		}
		return errors.New("unsupported elliptic curve")
	case ed25519.PublicKey:
		return nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("RSA key of %d bits is too small", k.N.BitLen())
		}
		return nil
	}
	return fmt.Errorf("unsupported key type %T", k)
}

// curveHash is the hash that goes with an ECDSA curve in Sigstore.
func curveHash(k *ecdsa.PublicKey) crypto.Hash {
	switch k.Curve {
	case elliptic.P384():
		return crypto.SHA384
	case elliptic.P521():
		return crypto.SHA512
	}
	return crypto.SHA256
}

// verifyMessage checks a cosign or Sigstore signature over msg the way
// sigstore's verifiers do: ECDSA in ASN.1 over the curve's hash (or
// SHA-256, which cosign uses for every curve), ed25519 over the message
// itself, RSA PKCS #1 v1.5 or PSS over SHA-256.
func verifyMessage(pub crypto.PublicKey, msg, sig []byte) error {
	if err := checkKey(pub); err != nil {
		return err
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		if ed25519.Verify(k, msg, sig) {
			return nil
		}
		return errBadSignature
	case *ecdsa.PublicKey:
		hashes := []crypto.Hash{curveHash(k)}
		if hashes[0] != crypto.SHA256 {
			hashes = append(hashes, crypto.SHA256)
		}
		for _, h := range hashes {
			if verifyDigest(pub, h, digestOf(h, msg), sig) == nil {
				return nil
			}
		}
		return errBadSignature
	}
	return verifyDigest(pub, crypto.SHA256, digestOf(crypto.SHA256, msg), sig)
}

// verifyDigest checks a signature over a message of which only the digest
// is known. ed25519 signs the message itself, so it cannot be checked here.
func verifyDigest(pub crypto.PublicKey, h crypto.Hash, dgst, sig []byte) error {
	if err := checkKey(pub); err != nil {
		return err
	}
	if len(dgst) != h.Size() {
		return fmt.Errorf("digest of %d bytes is not %v", len(dgst), h)
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if ecdsa.VerifyASN1(k, dgst, sig) {
			return nil
		}
		return errBadSignature
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(k, h, dgst, sig) == nil {
			return nil
		}
		if rsa.VerifyPSS(k, h, dgst, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto}) == nil {
			return nil
		}
		return errBadSignature
	case ed25519.PublicKey:
		return errors.New("an ed25519 signature over an artifact can only be checked with the artifact itself")
	}
	return fmt.Errorf("unsupported key type %T", pub)
}

func digestOf(h crypto.Hash, b []byte) []byte {
	w := h.New()
	w.Write(b)
	return w.Sum(nil)
}

// decodeB64 decodes standard or URL-safe base64, padded or not, as
// protobuf JSON and the various signers write it.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// pae is the DSSE pre-authentication encoding, what DSSE signatures sign.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}
