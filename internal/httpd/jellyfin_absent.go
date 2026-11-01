//go:build light || no_jellyfin

package httpd

import "net/http"

// A binary built without the Jellyfin music library, go-fs-light for one,
// has no jellyfin repository, so no request is one of a Jellyfin client and
// these are never reached.

func isJellyfinRequest(*http.Request) bool { return false }

func (s *Server) serveJellyfin(*settings, http.ResponseWriter, *http.Request) {}

func (s *Server) warnAboutJellyfin(*settings) {}
