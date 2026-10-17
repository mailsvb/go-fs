//go:build light || no_cryptomator

package remote

import "errors"

// A binary built without the vaults of Cryptomator, go-fs-light for one,
// keeps none of package cryptomator: checkedVault refuses a login that names
// a vault, so that nothing on a host is ever read or written beside the vault
// it was meant to go into, and these are never reached.

// ErrWrongVaultPassword is never returned by a binary built without vaults.
var ErrWrongVaultPassword = errors.New("the vault password is wrong")

func openVault(inner FS, root, password string) (FS, error) {
	_ = inner.Close()
	return nil, errNoVaults
}

func createVault(inner FS, root, password string) (string, error) {
	return "", errNoVaults
}
