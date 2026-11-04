//go:build !no_viewer

package viewer

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// requireLibraries skips a test that needs the libraries when they were not
// fetched, unless GOFS_REQUIRE_VIEWER says they must be, as it does in CI.
func requireLibraries(t *testing.T) {
	t.Helper()
	if Available() {
		return
	}
	if os.Getenv("GOFS_REQUIRE_VIEWER") != "" {
		t.Fatal("the libraries of the viewer are not embedded; run make viewer-deps")
	}
	t.Skip("the libraries of the viewer are not fetched; run make viewer-deps")
}

func serve(t *testing.T, method, path, encoding string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if encoding != "" {
		req.Header.Set("Accept-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	Serve(rec, req)
	return rec.Result()
}

func TestServeThePageWithItsPolicy(t *testing.T) {
	requireLibraries(t)
	res := serve(t, http.MethodGet, Page(), "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	policy := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self' 'wasm-unsafe-eval'", "frame-ancestors 'self'", "connect-src 'self'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the policy %q lacks %q", policy, want)
		}
	}
	for _, refused := range []string{"unsafe-eval'", "https:", "*"} {
		if strings.Contains(strings.ReplaceAll(policy, "'wasm-unsafe-eval'", ""), refused) {
			t.Errorf("the policy %q allows %q", policy, refused)
		}
	}
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `src="viewer.js"`) {
		t.Error("the page does not load its script")
	}
}

// A library is sent as it is stored, compressed, to a client that takes
// gzip, and unpacked for one that does not; either way it is the same file.
func TestServeALibraryCompressedOrNot(t *testing.T) {
	requireLibraries(t)
	path := Prefix() + "pdfjs/build/pdf.min.mjs"

	zipped := serve(t, http.MethodGet, path, "gzip, deflate, br")
	if zipped.StatusCode != http.StatusOK || zipped.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q", zipped.StatusCode, zipped.Header.Get("Content-Encoding"))
	}
	if got := zipped.Header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := zipped.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	reader, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	plain := serve(t, http.MethodGet, path, "")
	if plain.Header.Get("Content-Encoding") != "" {
		t.Errorf("a client without gzip got Content-Encoding %q", plain.Header.Get("Content-Encoding"))
	}
	body, _ := io.ReadAll(plain.Body)
	if len(body) == 0 || string(body) != string(unpacked) {
		t.Errorf("the plain body (%d bytes) is not the unpacked one (%d bytes)", len(body), len(unpacked))
	}
	// a script carries the policy of the page, which a worker started from
	// it runs under
	if !strings.Contains(plain.Header.Get("Content-Security-Policy"), "wasm-unsafe-eval") {
		t.Errorf("a script has the policy %q", plain.Header.Get("Content-Security-Policy"))
	}
}

func TestServeTypesTheParsersAndLocksDownTheRest(t *testing.T) {
	requireLibraries(t)
	cases := []struct{ path, media string }{
		{"ooxml/docx_parser_bg.wasm", "application/wasm"},
		{"pdfjs/web/pdf_viewer.css", "text/css; charset=utf-8"},
		{"pdfjs/cmaps/Adobe-Japan1-UCS2.bcmap", "application/octet-stream"},
		{"pdfjs/LICENSE", "text/plain; charset=utf-8"},
		{"VERSIONS", "text/plain; charset=utf-8"},
	}
	for _, tc := range cases {
		res := serve(t, http.MethodGet, Prefix()+tc.path, "gzip")
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", tc.path, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Content-Type"); got != tc.media {
			t.Errorf("%s: Content-Type = %q, want %q", tc.path, got, tc.media)
		}
		if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
			t.Errorf("%s: Content-Security-Policy = %q, want the sandbox", tc.path, got)
		}
	}
}

func TestServeOnlyWhatIsThere(t *testing.T) {
	requireLibraries(t)
	for _, path := range []string{
		Root + "000000000000/index.html", // the path of another build
		Prefix() + "missing.js",
		Prefix() + "pdfjs/",
		Prefix() + "../index.html",
		Prefix() + "pdfjs/build/pdf.min.mjs.gz",
		Prefix(),
	} {
		if res := serve(t, http.MethodGet, path, ""); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, res.StatusCode)
		}
	}
	res := serve(t, http.MethodPost, Page(), "")
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: status %d, Allow %q", res.StatusCode, res.Header.Get("Allow"))
	}
	head := serve(t, http.MethodHead, Page(), "")
	if body, _ := io.ReadAll(head.Body); head.StatusCode != http.StatusOK || len(body) != 0 {
		t.Errorf("HEAD: status %d, %d bytes of body", head.StatusCode, len(body))
	}
}

func TestKind(t *testing.T) {
	requireLibraries(t)
	cases := map[string]string{
		"report.PDF":   "pdf",
		"letter.docx":  "docx",
		"budget.xlsm":  "xlsx",
		"deck.pptx":    "pptx",
		"show.ppsx":    "pptx",
		"export.csv":   "sheet",
		"server.log":   "text",
		"photo.JPEG":   "image",
		"drawing.svg":  "",
		"page.html":    "",
		"archive.zip":  "",
		"old.doc":      "",
		"no-extension": "",
	}
	for name, want := range cases {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}

// Without the libraries no file is offered, and nothing is served.
func TestNothingIsOfferedWithoutTheLibraries(t *testing.T) {
	saved := available
	t.Cleanup(func() { available = saved })
	available = false
	if Kind("report.pdf") != "" || Page() != "" || Prefix() != "" {
		t.Error("a binary without the libraries offers the viewer")
	}
	if res := serve(t, http.MethodGet, prefix+"index.html", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", res.StatusCode)
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                  false,
		"gzip":              true,
		"GZIP":              true,
		"deflate, gzip;q=1": true,
		"br, gzip;q=0.5":    true,
		"gzip;q=0":          false,
		"gzip; q=0":         false,
		"identity":          false,
		"x-gzip":            false,
	}
	for header, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", header)
		if got := acceptsGzip(req); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
