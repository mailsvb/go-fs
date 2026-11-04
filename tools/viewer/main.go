// Command viewer fetches the libraries the document viewer of the web interface
// is built from, pdf.js, the viewer of Office Open XML, the editor Ace and the
// player Video.js, from npm at the versions internal/httpd/viewer/libraries.lock pins, and puts
// the files the viewer needs into internal/httpd/viewer/libs, where the binary
// embeds them.
//
//	go run ./tools/viewer          fetch what the lock file pins, unless it is there already
//	go run ./tools/viewer -check   fail unless what is there is what the lock file pins
//	go run ./tools/viewer -update  pin the latest version of each library, then fetch it
//
// The latest version is the one npm tags latest, or the tag a library names
// instead, which is how Video.js stays on the line of version 8.
//
// Only the lock file is committed; what is fetched is not. Every tarball is
// checked against the sha512 the lock file pins, the one npm published, each
// time it is used, whether it was downloaded or comes from the cache, so a
// mirror or a cache can serve the files but never change them.
//
// GOFS_NPM_REGISTRY names a registry other than registry.npmjs.org, a mirror
// for one; the proxy variables of the environment are honoured. Tarballs are
// kept in go-fs/npm of the user's cache folder, or in GOFS_NPM_CACHE, which
// CI sets to a folder it keeps between runs.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// layout is the version of what the tool makes of a tarball. It is part of
// the stamp, so that a change to which files are kept fetches again even when
// the lock file did not change.
const layout = "1"

// versionsName is the file written last into the output folder: it says what
// was fetched, and its first line is the stamp of the lock file it was fetched
// for. Until it is there, nothing counts as fetched, so a fetch that was
// interrupted is done again. The binary embeds it, and only offers the viewer
// where it finds it.
const versionsName = "VERSIONS"

// stampPrefix starts the first line of the versions file.
const stampPrefix = "# stamp "

// keepName is the one file of the output folder that is committed, and so
// the one a fetch leaves alone.
const keepName = "README.md"

// maxTarball is more than any of the libraries has ever been.
const maxTarball = 256 << 20

// library is what the viewer takes from the tarball of a package: the folder
// it goes to below the output, and where each file it keeps goes in that
// folder, "" for a file it leaves out. A path is relative to package/ in the
// tarball, with forward slashes. tag is the dist-tag of npm an update follows,
// "" for latest.
type library struct {
	folder string
	tag    string
	place  func(name string) string
}

// tagOf is the dist-tag an update of a library follows.
func (lib library) tagOf() string {
	if lib.tag == "" {
		return "latest"
	}
	return lib.tag
}

