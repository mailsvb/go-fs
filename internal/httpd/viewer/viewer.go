//go:build !no_viewer

// Package viewer is the document viewer of the web interface: the page the
// listing opens over itself to show a file in the browser, and the libraries
// it draws them with, pdf.js for PDF and the viewer of Office Open XML for
// Word, Excel and PowerPoint, which the binary embeds, so that nothing is
// fetched from anywhere but go-fs.
//
// The libraries load their parts by paths relative to their own, so they are
// served below a path of their own, Prefix, in the folder /.go-fs/ that the
// served tree reserves (see vfs.ReservedName), rather than with a query as
// everything else the server answers is.
//
// They are fetched from npm at build time, by go run ./tools/viewer, into
// libs, which is not committed. A binary built before that has the page but
// not the libraries, and offers no viewer: Available says which.
package viewer

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Compiled says the binary was built with the viewer, which the tag
// no_viewer leaves out.
const Compiled = true

//go:embed app libs
var files embed.FS

// versions is what tools/viewer writes last, so a binary that embeds it has
// every library the lock file pins.
const versions = "libs/VERSIONS"

// Root is the folder below the reserved one the viewer is served from.
const Root = "/.go-fs/viewer/"

var (
	available bool
	// prefix is where the viewer is served: Root and a hash of what is
	// embedded, so that every build has a path of its own, which a browser
	// can keep for as long as it likes.
	prefix string
)

func init() {
	_, err := fs.Stat(files, versions)
	available = err == nil
	prefix = Root + contentHash() + "/"
}

// contentHash hashes the page and the versions of the libraries. The libraries
// themselves are not read: the versions file holds the hash of the lock file
// they were fetched for, which says as much and costs nothing at startup.
func contentHash() string {
	sum := sha256.New()
	var names []string
	_ = fs.WalkDir(files, "app", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, name)
		}
		return nil
	})
	names = append(names, versions)
	sort.Strings(names)
	for _, name := range names {
		body, err := files.ReadFile(name)
		if err != nil {
			continue
		}
		io.WriteString(sum, name+"\x00"+strconv.Itoa(len(body))+"\x00")
		sum.Write(body)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// Available reports whether the binary has the libraries, and so whether a
// file is offered to be viewed at all.
func Available() bool {
	return available
}

// Prefix is the path the viewer is served below, with a slash at its end, ""
// without the libraries.
func Prefix() string {
	if !available {
		return ""
	}
	return prefix
}

// Page is the path of the page the listing opens in its frame, "" without
// the libraries.
func Page() string {
	if !available {
		return ""
	}
	return prefix + "index.html"
}

// kinds says how the viewer shows a file, by its extension. Text is the text
// the listing would otherwise show in a tab of its own, less the delimited
// text, which is shown as a sheet. An SVG is never shown: it is a page that
// can run script, which no other kind here is.
var kinds = map[string]string{
	".pdf": "pdf",

	".docx": "docx",
	".docm": "docx",
	".xlsx": "xlsx",
	".xlsm": "xlsx",
	".pptx": "pptx",
	".pptm": "pptx",
	".ppsx": "pptx",

	".csv": "sheet",
	".tsv": "sheet",

	".txt":        "text",
	".log":        "text",
	".md":         "text",
	".ini":        "text",
	".conf":       "text",
	".cfg":        "text",
	".yaml":       "text",
	".yml":        "text",
	".toml":       "text",
	".json":       "text",
	".xml":        "text",
	".sh":         "text",
	".properties": "text",

	".png":  "image",
	".jpg":  "image",
	".jpeg": "image",
	".gif":  "image",
	".webp": "image",
	".avif": "image",
	".bmp":  "image",
}

// Kind is how the viewer shows a file by its name, "" for a file it does not
// show, and for every file without the libraries.
func Kind(name string) string {
	if !available {
		return ""
	}
	return kinds[strings.ToLower(path.Ext(name))]
}

// pagePolicy is the Content-Security-Policy of the page and of the scripts,
// which the workers they start run under: script only from go-fs, with the
// WebAssembly the parsers are compiled to, and the workers the libraries
// start from their own code in a blob or a data URL. Nothing is fetched from
// anywhere else, and no inline script runs, so a document that got a
// library to write markup still could not run any. Inline style is allowed:
// both libraries lay out text with the style they set, and style runs
// nothing.
const pagePolicy = "default-src 'none'; " +
	"script-src 'self' 'wasm-unsafe-eval'; " +
	"worker-src 'self' blob: data:; " +
	"connect-src 'self'; " +
	"img-src 'self' blob: data:; " +
	"font-src 'self' blob: data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"frame-ancestors 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'"

// assetPolicy is that of everything else: none of it is a page, and if one
// were opened as one, an image of pdf.js for one, it could do nothing.
const assetPolicy = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// Serve answers a request below Root: a file of the page or of a library
// below the current Prefix, and nothing else, so that the path of an older
// build is not mistaken for this one's.
func Serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !available || !ok || !fs.ValidPath(rest) {
		http.NotFound(w, r)
		return
	}
	name, compressed := locate(rest)
	if name == "" {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Type", typeOf(rest))
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	if scripted(rest) {
		header.Set("Content-Security-Policy", pagePolicy)
		header.Set("X-Frame-Options", "SAMEORIGIN")
	} else {
		header.Set("Content-Security-Policy", assetPolicy)
	}
	body, err := files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = body.Close() }()
	var out io.Reader = body
	switch {
	case compressed && acceptsGzip(r):
		header.Set("Content-Encoding", "gzip")
		header.Add("Vary", "Accept-Encoding")
		if info, err := body.Stat(); err == nil {
			header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
	case compressed:
		header.Add("Vary", "Accept-Encoding")
		unzipped, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "Server Error", http.StatusInternalServerError)
			return
		}
		out = unzipped
	default:
		if info, err := body.Stat(); err == nil {
			header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, out)
}

// locate finds a path below the prefix among what is embedded: the page is
// app, the libraries are libs, compressed, as tools/viewer wrote them. A
// file stored compressed is only there by its own name, so that nothing is
// sent compressed as if it were what it holds.
func locate(rest string) (string, bool) {
	if strings.HasSuffix(rest, ".gz") {
		return "", false
	}
	for _, candidate := range []struct {
		name       string
		compressed bool
	}{
		{"app/" + rest, false},
		{"libs/" + rest + ".gz", true},
		{"libs/" + rest, false},
	} {
		if info, err := fs.Stat(files, candidate.name); err == nil && !info.IsDir() {
			return candidate.name, candidate.compressed
		}
	}
	return "", false
}

// scripted is what runs, and so carries the policy of the page.
func scripted(name string) bool {
	switch path.Ext(name) {
	case ".html", ".js", ".mjs":
		return true
	}
	return false
}

// typeOf is the type a file is served as. The browser is held to it, so a
// parser is application/wasm, which is what lets it be compiled as it
// arrives, and a script is a script.
func typeOf(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".svg":
		return "image/svg+xml"
	case ".gif":
		return "image/gif"
	case ".png":
		return "image/png"
	case ".json":
		return "application/json"
	case ".md", "":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// acceptsGzip reports whether the client takes a gzip body, which every
// browser does; a q of 0 refuses it.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
		if !ok {
			return true
		}
		value, err := strconv.ParseFloat(q, 64)
		return err == nil && value > 0
	}
	return false
}
