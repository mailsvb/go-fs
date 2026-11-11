package store

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every operation of a bucket is traced at debug with its key and its size,
// naming the line that called it, and never with the value.
func TestOperationsAreTraced(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := open(filepath.Join(t.TempDir(), "go-fs.db"), log, time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := s.Bucket("shares")
	if err := b.Put("abc", share{Path: "/docs", Owner: "valueowner"}); err != nil {
		t.Fatal(err)
	}
	var got share
	if _, err := b.Get("abc", &got); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("abc"); err != nil {
		t.Fatal(err)
	}

	trace := out.String()
	for _, want := range []string{`msg="store put" bucket=shares key=abc`, `msg="store get" bucket=shares key=abc found=true`,
		`msg="store delete" bucket=shares key=abc`} {
		if !strings.Contains(trace, want) {
			t.Errorf("the trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "valueowner") {
		t.Errorf("the trace holds a value:\n%s", trace)
	}
}
