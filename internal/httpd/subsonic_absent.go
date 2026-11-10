//go:build light || no_subsonic

package httpd

import (
	"log/slog"
	"net/http"

	"github.com/mailsvb/go-fs/internal/store"
)

// A binary built without the OpenSubsonic music library, go-fs-light for one,
// has no subsonic repository, so no request is one of an OpenSubsonic client
// and these are never reached.

func isSubsonicRequest(*http.Request) bool { return false }

func (s *Server) serveSubsonic(*settings, http.ResponseWriter, *http.Request) {}

func (s *Server) warnAboutSubsonic(*settings) {}

type subsonicState struct{}

func newSubsonicState(*store.Store, *slog.Logger) *subsonicState { return nil }
