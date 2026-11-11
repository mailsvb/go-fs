package logging

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each traced call does what the os call does and writes one record that
// names the line that called it.
func TestFileCallsAreTraced(t *testing.T) {
	var out bytes.Buffer
	log := NewTo(&out, "debug", "json").Logger
	dir := t.TempDir()
	name := filepath.Join(dir, "a.txt")

	wantWrite := here()
	if err := WriteFile(log, name, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stat(log, name); err != nil {
		t.Fatal(err)
	}
	if err := Rename(log, name, name+".old"); err != nil {
		t.Fatal(err)
	}
	if _, err := Stat(log, name); !os.IsNotExist(err) {
		t.Fatalf("the rename did not happen: %v", err)
	}
	visited := 0
	if err := WalkDir(log, dir, func(string, fs.DirEntry, error) error { visited++; return nil }); err != nil {
		t.Fatal(err)
	}

	got := records(t, &out)
	if len(got) != 5 {
		t.Fatalf("records = %v", got)
	}
	if got[0]["msg"] != "file write" || got[0]["bytes"] != float64(5) || got[0]["source"] != wantWrite {
		t.Errorf("write record = %v, want source %s", got[0], wantWrite)
	}
	if got[1]["msg"] != "file stat" || got[1]["size"] != float64(5) {
		t.Errorf("stat record = %v", got[1])
	}
	if got[2]["msg"] != "file rename" || got[2]["to"] != name+".old" {
		t.Errorf("rename record = %v", got[2])
	}
	if got[3]["error"] == nil {
		t.Errorf("a failed stat does not say why: %v", got[3])
	}
	if got[4]["msg"] != "file walk" || got[4]["entries"] != float64(visited) {
		t.Errorf("walk record = %v, visited %d", got[4], visited)
	}
}

func TestOpenFlagsAreNamed(t *testing.T) {
	if got := openFlags(os.O_WRONLY | os.O_CREATE | os.O_EXCL); got != "WRONLY|CREATE|EXCL" {
		t.Errorf("openFlags = %q", got)
	}
	if got := openFlags(os.O_RDONLY); got != "RDONLY" {
		t.Errorf("openFlags = %q", got)
	}
}

func TestSummaryNamesTheVersionOfAFile(t *testing.T) {
	var out bytes.Buffer
	log := NewTo(&out, "debug", "json").Logger
	name := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(name, []byte("[general]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Summary(log, "read", name)
	got := records(t, &out)
	if len(got) != 1 || got[0]["msg"] != "file read" || got[0]["bytes"] != float64(10) ||
		len(got[0]["sha256"].(string)) != 12 || !strings.HasPrefix(got[0]["source"].(string), "fs_test.go:") {
		t.Errorf("record = %v", got)
	}
}
