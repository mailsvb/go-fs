// Package vfs confines path operations to a base folder.
//
// Both servers expose a virtual filesystem to their clients: the client sees
// paths below "/", which are mapped onto a configured base folder. Every path
// a client supplies has to be normalized and then checked, so that neither
// ".." segments nor symbolic links can reach out of that folder.
package vfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

// Root is a base folder that client paths are resolved against.
type Root struct {
	base string
	// folds says the filesystem under base ignores case, so that two
	// spellings of a path which differ only in case name the same file.
	folds bool
	// reserved is the resolved folder ReservedName at the top of the served
	// tree, which no path of the root reaches.
	reserved string
}

// ReservedName is the folder at the top of the served tree that go-fs keeps
// for itself. HTTP answers its fixed content, the document viewer for one,
// below /.go-fs/, so a folder of that name in the tree could never be reached
// over HTTP anyway; every protocol refuses it and leaves it out of listings,
// so that nobody puts files where only some clients would see them.
const ReservedName = ".go-fs"

// Option adjusts a Root.
type Option func(*Root)

// Served says which folder is the top of the served tree, when it is not the
// base of the root: an account of FTP or SFTP that sees a folder of its own
// still must not reach the reserved folder of the tree, wherever it sits
// below its base.
func Served(folder string) Option {
	return func(r *Root) {
		if resolved, err := resolveFolder(folder); err == nil {
			r.reserved = filepath.Join(resolved, ReservedName)
		}
	}
}

// New resolves base, following symbolic links, and returns a Root for it. The
// folder must exist, otherwise nothing can be confined to it. The base is the
// top of the served tree unless Served says otherwise.
func New(base string, options ...Option) (*Root, error) {
	if base == "" {
		return nil, errors.New("base folder is not set")
	}
	resolved, err := resolveFolder(base)
	if err != nil {
		return nil, err
	}
	root := &Root{base: resolved, folds: foldsCase(resolved), reserved: filepath.Join(resolved, ReservedName)}
	for _, option := range options {
		option(root)
	}
	return root, nil
}

// resolveFolder makes folder absolute and follows its symbolic links, once, so
// that a symlinked base (/tmp and /var are symlinks on macOS) still compares
// equal to the resolved targets below it.
func resolveFolder(folder string) (string, error) {
	absolute, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New(absolute + " is not a folder")
	}
	return filepath.EvalSymlinks(absolute)
}

// Base returns the resolved base folder.
func (r *Root) Base() string {
	return r.base
}

// CaseInsensitive reports whether the filesystem under the base folder ignores
// case. Anything that decides by the spelling of a path, as the HTTP path
// patterns do, has to ignore it too, or /PRIVATE reaches what /private
// protects.
func (r *Root) CaseInsensitive() bool {
	return r.folds
}

// foldsCase asks the filesystem rather than guessing from the platform: macOS
// can be formatted case sensitive, and Linux can mount a folder that is not.
// The closest component of the path that has a letter in it is looked up with
// its case swapped; the filesystem folds case if that finds the same folder.
// A path without a single letter leaves only the platform's default to go by.
func foldsCase(folder string) bool {
	for current := folder; ; {
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		name := filepath.Base(current)
		if swapped := swapCase(name); swapped != name {
			original, err := os.Stat(current)
			if err != nil {
				return false
			}
			other, err := os.Stat(filepath.Join(parent, swapped))
			return err == nil && os.SameFile(original, other)
		}
		current = parent
	}
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func swapCase(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		return unicode.ToUpper(r)
	}, name)
}

// Target is a client supplied path resolved against a Root.
type Target struct {
	// Virtual is the normalized path as the client sees it, always starting
	// with a slash.
	Virtual string
	// Path is the location on the filesystem. Only meaningful when Valid.
	Path string
	// Valid reports whether the path stays inside the base folder.
	Valid bool
}

// Resolve normalizes arg against the current working directory cwd and maps it
// onto the filesystem.
func (r *Root) Resolve(cwd, arg string) Target {
	virtual, escaped := Normalize(cwd, arg)
	osPath := filepath.Join(r.base, filepath.FromSlash(virtual))
	resolved := resolveReal(osPath)
	return Target{
		Virtual: virtual,
		Path:    osPath,
		Valid: !escaped && segmentsAllowed(virtual) && r.within(resolved) &&
			!r.spellsReserved(virtual) && !r.isReserved(resolved),
	}
}

// onWindows is read once: it decides which names are other spellings of a path.
var onWindows = runtime.GOOS == "windows"

func segmentsAllowed(virtual string) bool {
	for _, segment := range strings.Split(virtual, "/") {
		if !segmentAllowed(segment, onWindows) {
			return false
		}
	}
	return true
}