// libraries are the packages the lock file may pin. A package it pins that is
// not here is an error: what to take from it has to be decided first.
var libraries = map[string]library{
	// pdf.js: the library and its worker, the viewer component the page
	// scrolls the pages in, and the files the worker fetches as it needs
	// them. Left out are the source maps, the legacy build, the scripting
	// sandbox (scripts in a PDF are never run) and its wasm, the image
	// decoders that are only for use without the worker, the types, and the
	// fallbacks in JavaScript for a browser without WebAssembly.
	"pdfjs-dist": {folder: "pdfjs", place: func(name string) string {
		switch {
		case name == "LICENSE",
			name == "build/pdf.min.mjs",
			name == "build/pdf.worker.min.mjs",
			name == "web/pdf_viewer.mjs",
			name == "web/pdf_viewer.css",
			strings.HasPrefix(name, "web/images/"),
			strings.HasPrefix(name, "cmaps/"),
			strings.HasPrefix(name, "standard_fonts/"),
			strings.HasPrefix(name, "iccs/"):
			return name
		case strings.HasPrefix(name, "wasm/"):
			base := path.Base(name)
			if strings.Contains(strings.ToLower(base), "quickjs") || strings.HasSuffix(base, ".js") {
				return ""
			}
			return name
		}
		return ""
	}},
	// Ace, the editor text is shown and edited in: its minified build without
	// the module loader of its own, every mode, which load one another, the
	// list that picks a mode by the name of a file, the search box, and the
	// two themes the page uses, light and dark. Left out are the workers
	// (the page runs none; they only check syntax as one types), the
	// snippets, the key bindings of other editors, the other extensions and
	// themes, and the builds that are not minified.
	"ace-builds": {folder: "ace", place: func(name string) string {
		if name == "LICENSE" {
			return name
		}
		rest, ok := strings.CutPrefix(name, "src-min-noconflict/")
		if !ok || strings.Contains(rest, "/") {
			return ""
		}
		switch {
		case rest == "ace.js",
			rest == "ext-modelist.js",
			rest == "ext-searchbox.js",
			rest == "theme-tomorrow.js",
			rest == "theme-tomorrow_night.js",
			strings.HasPrefix(rest, "mode-") && strings.HasSuffix(rest, ".js"):
			return rest
		}
		return ""
	}},
	// the viewer of Office Open XML: everything it ships for the browser,
	// which loads its chunks, parsers and workers by their relative paths,
	// so that none can be left out on a guess. Left out are the types, the
	// entry for Node, and the engine that draws equations, 4 MB that is only
	// loaded when it is passed to a viewer, which the page does not do.
	"@silurus/ooxml": {folder: "ooxml", place: func(name string) string {
		switch {
		case name == "LICENSE", name == "THIRD_PARTY_NOTICES.md":
			return name
		case !strings.HasPrefix(name, "dist/"):
			return ""
		}
		rest := strings.TrimPrefix(name, "dist/")
		switch {
		case strings.HasPrefix(rest, "types/"), strings.HasSuffix(rest, ".map"),
			rest == "node.mjs", rest == "math.mjs", rest == "mathjax-stix2.js":
			return ""
		}
		return rest
	}},
	// Video.js, which plays audio and video: the core of the line of version
	// 8, without the streaming of HLS and DASH (a file is played as it is),
	// and its style. The name video.js tags latest is a stub of version 10,
	// which only throws, so an update follows latest-8.
	"video.js": {folder: "videojs", tag: "latest-8", place: func(name string) string {
		switch name {
		case "LICENSE":
			return name
		case "dist/alt/video.core.min.js":
			return "video.core.min.js"
		case "dist/video-js.min.css":
			return "video-js.min.css"
		}
		return ""
	}},
}

// pin is a package the lock file pins.
type pin struct {
	name, version, integrity string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "viewer:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		lockPath = flag.String("lock", filepath.Join("internal", "httpd", "viewer", "libraries.lock"), "the lock file")
		out      = flag.String("out", filepath.Join("internal", "httpd", "viewer", "libs"), "the folder the files go to")
		check    = flag.Bool("check", false, "fail unless the folder holds what the lock file pins, and fetch nothing")
		update   = flag.Bool("update", false, "pin the latest version of each library in the lock file, then fetch")
	)
	flag.Parse()
	registry := strings.TrimRight(os.Getenv("GOFS_NPM_REGISTRY"), "/")
	if registry == "" {
		registry = "https://registry.npmjs.org"
	}
	client := &http.Client{Timeout: 5 * time.Minute}

	if *update {
		if err := pinLatest(client, registry, *lockPath); err != nil {
			return err
		}
	}
	lock, err := os.ReadFile(*lockPath)
	if err != nil {
		return err
	}
	pins, err := parseLock(lock)
	if err != nil {
		return fmt.Errorf("%s: %w", *lockPath, err)
	}
	stamp := stampOf(lock)
	if fetched(*out) == stamp {
		return nil
	}
	if *check {
		return fmt.Errorf("%s does not hold what %s pins; run make viewer-deps", *out, *lockPath)
	}
	cache, err := cacheFolder()
	if err != nil {
		return err
	}
	if err := clearOutput(*out); err != nil {
		return err
	}
	for _, p := range pins {
		tarball, err := tarballOf(client, registry, cache, p)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.name, p.version, err)
		}
		files, err := extract(tarball, libraries[p.name], *out)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.name, p.version, err)
		}
		fmt.Printf("viewer: %s %s, %d files\n", p.name, p.version, files)
	}
	return writeVersions(*out, stamp, pins)
}

