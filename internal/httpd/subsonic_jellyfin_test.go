//go:build !light && !no_subsonic && !no_jellyfin

package httpd

import (
	"testing"
)

// TestSubsonicAndJellyfinKeepApart checks the libraries of the two APIs stay
// with their own clients, and that what one kind of client does the other
// does not see.
func TestSubsonicAndJellyfinKeepApart(t *testing.T) {
	server, library := newSubsonicServer(t, func(cfg *httpConfig) { cfg.JellyfinFolders = []string{"/music"} })
	token := jfToken(t, server, "john", "doe")
	if views := jfList(t, server, "/UserViews", token); len(views.Items) != 0 {
		t.Errorf("a Jellyfin client sees the subsonic library: %q", views.names())
	}
	// nor does it get its covers without a token
	if res := jf(t, server, "GET", "/Items/"+library.Albums[0].ID+"/Images/Primary", "", nil); res.StatusCode != 404 {
		t.Errorf("a Jellyfin client gets the cover of a subsonic album: %d", res.StatusCode)
	}

	subOK(t, server, "star", subParams("id", library.Tracks[0].ID))
	if data := server.jellyfin.listeners.Get("john", library.Tracks[0].ID); data.Favorite {
		t.Error("a star of an OpenSubsonic client is a favorite of Jellyfin's")
	}
	if data := server.subsonic.listeners.Get("john", library.Tracks[0].ID); !data.Favorite || data.FavoriteAt.IsZero() {
		t.Errorf("the star is kept as %+v", data)
	}
}
