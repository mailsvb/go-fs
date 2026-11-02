//go:build !light && !no_sftpd

package sftp

import (
	"fmt"
	"log/slog"

	"golang.org/x/crypto/ssh"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

// hostKey returns the signer the SSH server identifies itself with.
//
// As with the TLS certificate of the FTPS listener, a key that is not
// configured is generated, which keeps the server usable without any setup.
// The generated key is kept in keys, so that it stays the same across a
// restart; with keys nil it lasts this run only, and every client that
// remembers host keys reports the change. The warning says which, and the
// configuration documents how to produce a stable value.
//
// A generated key is made as the value the file would hold and then read back,
// so that a generated key and a configured one go down the same path.
func hostKey(cfg config.SFTP, keys *store.Bucket, logger *slog.Logger) (ssh.Signer, error) {
	if cfg.HostKey != "" {
		signer, err := parseHostKey(cfg.HostKey)
		if err != nil {
			return nil, fmt.Errorf("sftp.hostkey: %w", err)
		}
		// the fingerprint is what a client shows when it asks whether to
		// trust the host, and what it reports when the key changed, so it is
		// said once so that either can be checked against the log
		logger.Info("sftp host key loaded", "type", signer.PublicKey().Type(),
			"fingerprint", ssh.FingerprintSHA256(signer.PublicKey()))
		return signer, nil
	}

	var generated string
	_, err := keys.Remembered(hostKeyRecord, &generated, func() (err error) {
		generated, err = config.GenerateHostKey()
		return err
	})
	var signer ssh.Signer
	if err == nil {
		signer, err = parseHostKey(generated)
	}
	if err != nil {
		// a kept key that cannot be read is replaced by a new one, which is
		// kept where it can be and used for this run where it cannot,
		// rather than keeping the server down
		logger.Warn("cannot use the kept sftp host key, so a new one is generated", "error", err)
		if generated, err = config.GenerateHostKey(); err != nil {
			return nil, fmt.Errorf("sftp: cannot generate a host key: %w", err)
		}
		if signer, err = parseHostKey(generated); err != nil {
			return nil, fmt.Errorf("sftp: cannot generate a host key: %w", err)
		}
		if err := keys.Put(hostKeyRecord, generated); err != nil {
			logger.Warn("cannot keep the generated sftp host key", "error", err)
			keys = nil
		}
	}
	attrs := []any{"type", signer.PublicKey().Type(), "fingerprint", ssh.FingerprintSHA256(signer.PublicKey())}
	if keys == nil {
		logger.Warn("no sftp.hostkey configured, generated a temporary host key; "+
			"it changes on every restart, so clients will report a changed host key", attrs...)
	} else {
		logger.Warn("no sftp.hostkey configured, using a host key generated and kept in "+
			"the persistent data file; configure one to keep it when that file is lost", attrs...)
	}
	return signer, nil
}

// hostKeyRecord is the record of the keys bucket the generated host key is
// kept in.
const hostKeyRecord = "sftp.hostKey"

// parseHostKey turns a configured hostkey value into a signer. config.DecodeHostKey
// reports which of the things that can be wrong with the value is wrong,
// because "invalid key" on its own does not tell a truncated paste from a
// passphrase protected key.
func parseHostKey(value string) (ssh.Signer, error) {
	decoded, err := config.DecodeHostKey(value)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(decoded)
}