// segmentAllowed reports whether one segment of a normalized path names what
// it appears to. On Windows a backslash separates folders as a slash does, a
// colon opens a drive or an alternate data stream, and a trailing dot or space
// is dropped from a name, so "private\x", "private." and "x::$DATA" all reach
// a path the client did not spell, past any pattern that decides by the
// spelling. Elsewhere these are ordinary characters in a name.
func segmentAllowed(segment string, windows bool) bool {
	if !windows {
		return true
	}
	if strings.ContainsAny(segment, `\:`) {
		return false
	}
	return !strings.HasSuffix(segment, ".") && !strings.HasSuffix(segment, " ")
}

// Contains reports whether osPath stays inside the base folder once symbolic
// links have been resolved. A link inside the base folder that points out of it
// is therefore rejected.
func (r *Root) Contains(osPath string) bool {
	return r.within(resolveReal(osPath))
}

func (r *Root) within(resolved string) bool {
	if resolved == r.base {
		return true
	}
	return strings.HasPrefix(resolved, r.base+string(filepath.Separator))
}

// Reserved reports whether osPath is the reserved folder of the served tree or
// below it, once symbolic links have been resolved, so that neither a link to
// it nor another spelling of it on a filesystem that ignores case gets there.
func (r *Root) Reserved(osPath string) bool {
	return r.isReserved(resolveReal(osPath))
}

func (r *Root) isReserved(resolved string) bool {
	if r.reserved == "" || len(resolved) < len(r.reserved) {
		return false
	}
	head, rest := resolved[:len(r.reserved)], resolved[len(r.reserved):]
	if rest != "" && rest[0] != filepath.Separator {
		return false
	}
	if r.folds {
		return strings.EqualFold(head, r.reserved)
	}
	return head == r.reserved
}

// spellsReserved catches the reserved folder by the name a client wrote, at
// the top of a root that is the served tree, before anything is resolved.
func (r *Root) spellsReserved(virtual string) bool {
	if r.reserved != filepath.Join(r.base, ReservedName) {
		return false
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(virtual, "/"), "/")
	return r.sameName(first, ReservedName)
}

func (r *Root) sameName(a, b string) bool {
	if r.folds {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Hides reports whether entry, listed in the folder dir, is to be left out of
// what a client is shown: the reserved folder, or a link to it or into it.
// Only an entry of the reserved name or a link costs a resolve, so a large
// folder lists as fast as before.
func (r *Root) Hides(dir string, entry fs.DirEntry) bool {
	if r.reserved == "" {
		return false
	}
	if entry.Type()&fs.ModeSymlink == 0 && !r.sameName(entry.Name(), ReservedName) {
		return false
	}
	return r.Reserved(filepath.Join(dir, entry.Name()))
}

// ReadDir lists the folder of a target as os.ReadDir does, less what Hides
// leaves out.
func (r *Root) ReadDir(t Target) ([]os.DirEntry, error) {
	return r.ReadFolder(t.Path)
}

// ReadFolder is ReadDir for a folder given by its path on the filesystem.
func (r *Root) ReadFolder(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	shown := entries[:0]
	for _, entry := range entries {
		if !r.Hides(dir, entry) {
			shown = append(shown, entry)
		}
	}
	return shown, err
}

// Normalize resolves "." and ".." segments of a client supplied path against
// the current working directory. It reports separately whether the path tried
// to climb above the root, so that callers can refuse it instead of silently
// working on a clamped path.
func Normalize(cwd, arg string) (virtual string, escaped bool) {
	raw := arg
	if !strings.HasPrefix(arg, "/") {
		raw = cwd + "/" + arg
	}
	stack := make([]string, 0, 8)
	for _, segment := range strings.Split(raw, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "/", true
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, segment)
		}
	}
	return "/" + strings.Join(stack, "/"), false
}

// IsRoot reports whether the target is the base folder itself. It is the one
// path a client may see but must not remove, rename or change the mode of:
// deleting it would take the served folder with it.
func (t Target) IsRoot() bool {
	return t.Virtual == "/"
}

// AsFolder returns virtual with a trailing slash.
func AsFolder(virtual string) string {
	if strings.HasSuffix(virtual, "/") {
		return virtual
	}
	return virtual + "/"
}

// resolveReal follows symbolic links for the containment check. A path that
// does not exist yet, as when a file is about to be created, is resolved
// through its closest existing ancestor.
func resolveReal(target string) string {
	current := filepath.Clean(target)
	var missing []string
	for {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			if len(missing) == 0 {
				return real
			}
			slices.Reverse(missing)
			return filepath.Join(append([]string{real}, missing...)...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(target)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
