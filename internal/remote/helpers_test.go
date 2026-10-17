package remote_test

import "github.com/mailsvb/go-fs/internal/config"

// sshConfig is the SSH transport of a default configuration.
func sshConfig() config.SSH {
	return config.Default().General.SSH
}
