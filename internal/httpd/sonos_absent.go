//go:build light || no_sonos

package httpd

import (
	"net/http"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// A binary built without playing on Sonos speakers, go-fs-light for one,
// never offers it, so these are never reached.

func sonosAction(*http.Request) bool { return false }

func sonosOn(*settings) bool { return false }

func (s *Server) sonosFile(*settings, *http.Request, vfs.Target) bool { return false }

func (s *Server) handleSonos(*settings, http.ResponseWriter, *http.Request, vfs.Target, credential) {}

func (s *Server) sonosSessionOf(credential) string { return "" }

type sonosState struct{}
