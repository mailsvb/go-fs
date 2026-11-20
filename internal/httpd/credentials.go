package httpd

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/webui"
)

// The Credentials overlay lets whoever is signed in change their own
// password. It is on every page with the session menu, and on a page of its
// own, the Profile page, which the start page offers so that an account that
// may use nothing else here — one that sets ftp or sftp alone, say — can log
// in and change its password all the same.
//
// The password lives in the configuration file, so changing it rewrites that
// one line of the file (see config.SetPassword) and has the file applied at
// once, whether or not it is watched, so that the new password works on every
// server right away. The session is issued again for the new password: a
// session is bound to the credentials it was issued for (see issuedFor), so
// every other browser signed in to the account is signed out by the change,
// and this one would be too.

const (
	// actionProfile is the Profile page. It is named in session.go's
	// lists as well, since a login returns to it.
	actionProfile  = "profile"
	actionPassword = "password"
)

// minPassword is the shortest new password the overlay accepts.
const minPassword = 8

// reloadWait is how long a password change waits for the configuration to be
// applied. It is bounded because applying it may restart this very server,
// which waits for the request asking for it to finish.
const reloadWait = 10 * time.Second

// profileAction reports which of the two endpoints a request is for, or "".
func profileAction(r *http.Request) string {
	switch action := r.URL.Query().Get(sessionParam); action {
	case actionProfile, actionPassword:
		return action
	default:
		return ""
	}
}

// profileURL is the Profile page.
func profileURL() string {
	return "/?" + sessionParam + "=" + actionProfile
}

// passwordURL is where the overlay posts to.
func passwordURL() string {
	return "/?" + sessionParam + "=" + actionPassword
}

// SetReloader hands over what applies the configuration file to every running
// server; the supervisor does, every time it builds this one. Without it a
// changed password is written and waits for the file to be reloaded.
func (s *Server) SetReloader(reload func() error) {
	s.reloader = reload
}

// profileOn reports whether passwords can be changed here at all: there is a
// file to write them to and an account with a password.
func (s *Server) profileOn(set *settings) bool {
	return s.configPath != "" && canLogInToProfile(set)
}

// canLogInToProfile reports whether some account may log in to the Profile
// page, which every account with a password may.
func canLogInToProfile(set *settings) bool {
	return len(set.profileAccounts) > 0
}

// profileClaims is the account a session token stands for among those with a
// password, or nothing: every account the file tree or the registry knows by a
// session is one of them.
func (s *Server) profileClaims(set *settings, claims *sessionClaims) *account {
	if claims.outlived(sessionWindowOf(set.cfg), time.Now()) {
		return nil
	}
	user := accountNamed(set.profileAccounts, claims.Subject)
	if user == nil || !s.tokens.issuedFor(claims, user) {
		return nil
	}
	return user
}

// profileSession is the account the browser holds a session of, whichever
// kind it is, or nothing. Like registrySession it clears and logs nothing.
func (s *Server) profileSession(set *settings, r *http.Request) *account {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	claims, err := s.tokens.read(cookie.Value)
	if err != nil {
		return nil
	}
	return s.profileClaims(set, claims)
}

