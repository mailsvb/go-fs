//go:build !light && !no_subsonic

package features

// The OpenSubsonic music library is left out by the tag no_subsonic, and by
// light.
func init() { enable(Subsonic) }
