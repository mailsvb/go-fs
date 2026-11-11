//go:build !light && !no_nbd

package nbd

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The calls an image makes below its reads and writes are traced.
func TestTheImageTracesItsCalls(t *testing.T) {
	var out bytes.Buffer
	file, err := os.Create(filepath.Join(t.TempDir(), "d.raw"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	img := fileImage{File: file, log: slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if err := img.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = img.ZeroRange(0, 4096)
	_, _ = img.Extents(0, 1<<20)
	for _, want := range []string{`msg="syscall fsync"`, `msg="syscall zero range" offset=0 length=4096`, `msg="syscall seek data" offset=0`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the trace lacks %q:\n%s", want, out.String())
		}
	}
}