// handleProfile guards the Profile page and the password change.
func (s *Server) handleProfile(set *settings, w http.ResponseWriter, r *http.Request, action string) {
	if !s.profileOn(set) {
		// nothing to change, or nowhere to write it: the marker names nothing
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Cache-Control", "no-store")
	user := s.profileSession(set, r)
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case action == actionProfile && read:
		if user == nil {
			http.Redirect(w, r, loginURL(r.URL), http.StatusSeeOther)
			return
		}
		s.profilePage(set, w, r, user)
	case action == actionPassword && r.Method == http.MethodPost:
		if !s.sameSite(set, w, r) {
			return
		}
		if user == nil {
			s.logFor(r).Info("http password change refused, no session", "address", clientAddress(set, r))
			http.Error(w, "Not authenticated", http.StatusUnauthorized)
			return
		}
		s.changePassword(set, w, r, user)
	default:
		if action == actionProfile {
			w.Header().Set("Allow", "GET, HEAD")
		} else {
			w.Header().Set("Allow", "POST")
		}
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// changePassword checks what the overlay posted, writes the new password and
// has it applied.
func (s *Server) changePassword(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	if kind := r.Header.Get("Content-Type"); !strings.HasPrefix(kind, "application/x-www-form-urlencoded") {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}
	address := clientAddress(set, r)
	// the current password is checked as a login is, so the overlay is no
	// way around the lock against guessing
	if remaining, locked := s.lockedOut(set, r); locked {
		s.logFor(r).Info("http password change refused, the address is locked out", "user", user.name,
			"address", address, "remaining", remaining.Round(time.Second))
		http.Error(w, lockoutMessage(remaining), http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	current := r.PostFormValue("current")
	password := r.PostFormValue("password")

	if matchAccount([]*account{user}, user.name, current) == nil {
		s.logFor(r).Info("http password change refused, the current password is wrong", "user", user.name,
			"address", address)
		s.recordFailure(set, r)
		if delay := set.cfg.LoginFailureDelay; delay > 0 {
			time.Sleep(time.Duration(delay) * time.Second)
		}
		http.Error(w, "The current password is wrong.", http.StatusForbidden)
		return
	}
	if problem := newPasswordProblem(current, password, r.PostFormValue("repeat")); problem != "" {
		s.logFor(r).Info("http password change refused", "user", user.name, "reason", problem, "address", address)
		http.Error(w, problem, http.StatusBadRequest)
		return
	}

	s.passwordMu.Lock()
	err := config.SetPassword(s.configPath, user.name, password, s.logFor(r))
	s.passwordMu.Unlock()
	if err != nil {
		s.logFor(r).Error("http cannot store a changed password", "user", user.name, "address", address,
			"error", err)
		message := "The password cannot be stored here; ask an administrator."
		if errors.Is(err, config.ErrPasswordPlace) {
			message = "The configuration file is written in a way the password cannot be changed in; ask an administrator."
		}
		http.Error(w, message, http.StatusInternalServerError)
		return
	}
	s.logFor(r).Info("http password changed", "user", user.name, "address", address)

	applied := s.applyConfig(r, user.name, password)
	// the session is issued again for the password that is in force now, so
	// that this browser stays signed in while every other one is signed out
	next := s.settings()
	if renewed := accountNamed(next.profileAccounts, user.name); renewed != nil {
		if err := s.setSession(next, w, r, renewed); err != nil {
			s.logFor(r).Error("http cannot create a session token", "error", err)
		}
	}
	if !applied {
		http.Error(w, "The new password is stored, but takes effect once the configuration is reloaded.",
			http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyConfig has the configuration file applied and reports whether the
// account named name has password now.
func (s *Server) applyConfig(r *http.Request, name, password string) bool {
	if s.reloader != nil {
		done := make(chan error, 1)
		go func() { done <- s.reloader() }()
		select {
		case err := <-done:
			if err != nil {
				s.logFor(r).Error("http cannot apply the configuration after a password change", "error", err)
			}
		case <-time.After(reloadWait):
			s.logFor(r).Warn("http applying the configuration after a password change takes long, "+
				"not waiting for it", "waited", reloadWait)
		}
	}
	user := accountNamed(s.settings().profileAccounts, name)
	return user != nil && matchAccount([]*account{user}, name, password) != nil
}

// newPasswordProblem is what is wrong with a new password, or "".
func newPasswordProblem(current, password, repeat string) string {
	switch {
	case password != repeat:
		return "The new passwords do not match."
	case utf8.RuneCountInString(password) < minPassword:
		return "The new password needs at least 8 characters."
	case password == current:
		return "The new password is the current one."
	case config.IsExampleSecret(password):
		return "That password is printed in the documentation; choose another one."
	case !utf8.ValidString(password) || strings.IndexFunc(password, unicode.IsControl) >= 0:
		return "The new password may not hold control characters."
	}
	return ""
}

// profileData is the Profile page.
type profileData struct {
	// Tiles are those of the start page, which the page shows behind the
	// overlay.
	Tiles       []homeTile
	Credentials string
	User        string
	Nonce       string
	Style       template.CSS
	Script      template.JS
}

var profileTemplate = template.Must(template.Must(template.ParseFS(assets, "assets/profile.html")).
	Parse(webui.CredentialsTemplate()))

// profilePage renders the page: the overlay, open, over the start page.
func (s *Server) profilePage(set *settings, w http.ResponseWriter, r *http.Request, user *account) {
	nonce, err := pageNonce()
	if err != nil {
		s.logFor(r).Error("http cannot render the profile page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	var page bytes.Buffer
	err = profileTemplate.Execute(&page, profileData{
		Tiles:       s.homeTiles(set, r.URL),
		Credentials: passwordURL(),
		User:        user.name,
		Nonce:       nonce,
		Style:       listingStyle,
		Script:      webui.CredentialsScript(),
	})
	if err != nil {
		s.logFor(r).Error("http cannot render the profile page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Content-Security-Policy", contentPolicy(nonce))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page.Bytes())
	}
}
