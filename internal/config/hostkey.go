package config

import (
	"encoding/base64"
	"errors"
	"strings"
)

// DecodeHostKey turns the configured sftp.hostkey value into the PEM bytes of
// a private key.
//
// The value is base64 of the PEM encoding, which keeps a key on one line of
// TOML. A value that is already PEM is passed through, so that a key pasted
// into a multi-line string works instead of failing as bad base64.
func DecodeHostKey(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, errors.New("is empty")
	}
	if strings.HasPrefix(trimmed, "-----BEGIN") {
		return []byte(trimmed), nil
	}
	// tolerate the line breaks a base64 tool leaves behind
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(trimmed)
	pem, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, errors.New("is neither base64 nor PEM")
	}
	if !strings.HasPrefix(string(pem), "-----BEGIN") {
		return nil, errors.New("does not decode to a PEM private key")
	}
	return pem, nil
}
