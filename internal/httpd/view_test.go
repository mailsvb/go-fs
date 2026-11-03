package httpd

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
)

// A PDF or a text file asked for with ?go-fs=view is shown in the browser,
// in a sandbox of an origin of its own so that it cannot act on the listing's.
func TestViewShowsPDFAndText(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/report.pdf", "%PDF-1.4 pretend")
	server.write(t, "private/notes.txt", "plain notes")
	server.write(t, "private/data.json", `{"a":1}`)

	cases := []struct {
		name, media string
	}{
		{"report.pdf", "application/pdf"},
		{"notes.txt", "text/plain; charset=utf-8"},
		// shown as text, never as what a browser would make of it
		{"data.json", "text/plain; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := basic(t, server, http.MethodGet, "/private/"+tc.name+"?go-fs=view", "john", "doe", nil)
			if body := bodyOf(t, res); res.StatusCode != http.StatusOK || body != server.read(t, "private/"+tc.name) {
				t.Fatalf("status %d body %q", res.StatusCode, body)
			}
			if got := res.Header.Get("Content-Type"); got != tc.media {
				t.Errorf("Content-Type = %q, want %q", got, tc.media)
			}
			if got := res.Header.Get("Content-Disposition"); got != `inline; filename="`+tc.name+`"` {
				t.Errorf("Content-Disposition = %q", got)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := res.Header.Get("Content-Security-Policy"); !strings.HasPrefix(got, "sandbox;") {
				t.Errorf("Content-Security-Policy = %q, want a sandbox", got)
			}
		})
	}
	if record := server.logs.find("http view"); record == nil {
		t.Error("a view has to be reported")
	} else if record["file"] != "/private/report.pdf" {
		t.Errorf("view record = %v", record)
	}
}

// What a browser cannot be trusted to show, or cannot show at all, is
// downloaded even when a view is asked for: an uploaded page would run its
// script with the session of whoever opened it.
func TestViewDownloadsWhatItCannotShow(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/page.html", "<script>alert(1)</script>")
	server.write(t, "private/image.svg", "<svg/>")
	server.write(t, "private/bundle.zip", "zip")

	for _, name := range []string{"page.html", "image.svg", "bundle.zip"} {
		res := basic(t, server, http.MethodGet, "/private/"+name+"?go-fs=view", "john", "doe", nil)
		_, _ = io.Copy(io.Discard, res.Body)
		if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="`+name+`"` {
			t.Errorf("%s: Content-Disposition = %q", name, got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", name, got)
		}
	}
}

// Without the marker a file is downloaded as it always was, so that programs
// and the menu's Download see no difference.
func TestPlainGetStillDownloadsAViewableFile(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/report.pdf", "%PDF")

	res := basic(t, server, http.MethodGet, "/private/report.pdf", "john", "doe", nil)
	_, _ = io.Copy(io.Discard, res.Body)
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="report.pdf"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("a download carries the policy %q", got)
	}
}

// A viewer asks for a PDF in ranges, so a view answers them as a download does.
func TestViewAnswersARange(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/report.pdf", "0123456789")

	req, _ := http.NewRequest(http.MethodGet, server.url("/private/report.pdf?go-fs=view"), nil)
	req.SetBasicAuth("john", "doe")
	req.Header.Set("Range", "bytes=2-5")
	res := do(t, req)
	if body := bodyOf(t, res); res.StatusCode != http.StatusPartialContent || body != "2345" {
		t.Errorf("status %d body %q", res.StatusCode, body)
	}
}

// Viewing is reading the file, so an account that may not download it may not
// view it either.
func TestViewNeedsTheRightToRead(t *testing.T) {
	server := newServer(t, func(cfg *httpConfig) {
		user := fullUser("john", "doe")
		user.AllowUserFileRetrieve = new(false)
		cfg.Users = []config.User{user}
	})
	server.write(t, "private/notes.txt", "secret")

	res := basic(t, server, http.MethodGet, "/private/notes.txt?go-fs=view", "john", "doe", nil)
	if body := bodyOf(t, res); res.StatusCode != http.StatusForbidden || body == "secret" {
		t.Errorf("status %d body %q", res.StatusCode, body)
	}
}

// The listing links the names of the files it can show to their view, in a tab
// of their own, and those of the others to the file itself.
func TestListingLinksViewableFilesToTheirView(t *testing.T) {
	server := newServer(t, publicServer)
	server.write(t, "sub/report.pdf", "%PDF")
	server.write(t, "sub/bundle.zip", "zip")
	server.write(t, "sub/page.html", "<p>")

	res, body := get(t, server, "/sub/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	for _, want := range []string{
		`<a href="report.pdf?go-fs=view" target="_blank" rel="noopener">report.pdf</a>`,
		`<a href="bundle.zip">bundle.zip</a>`,
		`<a href="page.html">page.html</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}
