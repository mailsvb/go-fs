//go:build !light && !no_webdav

package features

// WebDAV is left out by the tag no_webdav, and by light.
func init() { enable(WebDAV) }
