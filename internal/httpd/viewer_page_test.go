package httpd

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/httpd/viewer"
)

// requireViewer skips a test of the document viewer in a binary without its
// libraries, unless GOFS_REQUIRE_VIEWER says they must be there, as CI does
// for every build that has the viewer.
func requireViewer(t *testing.T) {
	t.Helper()
	if viewer.Available() {
		return
	}
	if viewer.Compiled && os.Getenv("GOFS_REQUIRE_VIEWER") != "" {
		t.Fatal("the libraries of the document viewer are not embedded; run make viewer-deps")
	}
	t.Skip("the libraries of the document viewer are not fetched; run make viewer-deps")
}

// /.go-fs/ is go-fs's own: nothing in it is a file of the served tree, even
// one that is on disk there, and the listing of the top does not show it.
func TestReservedFolderIsNeverServed(t *testing.T) {
	server := newServer(t, publicServer)
	server.write(t, ".go-fs/secret.txt", "planted")
	server.write(t, "sub/.go-fs/kept.txt", "kept")
	server.write(t, "visible.txt", "visible")

	for _, path := range []string{"/.go-fs/secret.txt", "/.go-fs/", "/.go-fs", "/.go-fs/viewer/x", "/sub/../.go-fs/secret.txt"} {
		res, body := get(t, server, path)
		if res.StatusCode != http.StatusNotFound || strings.Contains(body, "planted") {
			t.Errorf("GET %s: status %d, body %q", path, res.StatusCode, body)
		}
	}
	if res, body := get(t, server, "/sub/.go-fs/kept.txt"); res.StatusCode != http.StatusOK || body != "kept" {
		t.Errorf("a .go-fs further down is not served: status %d, body %q", res.StatusCode, body)
	}
	res, body := get(t, server, "/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "visible.txt") {
		t.Fatalf("status %d", res.StatusCode)
	}
	if strings.Contains(body, `data-name=".go-fs"`) {
		t.Error("the listing of the top shows the reserved folder")
	}

	put := basic(t, server, http.MethodPut, "/.go-fs/new.txt", "john", "doe", strings.NewReader("x"))
	if put.StatusCode < 400 {
		t.Errorf("PUT into the reserved folder: status %d", put.StatusCode)
	}
	if _, err := os.Stat(server.base + "/.go-fs/new.txt"); err == nil {
		t.Error("a PUT wrote into the reserved folder")
	}
}

func TestViewerIsServedBelowTheReservedFolder(t *testing.T) {
	requireViewer(t)
	server := newServer(t, nil)
	// public, as the code of a page is: nobody signs in to fetch it
	res, body := get(t, server, viewer.Page())
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "viewer.js") {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'self'") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
}

// The listing marks the names the viewer shows with how it shows them and
// where it reads them from: the view of the file, whatever its type, so that
// the log says it was viewed.
func TestListingOffersTheViewer(t *testing.T) {
	requireViewer(t)
	server := newServer(t, publicServer)
	server.write(t, "sub/report.pdf", "%PDF")
	server.write(t, "sub/letter.docx", "PK")
	server.write(t, "sub/photo.png", "png")
	server.write(t, "sub/bundle.zip", "zip")
	server.write(t, "sub/drawing.svg", "<svg/>")

	res, body := get(t, server, "/sub/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	for _, want := range []string{
		`data-viewer="pdf" data-viewer-src="report.pdf?go-fs=view"`,
		`<a href="letter.docx" data-viewer="docx" data-viewer-src="letter.docx?go-fs=view">`,
		`data-viewer="image" data-viewer-src="photo.png?go-fs=view"`,
		`<a href="bundle.zip">bundle.zip</a>`,
		`<a href="drawing.svg">drawing.svg</a>`,
		`<dialog id="viewer" class="viewer" data-page="` + viewer.Page() + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-src 'self'") {
		t.Errorf("the listing does not let in the frame of the viewer: %q", got)
	}
}

