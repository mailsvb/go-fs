package syncer

import (
	"errors"
	"path"
	"slices"
	"strings"
	"time"
)

// entry is a file or a folder of one side, by its path from the top of the
// folder that is synced.
type entry struct {
	Size int64     `json:"size,omitempty"`
	Mod  time.Time `json:"mod,omitzero"`
	Dir  bool      `json:"dir,omitempty"`
}

// window is how far apart two modification times may be and still be the
// same: SFTP carries whole seconds.
const window = time.Second

// same reports whether two entries are the same file or both folders, as
// rsync tells them apart without reading them: by their size and their
// modification time.
func same(a, b entry) bool {
	if a.Dir || b.Dir {
		return a.Dir == b.Dir
	}
	diff := a.Mod.Sub(b.Mod)
	return a.Size == b.Size && diff <= window && diff >= -window
}

// step is one thing a run does. remote says which side it is done on: the
// side a folder is made or something removed on, or the side a file is
// copied to.
type step struct {
	path   string
	remote bool
	entry  entry
}

// plan is what a run does, in the order it does it: the folders are made
// first, from the top down, and removed last, from the bottom up.
type plan struct {
	mkdirs  []step
	copies  []step
	removes []step
	rmdirs  []step
	// clashes are the paths that are a file on one side and a folder on the
	// other, which are left as they are.
	clashes []string
	// conflicts are the files changed on both sides since the last run, of
	// which the newer was kept.
	conflicts []string
	// insync are the paths already alike on both sides, which the snapshot of
	// a run both ways starts from.
	insync map[string]entry
}

// errClash is a path that is a file on one side and a folder on the other.
var errClash = errors.New("is a file on one side and a folder on the other")

// planOneWay brings dst in line with src: what src has that dst has not, or
// has otherwise, is copied, and with del what dst has that src has not is
// removed.
func planOneWay(src, dst map[string]entry, toRemote, del bool) plan {
	p := plan{insync: map[string]entry{}}
	for _, rel := range sortedPaths(src) {
		s := src[rel]
		d, there := dst[rel]
		switch {
		case there && s.Dir != d.Dir:
			p.clashes = append(p.clashes, rel)
		case s.Dir && !there:
			p.mkdirs = append(p.mkdirs, step{path: rel, remote: toRemote, entry: s})
		case !s.Dir && (!there || !same(s, d)):
			p.copies = append(p.copies, step{path: rel, remote: toRemote, entry: s})
		default:
			p.insync[rel] = s
		}
	}
	for _, rel := range sortedPaths(dst) {
		if _, ok := src[rel]; ok || !del {
			continue
		}
		d := dst[rel]
		if d.Dir {
			p.rmdirs = append(p.rmdirs, step{path: rel, remote: toRemote, entry: d})
		} else {
			p.removes = append(p.removes, step{path: rel, remote: toRemote, entry: d})
		}
	}
	p.order()
	return p
}

// planBoth brings both sides in line with each other. last is what was
// alike on both after the previous run, nil before the first one: a file
// changed on one side since then is copied to the other, and of one changed
// on both the newer is kept. With del, what one side has, as it was at the
// last run, and the other no longer has, was removed there, and is removed
// here too; anything else one side alone has is copied to the other.
func planBoth(local, remote, last map[string]entry, del bool) plan {
	p := plan{insync: map[string]entry{}}
	// kept are the folders something is kept in, which are not removed
	kept := map[string]bool{}
	keep := func(rel string) {
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			kept[dir] = true
		}
	}
	var dirs []string
	for _, rel := range sortedPaths(local, remote) {
		l, onLocal := local[rel]
		r, onRemote := remote[rel]
		s, known := last[rel]
		if onLocal && onRemote && l.Dir != r.Dir {
			p.clashes = append(p.clashes, rel)
			keep(rel)
			continue
		}
		if l.Dir || r.Dir {
			dirs = append(dirs, rel)
			continue
		}
		switch {
		case onLocal && onRemote:
			keep(rel)
			if same(l, r) {
				p.insync[rel] = l
				continue
			}
			localChanged := !known || !same(l, s)
			remoteChanged := !known || !same(r, s)
			switch {
			case localChanged && !remoteChanged:
				p.copies = append(p.copies, step{path: rel, remote: true, entry: l})
			case remoteChanged && !localChanged:
				p.copies = append(p.copies, step{path: rel, remote: false, entry: r})
			default:
				if known {
					p.conflicts = append(p.conflicts, rel)
				}
				// the newer wins, and the local file when neither is
				if r.Mod.After(l.Mod.Add(window)) {
					p.copies = append(p.copies, step{path: rel, remote: false, entry: r})
				} else {
					p.copies = append(p.copies, step{path: rel, remote: true, entry: l})
				}
			}
		case onLocal:
			if del && known && same(l, s) {
				p.removes = append(p.removes, step{path: rel, remote: false, entry: l})
				continue
			}
			keep(rel)
			p.copies = append(p.copies, step{path: rel, remote: true, entry: l})
		case onRemote:
			if del && known && same(r, s) {
				p.removes = append(p.removes, step{path: rel, remote: true, entry: r})
				continue
			}
			keep(rel)
			p.copies = append(p.copies, step{path: rel, remote: false, entry: r})
		}
	}
	// a folder is decided once what is in it is, from the bottom up
	slices.SortFunc(dirs, deepestFirst)
	for _, rel := range dirs {
		_, onLocal := local[rel]
		_, onRemote := remote[rel]
		_, known := last[rel]
		switch {
		case onLocal && onRemote:
			p.insync[rel] = entry{Dir: true}
			keep(rel)
		case kept[rel] || !del || !known:
			p.mkdirs = append(p.mkdirs, step{path: rel, remote: onLocal, entry: entry{Dir: true}})
			keep(rel)
		default:
			p.rmdirs = append(p.rmdirs, step{path: rel, remote: onRemote, entry: entry{Dir: true}})
		}
	}
	p.order()
	return p
}

// order puts the folders to make from the top down and those to remove
// from the bottom up.
func (p *plan) order() {
	slices.SortFunc(p.mkdirs, func(a, b step) int { return -deepestFirst(a.path, b.path) })
	slices.SortFunc(p.rmdirs, func(a, b step) int { return deepestFirst(a.path, b.path) })
}

// deepestFirst orders paths by how deep they are, the deepest first, and
// then by name.
func deepestFirst(a, b string) int {
	if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
		return db - da
	}
	return strings.Compare(a, b)
}

// sortedPaths are the paths of the listings, each once, in order, so that a
// run does what it does in the same order every time.
func sortedPaths(listings ...map[string]entry) []string {
	seen := map[string]bool{}
	var paths []string
	for _, listing := range listings {
		for rel := range listing {
			if !seen[rel] {
				seen[rel] = true
				paths = append(paths, rel)
			}
		}
	}
	slices.Sort(paths)
	return paths
}
