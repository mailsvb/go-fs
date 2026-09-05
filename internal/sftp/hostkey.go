package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/ssh"

	"go-fs/internal/config"
)

// hostKey returns the signer the SSH server identifies itself with.
//
// As with the TLS certificate of the FTPS listener, a key that is not
// configured is generated for this run. That keeps the server usable without
// any setup, at the cost of a key that changes on every restart: every client
// that remembers host keys reports the change. The warning says so, and the
// configuration documents how to produce a stable value.
func hostKey(cfg config.SFTP, logger *slog.Logger) (ssh.Signer, error) {
	if cfg.HostKey != "" {
		signer, err := parseHostKey(cfg.HostKey)
		if err != nil {
			return nil, fmt.Errorf("sftp.hostkey: %w", err)
		}
		return signer, nil
	}

	signer, err := generateHostKey()
	if err != nil {
		return nil, fmt.Errorf("sftp: cannot generate a host key: %w", err)
	}
	logger.Warn("no sftp.hostkey configured, generated a temporary host key; "+
		"it changes on every restart, so clients will report a changed host key",
		"fingerprint", ssh.FingerprintSHA256(signer.PublicKey()))
	return signer, nil
}

// parseHostKey turns a configured hostkey value into a signer. It reports
// which of the three things went wrong, because "invalid key" on its own does
// not tell a truncated paste from a passphrase protected key.
func parseHostKey(value string) (ssh.Signer, error) {
	decoded, err := config.DecodeHostKey(value)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(decoded)
	if err != nil {
		var passphraseNeeded *ssh.PassphraseMissingError
		if errors.As(err, &passphraseNeeded) {
			return nil, errors.New("is protected by a passphrase, which is not supported")
		}
		return nil, fmt.Errorf("cannot be parsed: %w", err)
	}
	return signer, nil
}

// generateHostKey creates an ed25519 host key for this run.
func generateHostKey() (ssh.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}
