//go:build !light && !no_jellyfin

package features

// The Jellyfin music library is left out by the tag no_jellyfin, and by light.
func init() { enable(Jellyfin) }
