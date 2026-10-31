package syncer

import (
	"slices"
	"testing"
	"time"
)

var (
	early = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	late  = early.Add(time.Hour)
)

func file(size int64, mod time.Time) entry { return entry{Size: size, Mod: mod} }

var folder = entry{Dir: true}

// paths are the paths of steps, and the side each is on.
func paths(steps []step) []string {
	var out []string
	for _, s := range steps {
		side := "local:"
		if s.remote {
			side = "remote:"
		}
		out = append(out, side+s.path)
	}
	return out
}

func want(t *testing.T, what string, got []step, expected ...string) {
	t.Helper()
	if !slices.Equal(paths(got), expected) {
		t.Errorf("%s are %v, want %v", what, paths(got), expected)
	}
}

func TestSame(t *testing.T) {
	if !same(file(3, early), file(3, early.Add(900*time.Millisecond))) {
		t.Error("times within a second are not the same")
	}
	if same(file(3, early), file(3, early.Add(2*time.Second))) {
		t.Error("times two seconds apart are the same")
	}
	if same(file(3, early), file(4, early)) {
		t.Error("sizes that differ are the same")
	}
	if same(file(0, early), folder) || !same(folder, folder) {
		t.Error("a folder is told apart from a file wrongly")
	}
}

func TestPlanOneWay(t *testing.T) {
	src := map[string]entry{
		"a.txt":     file(1, early),
		"new":       folder,
		"new/b.txt": file(2, early),
		"same.txt":  file(3, early),
		"clash":     file(4, early),
	}
	dst := map[string]entry{
		"a.txt":          file(1, late),
		"same.txt":       file(3, early),
		"clash":          folder,
		"gone":           folder,
		"gone/deep":      folder,
		"gone/deep/x":    file(5, early),
		"gone/other.txt": file(6, early),
	}

	p := planOneWay(src, dst, true, false)
	want(t, "folders made", p.mkdirs, "remote:new")
	want(t, "copies", p.copies, "remote:a.txt", "remote:new/b.txt")
	want(t, "removals", p.removes)
	want(t, "folders removed", p.rmdirs)
	if !slices.Equal(p.clashes, []string{"clash"}) {
		t.Errorf("clashes are %v", p.clashes)
	}

	p = planOneWay(src, dst, false, true)
	want(t, "removals", p.removes, "local:gone/deep/x", "local:gone/other.txt")
	want(t, "folders removed", p.rmdirs, "local:gone/deep", "local:gone")
}

func TestPlanBoth(t *testing.T) {
	last := map[string]entry{
		"kept.txt":            file(1, early),
		"local-changed.txt":   file(1, early),
		"remote-changed.txt":  file(1, early),
		"both-changed.txt":    file(1, early),
		"removed-remote.txt":  file(1, early),
		"removed-local.txt":   file(1, early),
		"changed-removed.txt": file(1, early),
		"old":                 folder,
		"old/x.txt":           file(1, early),
		"busy":                folder,
		"busy/x.txt":          file(1, early),
	}
	local := map[string]entry{
		"kept.txt":            file(1, early),
		"local-changed.txt":   file(2, late),
		"remote-changed.txt":  file(1, early),
		"both-changed.txt":    file(2, early.Add(10*time.Minute)),
		"removed-remote.txt":  file(1, early),
		"changed-removed.txt": file(9, late),
		"new-local.txt":       file(1, late),
		"old":                 folder,
		"old/x.txt":           file(1, early),
		"busy":                folder,
		"busy/x.txt":          file(1, early),
		"busy/new.txt":        file(1, late),
	}
	remote := map[string]entry{
		"kept.txt":           file(1, early),
		"local-changed.txt":  file(1, early),
		"remote-changed.txt": file(3, late),
		"both-changed.txt":   file(3, late),
		"removed-local.txt":  file(1, early),
		"new-remote.txt":     file(1, late),
	}

	p := planBoth(local, remote, last, true)
	want(t, "copies", p.copies,
		"local:both-changed.txt", "remote:busy/new.txt", "remote:changed-removed.txt",
		"remote:local-changed.txt", "remote:new-local.txt", "local:new-remote.txt", "local:remote-changed.txt")
	want(t, "removals", p.removes,
		"local:busy/x.txt", "local:old/x.txt", "remote:removed-local.txt", "local:removed-remote.txt")
	// busy keeps the new file, so it is made on the remote side rather than
	// removed locally, while the file removed on the remote side goes
	want(t, "folders made", p.mkdirs, "remote:busy")
	want(t, "folders removed", p.rmdirs, "local:old")
	if !slices.Equal(p.conflicts, []string{"both-changed.txt"}) {
		t.Errorf("conflicts are %v", p.conflicts)
	}
	if _, ok := p.insync["kept.txt"]; !ok || len(p.insync) != 1 {
		t.Errorf("in sync are %v", p.insync)
	}

	// without delete, or without a last run, nothing is removed
	for _, p := range []plan{planBoth(local, remote, last, false), planBoth(local, remote, nil, true)} {
		want(t, "removals", p.removes)
		want(t, "folders removed", p.rmdirs)
	}
	// and without a last run, a file that differs is no conflict: the
	// newer is kept
	p = planBoth(local, remote, nil, true)
	if len(p.conflicts) != 0 {
		t.Errorf("a first run has conflicts: %v", p.conflicts)
	}
	if !slices.Contains(paths(p.copies), "remote:removed-remote.txt") {
		t.Errorf("a first run does not copy what one side alone has: %v", paths(p.copies))
	}
}

func TestPlanBothTieGoesToLocal(t *testing.T) {
	local := map[string]entry{"x": file(1, early)}
	remote := map[string]entry{"x": file(2, early)}
	p := planBoth(local, remote, nil, false)
	want(t, "copies", p.copies, "remote:x")
}
