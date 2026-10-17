//go:build !light && !no_cryptomator

package features

// The vaults of Cryptomator are left out by the tag no_cryptomator, and by light.
func init() { enable(Cryptomator) }
