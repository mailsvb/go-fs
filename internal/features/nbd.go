//go:build !light && !no_nbd

package features

// NBD is left out by the tag no_nbd, and by light.
func init() { enable(NBD) }