// Audio and video are offered to be played, with the type the browser is
// asked about, and the page names the bar that plays audio; what no browser
// plays stays a download.
func TestListingOffersToPlay(t *testing.T) {
	requireViewer(t)
	server := newServer(t, publicServer)
	server.write(t, "sub/song.mp3", "ID3")
	server.write(t, "sub/clip.mp4", "mp4")
	server.write(t, "sub/movie.mkv", "mkv")
	server.write(t, "sub/old.avi", "avi")

	res, body := get(t, server, "/sub/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	for _, want := range []string{
		`data-viewer="audio" data-viewer-src="song.mp3?go-fs=view" data-viewer-type="audio/mpeg"`,
		`data-viewer="video" data-viewer-src="clip.mp4?go-fs=view" data-viewer-type="video/mp4"`,
		`data-viewer="video" data-viewer-src="movie.mkv?go-fs=view" data-viewer-type="video/x-matroska"`,
		`<a href="old.avi">old.avi</a>`,
		`data-player="` + viewer.Player() + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// a document is not asked about
	server.write(t, "sub/report.pdf", "%PDF")
	if _, body := get(t, server, "/sub/"); strings.Contains(body, `data-viewer-src="report.pdf?go-fs=view" data-viewer-type`) {
		t.Error("a PDF carries a media type")
	}
}

// What is played is streamed: the browser asks for the part it needs, and
// is sent that part, as the type it is.
func TestPlayingStreamsARange(t *testing.T) {
	requireViewer(t)
	server := newServer(t, publicServer)
	server.write(t, "clip.mp4", "0123456789")
	req, err := http.NewRequest(http.MethodGet, server.url("/clip.mp4?go-fs=view"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=2-5")
	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("status %d body %q, want 206 and the range", res.StatusCode, body)
	}
	if got := res.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q", got)
	}
}

// Edit is offered on a file of text, and on delimited text, to an account
// that may replace it, and to nobody else; it reads the file as the viewer
// does.
func TestListingOffersEditToWhoMayReplace(t *testing.T) {
	requireViewer(t)
	page := func(t *testing.T, server *testServer, user string) string {
		t.Helper()
		var res *http.Response
		if user == "" {
			res, _ = get(t, server, "/sub/")
		} else {
			res = basic(t, server, http.MethodGet, "/sub/", user, "doe", nil)
		}
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		return string(body)
	}
	server := newServer(t, func(cfg *httpConfig) {
		publicServer(cfg)
		reader := fullUser("kim", "doe")
		reader.AllowUserFileOverwrite = new(false)
		cfg.Users = append(cfg.Users, reader)
	})
	server.write(t, "sub/notes.txt", "hello")
	server.write(t, "sub/main.go", "package main")
	server.write(t, "sub/export.csv", "a,b")
	server.write(t, "sub/report.pdf", "%PDF")

	body := page(t, server, "john")
	for _, want := range []string{
		`data-edit="notes.txt?go-fs=view"`,
		`data-edit="main.go?go-fs=view"`,
		`data-edit="export.csv?go-fs=view"`,
		`data-viewer="text" data-viewer-src="main.go?go-fs=view"`,
		`data-do="edit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(body, `data-edit="report.pdf`) {
		t.Error("a PDF is offered to be edited")
	}
	for _, user := range []string{"", "kim"} {
		body := page(t, server, user)
		if strings.Contains(body, "data-edit=") || strings.Contains(body, `data-do="edit"`) {
			t.Errorf("Edit is offered to %q, who may not replace a file", user)
		}
	}
}

// A stored server's listing reads a file through its own download, with its
// own view marker.
func TestRemoteListingOffersTheViewer(t *testing.T) {
	requireViewer(t)
	entries := []entry{
		{Name: "deck.pptx", IsFile: true, ModTime: time.Now()},
		{Name: "notes.txt", IsFile: true, ModTime: time.Now()},
		{Name: "song.flac", IsFile: true, ModTime: time.Now()},
	}
	page, err := listingPage(remotePlace("share", "/docs/"), entries, sortOrder{}, rights{Download: true},
		sendView{}, sessionView{}, "nonce", config.HTTP{})
	if err != nil {
		t.Fatal(err)
	}
	want := `data-viewer="pptx" data-viewer-src="` + strings.ReplaceAll(remoteURL("share", "/docs/deck.pptx"), "&", "&amp;") +
		"&amp;" + remoteViewParam + `=1"`
	if !strings.Contains(string(page), want) {
		t.Errorf("the page is missing %q", want)
	}
	if !strings.Contains(string(page), `data-viewer="audio"`) || !strings.Contains(string(page), `data-viewer-type="audio/flac"`) {
		t.Error("the audio of a stored server is not offered to be played")
	}
	// what the editor saves goes to the served tree alone
	if strings.Contains(string(page), "data-edit=") {
		t.Error("a file of a stored server is offered to be edited")
	}
}

// What the viewer reads of a file it shows and the browser could not, a Word
// file for one, is still sent as a download, and logged as a view.
func TestViewOfAnOfficeFileIsLoggedAsAView(t *testing.T) {
	requireViewer(t)
	server := newServer(t, publicServer)
	server.write(t, "letter.docx", "PK pretend")
	res, body := get(t, server, "/letter.docx?go-fs=view")
	if res.StatusCode != http.StatusOK || body != "PK pretend" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want the download it always was", got)
	}
	if server.logs.find("http view") == nil {
		t.Error("the view was not logged as one")
	}
}
