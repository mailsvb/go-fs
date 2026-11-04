package vfs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		cwd, arg string
		want     string
		escaped  bool
	}{
		{"/", "file.txt", "/file.txt", false},
		{"/", "/file.txt", "/file.txt", false},
		{"/sub/", "file.txt", "/sub/file.txt", false},
		{"/sub/", "/file.txt", "/file.txt", false},
		{"/", ".", "/", false},
		{"/", "./sub/.", "/sub", false},
		{"/sub/", "..", "/", false},
		{"/a/b/", "../c", "/a/c", false},
		{"/", "..", "/", true},
		{"/", "../../etc/passwd", "/", true},
		{"/a/", "../../b", "/", true},
		{"/", "sub//file", "/sub/file", false},
		{"/", "", "/", false},
	}
	for _, tc := range cases {
		got, escaped := Normalize(tc.cwd, tc.arg)
		if got != tc.want || escaped != tc.escaped {
			t.Errorf("Normalize(%q, %q) = (%q, %v), want (%q, %v)",
				tc.cwd, tc.arg, got, escaped, tc.want, tc.escaped)
		}
	}
}

func TestResolveStaysInsideBase(t *testing.T) {
	base := t.TempDir()
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}

	inside := root.Resolve("/", "file.txt")
	if !inside.Valid {
		t.Errorf("a plain name should be valid, got %+v", inside)
	}
	if inside.Virtual != "/file.txt" {
		t.Errorf("virtual = %q, want /file.txt", inside.Virtual)
	}

	for _, arg := range []string{"../escape", "../../etc/passwd", "/../escape"} {
		if target := root.Resolve("/", arg); target.Valid {
			t.Errorf("%q should not be valid, got %+v", arg, target)
		}
	}
}

func TestContainsRefusesSymlinkOutOfBase(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(base, "link.txt")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}

	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}

	if target := root.Resolve("/", "link.txt"); target.Valid {
		t.Error("a symlink pointing out of the base folder must not be valid")
	}
	// and a file that does not exist yet below a symlinked folder
	if target := root.Resolve("/", "escape/planted.txt"); target.Valid {
		t.Error("writing through a symlinked folder must not be valid")
	}
}

func TestContainsAllowsSymlinkInsideBase(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "real", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if target := root.Resolve("/", "link/f.txt"); !target.Valid {
		t.Error("a symlink that stays inside the base folder should be valid")
	}
}

func TestResolveNewFileBelowMissingParents(t *testing.T) {
	base := t.TempDir()
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	// nothing of a/b/c exists yet, it still has to resolve inside the base
	if target := root.Resolve("/", "a/b/c.txt"); !target.Valid {
		t.Errorf("a path below missing folders should be valid, got %+v", target)
	}
}

func TestNewRejectsMissingBase(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("a missing base folder should be an error")
	}
	if _, err := New(""); err == nil {
		t.Error("an empty base folder should be an error")
	}
}

// On Windows a backslash and a colon are separators, and a trailing dot or
// space is dropped from a name, so each is another spelling of a path the
// client did not write. Elsewhere they are ordinary name characters.
func TestSegmentAllowed(t *testing.T) {
	cases := []struct {
		segment string
		windows bool
	}{
		{"plain.txt", true},
		{"with space.txt", true},
		{".hidden", true},
		{"a\\b", false},
		{"file.txt::$DATA", false},
		{"c:", false},
		{"private.", false},
		{"private ", false},
		{"...", false},
	}
	for _, tc := range cases {
		if got := segmentAllowed(tc.segment, true); got != tc.windows {
			t.Errorf("segmentAllowed(%q, windows) = %v, want %v", tc.segment, got, tc.windows)
		}
		if !segmentAllowed(tc.segment, false) {
			t.Errorf("segmentAllowed(%q, other) = false, want true", tc.segment)
		}
	}
}

func TestResolveRefusesWindowsVariants(t *testing.T) {
	root, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{`/private\secret.txt`, "/private./secret.txt", "/file.txt::$DATA"} {
		target := root.Resolve("/", arg)
		if want := runtime.GOOS != "windows"; target.Valid != want {
			t.Errorf("Resolve(%q).Valid = %v, want %v", arg, target.Valid, want)
		}
	}
}

