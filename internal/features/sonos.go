//go:build !light && !no_sonos

package features

// Playing files on Sonos speakers from the file listing is left out by the
// tag no_sonos, and by light.
func init() { enable(Sonos) }
