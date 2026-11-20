package httpd

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
)

// The main URL is where someone arrives who has not said yet what they came
// for. Where it needs an account and the browser has none, it is not sent
// straight to the login for the files: the server may have a registry too,
// and the login for the files says nothing about it. It gets a page that
// offers each of them instead, and a login only once it has picked one.
//
// Only the plain request for / is answered so. A query on it is a page of the
// listing, or a fetch from one, and goes to the login as any other path does;
// a root that pathsWithoutAuth makes public never gets here, since refuse is
// only reached by a request that needs an account.

// homeTile is one of the places the page at / offers.
type homeTile struct {
	Label string
	Link  string
	Icon  string
}

type homeData struct {
	Tiles []homeTile
	Nonce string
	Style template.CSS
}

// homeTemplate is a page of its own for the reason loginTemplate is: it is
// shown before anything has authenticated, and has nothing of the folder in it
// to leave out.
var homeTemplate = template.Must(template.ParseFS(assets, "assets/home.html"))

func renderHome(data homeData) ([]byte, error) {
	var page bytes.Buffer
	if err := homeTemplate.Execute(&page, data); err != nil {
		return nil, err
	}
	return page.Bytes(), nil
}

// homeRequest reports a browser asking for the plain main URL.
func homeRequest(r *http.Request) bool {
	return r.URL.Path == "/" && r.URL.RawQuery == ""
}

// homeTiles is what a browser may go on to from /: the files where they are
// served and an http account can log in, the registry where it is on and
// someone may look at it, an account of its own or anyone at all, the admin
// interface where it is on and an account may open it, and the Profile page
// where some account has a password to change. Each link goes
// to its page, which sends a browser without the session it needs to log in.
func (s *Server) homeTiles(set *settings, u *url.URL) []homeTile {
	var tiles []homeTile
	if filesOn(set) && canLogIn(set) {
		tiles = append(tiles, homeTile{Label: "Files", Link: loginURL(u), Icon: "i-folder"})
	}
	if set.registry != nil && (canLogInToRegistry(set) || set.cfg.RegistryAnonymousRead) {
		// the registry page sends whoever it refuses to the login, which
		// returns there
		tiles = append(tiles, homeTile{Label: "Registry", Link: registryURL(u), Icon: "i-box"})
	}
	if s.adminAvailable(set) {
		tiles = append(tiles, homeTile{Label: "Admin", Link: adminURL(&url.URL{Path: "/"}), Icon: "i-gear"})
	}
	if s.profileOn(set) {
		// where any account with a password changes it, one that may use
		// nothing else here too; the page sends a browser without a session
		// to log in, which returns there
		tiles = append(tiles, homeTile{Label: "Profile", Link: profileURL(), Icon: "i-user"})
	}
	return tiles
}

// home answers a browser that asked for / without the account it needs, or
// any browser at all while the files are not served, since / then has no
// listing to show. With one place to go it is sent there, and with more it is
// asked which.
func (s *Server) home(set *settings, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tiles := s.homeTiles(set, r.URL)
	switch len(tiles) {
	case 0:
		if !filesOn(set) {
			// nothing is served here that a browser could open
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, loginURL(r.URL), http.StatusSeeOther)
		return
	case 1:
		http.Redirect(w, r, tiles[0].Link, http.StatusSeeOther)
		return
	}
	nonce, err := pageNonce()
	if err != nil {
		s.logFor(r).Error("http cannot render the start page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	page, err := renderHome(homeData{Tiles: tiles, Nonce: nonce, Style: listingStyle})
	if err != nil {
		s.logFor(r).Error("http cannot render the start page", "error", err)
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	// who is signed in decides whether this page is shown at all
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Content-Security-Policy", contentPolicy(nonce))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page)
	}
}

// filesOn reports whether the http server serves the files of the served
// folder, which http.services.files switches off for a server that is there
// for its registry, say.
func filesOn(set *settings) bool {
	return set.cfg.Files
}

// withoutFiles answers a request for the files while they are not served. A
// browser asking for the plain / gets the page that offers what is left;
// anything else names nothing on this server.
func (s *Server) withoutFiles(set *settings, w http.ResponseWriter, r *http.Request) {
	if homeRequest(r) && browserRequest(r) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.home(set, w, r)
		return
	}
	s.logFor(r).Debug("http files are not served, http.services.files is off", "method", r.Method,
		"url", r.URL.Path, "address", clientAddress(set, r))
	http.NotFound(w, r)
}