// CaseInsensitive has to agree with what the filesystem actually does.
func TestCaseInsensitiveMatchesTheFilesystem(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Served")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(filepath.Dir(base), "sERVED"))
	if want := statErr == nil; root.CaseInsensitive() != want {
		t.Errorf("CaseInsensitive() = %v, the filesystem says %v", root.CaseInsensitive(), want)
	}
}

// The folder ReservedName at the top of the served tree is go-fs's own: no
// spelling of it, and no link to it, is a valid path, while the same name
// further down is an ordinary folder.
func TestResolveRefusesTheReservedFolder(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ReservedName, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"/.go-fs", "/.go-fs/", "/.go-fs/inside", "/.go-fs/new.txt", "/sub/../.go-fs/x", ".go-fs"} {
		if target := root.Resolve("/", arg); target.Valid {
			t.Errorf("Resolve(%q) is valid, want the reserved folder refused", arg)
		}
	}
	for _, arg := range []string{"/sub/.go-fs", "/sub/.go-fs/x", "/.go-fs-other", "/.go-fsx/y", "/go-fs"} {
		if target := root.Resolve("/", arg); !target.Valid {
			t.Errorf("Resolve(%q) is refused, want it valid", arg)
		}
	}
	// another case is the same folder exactly where the filesystem says so
	upper := root.Resolve("/", "/.GO-FS/inside")
	if want := !root.CaseInsensitive(); upper.Valid != want {
		t.Errorf("Resolve(/.GO-FS/inside).Valid = %v, want %v on a filesystem that folds case: %v",
			upper.Valid, want, root.CaseInsensitive())
	}
}

func TestResolveRefusesALinkIntoTheReservedFolder(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ReservedName, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, ReservedName), filepath.Join(base, "door")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"/door", "/door/inside", "/door/planted.txt"} {
		if target := root.Resolve("/", arg); target.Valid {
			t.Errorf("Resolve(%q) is valid through a link into the reserved folder", arg)
		}
	}
}

// A symlinked base, as /tmp is on macOS, reserves the folder of the resolved
// base and still refuses it by the name the client wrote.
func TestResolveRefusesTheReservedFolderOfALinkedBase(t *testing.T) {
	real := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	root, err := New(linked)
	if err != nil {
		t.Fatal(err)
	}
	if target := root.Resolve("/", "/.go-fs/x"); target.Valid {
		t.Error("the reserved folder of a linked base is valid")
	}
}

// An account of FTP or SFTP may see a folder above the served tree; the
// reserved folder of the tree is refused to it all the same, by the path it
// has there, while a folder of that name at the top of its own base is not
// the tree's and stays its own.
func TestServedReservesTheFolderOfTheTreeForAnAccountRoot(t *testing.T) {
	account := t.TempDir()
	served := filepath.Join(account, "served")
	if err := os.MkdirAll(filepath.Join(served, ReservedName), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := New(account, Served(served))
	if err != nil {
		t.Fatal(err)
	}
	if target := root.Resolve("/", "/served/.go-fs/x"); target.Valid {
		t.Error("the reserved folder of the served tree is valid from an account root above it")
	}
	if target := root.Resolve("/", "/.go-fs/x"); !target.Valid {
		t.Error("a .go-fs at the top of an account root that is not the served tree is refused")
	}
}

func TestReadDirHidesTheReservedFolder(t *testing.T) {
	base := t.TempDir()
	for _, folder := range []string{ReservedName, "kept", filepath.Join("kept", ReservedName)} {
		if err := os.Mkdir(filepath.Join(base, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := os.Symlink(filepath.Join(base, ReservedName), filepath.Join(base, "door")) == nil
	root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	names := func(arg string) []string {
		entries, err := root.ReadDir(root.Resolve("/", arg))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		return got
	}
	if got := names("/"); len(got) != 1 || got[0] != "kept" {
		t.Errorf("the top of the tree lists %v, want only kept (links: %v)", got, links)
	}
	if got := names("/kept"); len(got) != 1 || got[0] != ReservedName {
		t.Errorf("a folder below lists %v, want its own %s", got, ReservedName)
	}
}
