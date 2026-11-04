package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The lock file of the repository pins every library the tool knows, and
// nothing else.
func TestTheLockFileParses(t *testing.T) {
	lock, err := os.ReadFile(filepath.Join("..", "..", "internal", "httpd", "viewer", "libraries.lock"))
	if err != nil {
		t.Fatal(err)
	}
	pins, err := parseLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(libraries) {
		t.Errorf("%d pins for %d libraries", len(pins), len(libraries))
	}
}

// Of Video.js only the core, its style and its licence are kept, and an
// update follows the line of version 8, never the stub of 10 that is latest.
func TestVideoJSKeepsTheCore(t *testing.T) {
	lib := libraries["video.js"]
	if lib.tagOf() != "latest-8" {
		t.Errorf("an update of video.js follows %q", lib.tagOf())
	}
	cases := map[string]string{
		"LICENSE":                     "LICENSE",
		"dist/alt/video.core.min.js":  "video.core.min.js",
		"dist/video-js.min.css":       "video-js.min.css",
		"dist/video.min.js":           "",
		"dist/alt/video.core.js":      "",
		"dist/alt/video.novtt.min.js": "",
		"dist/video-js.css":           "",
		"dist/lang/de.json":           "",
		"dist/font/VideoJS.woff":      "",
		"dist/types/video.d.ts":       "",
		"package.json":                "",
	}
	for name, want := range cases {
		if got := lib.place(name); got != want {
			t.Errorf("place(%q) = %q, want %q", name, got, want)
		}
	}
	if libraries["pdfjs-dist"].tagOf() != "latest" {
		t.Error("a library that names no tag does not follow latest")
	}
}