// parseLock reads the lock file: a line per package, its name, version and
// the integrity npm publishes for its tarball, with # starting a comment.
func parseLock(lock []byte) ([]pin, error) {
	var pins []pin
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(lock))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want a name, a version and an integrity", n)
		}
		p := pin{name: fields[0], version: fields[1], integrity: fields[2]}
		if _, ok := libraries[p.name]; !ok {
			return nil, fmt.Errorf("line %d: %s is not a library the viewer knows what to take from", n, p.name)
		}
		if seen[p.name] {
			return nil, fmt.Errorf("line %d: %s is pinned twice", n, p.name)
		}
		if _, err := digestOf(p.integrity); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		seen[p.name] = true
		pins = append(pins, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for name := range libraries {
		if !seen[name] {
			return nil, fmt.Errorf("%s is not pinned", name)
		}
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].name < pins[j].name })
	return pins, nil
}

// digestOf decodes an integrity of the form npm publishes, sha512-<base64>.
func digestOf(integrity string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		return nil, fmt.Errorf("integrity %q is not a sha512", integrity)
	}
	digest, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(digest) != sha512.Size {
		return nil, fmt.Errorf("integrity %q is not a sha512", integrity)
	}
	return digest, nil
}

func stampOf(lock []byte) string {
	sum := sha256.Sum256(append(append([]byte(nil), lock...), "\nlayout "+layout...))
	return hex.EncodeToString(sum[:])
}

// fetched is the stamp of what the output folder holds, "" for nothing whole.
func fetched(out string) string {
	f, err := os.Open(filepath.Join(out, versionsName))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	first, _ := bufio.NewReader(f).ReadString('\n')
	stamp, ok := strings.CutPrefix(strings.TrimSpace(first), strings.TrimSpace(stampPrefix))
	if !ok {
		return ""
	}
	return strings.TrimSpace(stamp)
}

// cacheFolder is where tarballs are kept between fetches, so that a library
// is downloaded once per version on a machine.
func cacheFolder() (string, error) {
	if folder := os.Getenv("GOFS_NPM_CACHE"); folder != "" {
		return folder, os.MkdirAll(folder, 0o755)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	folder := filepath.Join(base, "go-fs", "npm")
	return folder, os.MkdirAll(folder, 0o755)
}

// clearOutput removes everything of the output folder but the committed
// file, the versions file first, so that what an update leaves behind of an
// older version is never embedded beside the new one.
func clearOutput(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(out, versionsName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == keepName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(out, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// tarballOf is the tarball of a package, from the cache or else downloaded,
// and either way checked against the integrity pinned for it.
func tarballOf(client *http.Client, registry, cache string, p pin) ([]byte, error) {
	digest, err := digestOf(p.integrity)
	if err != nil {
		return nil, err
	}
	cached := filepath.Join(cache, hex.EncodeToString(digest)+".tgz")
	if body, err := os.ReadFile(cached); err == nil {
		if matches(body, digest) {
			return body, nil
		}
		return nil, fmt.Errorf("the cached tarball %s does not match the integrity pinned for it; "+
			"remove it to download it again", cached)
	}
	url := tarballURL(registry, p)
	body, err := get(client, url)
	if err != nil {
		return nil, err
	}
	if !matches(body, digest) {
		return nil, fmt.Errorf("%s does not match the integrity pinned for it", url)
	}
	// written aside and renamed, so that the cache never holds half a tarball
	temp, err := os.CreateTemp(cache, ".tarball-*")
	if err != nil {
		return nil, err
	}
	_, werr := temp.Write(body)
	cerr := temp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(temp.Name())
		return nil, errors.Join(werr, cerr)
	}
	if err := os.Rename(temp.Name(), cached); err != nil {
		_ = os.Remove(temp.Name())
		return nil, err
	}
	return body, nil
}

func matches(body, digest []byte) bool {
	sum := sha512.Sum512(body)
	return bytes.Equal(sum[:], digest)
}

// tarballURL is where a registry serves the tarball of a version: the name of
// a scoped package is its own in the path and without the scope in the file.
func tarballURL(registry string, p pin) string {
	return registry + "/" + p.name + "/-/" + path.Base(p.name) + "-" + p.version + ".tgz"
}

func get(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "go-fs tools/viewer")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxTarball+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTarball {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, maxTarball)
	}
	return body, nil
}

// extract writes the files the library keeps of a tarball below out, each
// compressed with gzip the same way every time, so that the same tarball
// always gives the same files.
func extract(tarball []byte, lib library, out string) (int, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return 0, err
	}
	archive := tar.NewReader(unzipped)
	files := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := strings.CutPrefix(path.Clean(header.Name), "package/")
		if !ok {
			continue
		}
		placed := lib.place(name)
		if placed == "" {
			continue
		}
		local := filepath.FromSlash(path.Join(lib.folder, placed))
		if !filepath.IsLocal(local) {
			return files, fmt.Errorf("%s leaves the output folder", header.Name)
		}
		if err := writeCompressed(filepath.Join(out, local+".gz"), archive); err != nil {
			return files, err
		}
		files++
	}
	if files == 0 {
		return 0, errors.New("the tarball has none of the files the viewer needs")
	}
	return files, nil
}

