package admin

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"go-fs/internal/secrets"
)

// realm is what the browser shows in its password dialog.
const realm = "go-fs admin"

// failureDelay slows down guessing, as loginFailureDelay does for the other
// servers. There is one account here and no key of its own for it.
const failureDelay = time.Second

// authenticate checks the single account of the interface.
//
// Basic is enough and Digest is not offered: there is one account, the listener
// is always TLS, and Basic is what a browser and a fetch both send without
// help. The comparison is constant time, as everywhere else.
func (s *Server) authenticate(set *settings, w http.ResponseWriter, r *http.Request) bool {
	name, password, ok := parseBasic(r.Header.Get("Authorization"))
	if ok && secrets.Match(name, set.cfg.AdminUsername) &&
		secrets.Match(password, set.cfg.AdminPassword) {
		return true
	}

	if ok {
		s.log.Warn("the admin interface refused a login", "user", name, "address", addressOf(r))
	}
	time.Sleep(failureDelay)
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return false
}

// parseBasic splits an RFC 7617 header into its two halves.
func parseBasic(header string) (name, password string, ok bool) {
	if !strings.HasPrefix(header, "Basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return "", "", false
	}
	name, password, found := strings.Cut(string(decoded), ":")
	return name, password, found
}
