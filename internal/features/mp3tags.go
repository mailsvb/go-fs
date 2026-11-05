//go:build !light && !no_mp3tags

package features

// The MP3 tag editor of the file listing is left out by the tag no_mp3tags,
// and by light.
func init() { enable(MP3Tags) }