func writeCompressed(target string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	// a header with neither a name nor a time, so the bytes depend on the
	// content alone
	compressed, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		_ = f.Close()
		return err
	}
	_, cerr := io.Copy(compressed, body)
	zerr := compressed.Close()
	ferr := f.Close()
	return errors.Join(cerr, zerr, ferr)
}

// writeVersions writes the versions file, the last thing a fetch does.
func writeVersions(out, stamp string, pins []pin) error {
	var b strings.Builder
	b.WriteString(stampPrefix + stamp + "\n")
	b.WriteString("# written by go run ./tools/viewer from internal/httpd/viewer/libraries.lock\n")
	for _, p := range pins {
		fmt.Fprintf(&b, "%s %s %s\n", p.name, p.version, p.integrity)
	}
	return os.WriteFile(filepath.Join(out, versionsName), []byte(b.String()), 0o644)
}

// pinLatest rewrites the lock file with the version npm tags latest for each
// library, or the tag it names instead, and the integrity of its tarball, keeping its comments, and says
// what changed.
func pinLatest(client *http.Client, registry, lockPath string) error {
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	pins, err := parseLock(lock)
	if err != nil {
		return fmt.Errorf("%s: %w", lockPath, err)
	}
	latest := map[string]pin{}
	for _, p := range pins {
		next, err := latestOf(client, registry, p.name, libraries[p.name].tagOf())
		if err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
		latest[p.name] = next
		if next.version == p.version {
			fmt.Printf("viewer: %s %s is the %s\n", p.name, p.version, libraries[p.name].tagOf())
		} else {
			fmt.Printf("viewer: %s %s -> %s\n", p.name, p.version, next.version)
		}
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(lock), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && !strings.HasPrefix(fields[0], "#") {
			next := latest[fields[0]]
			fmt.Fprintf(&b, "%s %s %s\n", next.name, next.version, next.integrity)
			continue
		}
		b.WriteString(line)
	}
	return os.WriteFile(lockPath, []byte(b.String()), 0o644)
}

// latestOf asks the registry for the version of a package it gives the tag.
func latestOf(client *http.Client, registry, name, tag string) (pin, error) {
	body, err := get(client, registry+"/"+name)
	if err != nil {
		return pin{}, err
	}
	var doc struct {
		Tags     map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return pin{}, err
	}
	version := doc.Tags[tag]
	integrity := doc.Versions[version].Dist.Integrity
	if version == "" {
		return pin{}, fmt.Errorf("the registry tags no version %s", tag)
	}
	if _, err := digestOf(integrity); err != nil {
		return pin{}, fmt.Errorf("%s: %w", version, err)
	}
	return pin{name: name, version: version, integrity: integrity}, nil
}
