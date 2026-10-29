//go:build light || no_webdav

package httpd

import (
	"net/http"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// A binary built without WebDAV, go-fs-light for one, keeps none of
// golang.org/x/net: no request is WebDAV, so http.webdav changes nothing and
// these are never reached.

type davState struct{}

func newDAVState() davState { return davState{} }

func davRequest(*settings, *http.Request) bool { return false }

func davBasic(*settings, *http.Request) bool { return false }

func (s *Server) serveDAV(*settings, http.ResponseWriter, *http.Request, vfs.Target, *account) {}

func (s *Server) davHold(*settings, http.ResponseWriter, ...string) (func(), bool) {
	return func() {}, true
}
