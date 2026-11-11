package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/store"
)

// runner keeps one repository.
type runner struct {
	repo config.Repository
	// base is the folder the http server serves, and folder the one of the
	// repository on disk.
	base   string
	folder string
	kind   kind
	log    *slog.Logger
	// cache keeps the packages read and what the metadata lists across a
	// restart, so that a restart reads only what changed meanwhile.
	cache *store.Bucket

	// quit is closed to stop the runner, and done once it has stopped.
	quit     chan struct{}
	done     chan struct{}
	launched bool

	// known are the packages read so far, by their path in the repository,
	// so that a package is read once rather than at every look.
	known map[string]known
	// listed is what the metadata written last lists, "" before the first
	// time.
	listed string
	// skipped are the files left out at the last look, with the reason,
	// so that a reason is logged when it first comes up and not every look.
	skipped map[string]string
	status  Status
}

// known is a package as it was when it was read.
type known struct {
	size    int64
	modTime time.Time
	entry   entry
}

// newRunner makes the runner of repo, which starts from what cache kept of
// the runs before when it runs.
func newRunner(repo config.Repository, base string, cache *store.Bucket, logger *slog.Logger) (*runner, error) {
	folder := config.RepositoryPath(base, repo.Folder)
	log := logger.With("repository", repo.Name)
	k, status, err := newKind(repo, folder, log)
	if err != nil {
		return nil, err
	}
	return &runner{
		repo:    repo,
		base:    base,
		folder:  folder,
		kind:    k,
		log:     log,
		cache:   cache,
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		known:   map[string]known{},
		skipped: map[string]string{},
		status:  status,
	}, nil
}

// cacheVersion is the version of what the cache keeps of a package. A cache
// of another is dropped, so that a change to what is read of a package
// reaches the packages read before it.
const cacheVersion = 2

// The cache keeps a record of each package under packagePrefix and its path
// in the repository, and one that says what they were read for under
// metaKey.
const (
	packagePrefix = "pkg/"
	metaKey       = "meta"
)

// cacheMeta says what the packages of the cache were read for, which they
// are good for only as long as it stays the same, and what the metadata
// written last lists.
type cacheMeta struct {
	Version    int    `json:"version"`
	Type       string `json:"type"`
	Folder     string `json:"folder"`
	SigningKey string `json:"signingKey,omitempty"`
	// Repo is the configuration the metadata was written for, which it is
	// written again for when it changed.
	Repo     json.RawMessage `json:"repo,omitempty"`
	Listed   string          `json:"listed,omitempty"`
	Packages int             `json:"packages,omitempty"`
	Albums   int             `json:"albums,omitempty"`
	Built    time.Time       `json:"built,omitzero"`
}

// cachedPackage is a package as the cache keeps it.
type cachedPackage struct {
	Size    int64           `json:"size"`
	ModTime time.Time       `json:"modTime"`
	Entry   json.RawMessage `json:"entry"`
}

// meta is what the cache says of the runner as it is now.
func (r *runner) meta() cacheMeta {
	repo, _ := json.Marshal(r.repo)
	return cacheMeta{Version: cacheVersion, Type: r.repo.Type, Folder: r.folder, SigningKey: r.repo.SigningKey,
		Repo: repo, Listed: r.listed, Packages: r.status.Packages, Albums: r.status.Albums, Built: r.status.Built}
}

// restore takes what the cache kept: the packages read, when they were read
// for the same type, folder and key, and what the metadata lists, when it was
// written for the same configuration too. Anything else is dropped. Where the
// cache cannot be read, every package is read again, as without one.
func (r *runner) restore() {
	var kept cacheMeta
	found, err := r.cache.Get(metaKey, &kept)
	if err != nil {
		r.log.Warn("cannot read the cache of the repository, so every package is read again", "error", err)
		return
	}
	now := r.meta()
	if !found || kept.Version != now.Version || kept.Type != now.Type || kept.Folder != now.Folder ||
		kept.SigningKey != now.SigningKey {
		r.drop()
		return
	}
	err = r.cache.Scan(packagePrefix, func(key string, decode func(any) error) error {
		var p cachedPackage
		if err := decode(&p); err != nil {
			return nil
		}
		e, err := r.kind.decode(p.Entry)
		if err != nil {
			// read again, as a package not kept
			return nil
		}
		r.known[strings.TrimPrefix(key, packagePrefix)] = known{size: p.Size, modTime: p.ModTime, entry: e}
		return nil
	})
	if err != nil {
		r.log.Warn("cannot read the cache of the repository, so every package is read again", "error", err)
		r.known = map[string]known{}
		return
	}
	if string(kept.Repo) == string(now.Repo) {
		r.listed = kept.Listed
		r.status.Packages = kept.Packages
		r.status.Albums = kept.Albums
		r.status.Built = kept.Built
	}
	r.log.Debug("repository cache restored", "packages", len(r.known), "listed", r.listed != "")
}

// drop removes everything the cache keeps.
func (r *runner) drop() {
	var keys []string
	err := r.cache.Scan("", func(key string, _ func(any) error) error {
		keys = append(keys, key)
		return nil
	})
	if err == nil && len(keys) > 0 {
		err = r.cache.Write(func(w *store.Writer) error {
			for _, key := range keys {
				if err := w.Delete(key); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err != nil {
		r.log.Warn("cannot drop the cache of the repository", "error", err)
	}
}

// keep brings the packages of the cache in line with now, from before,
// writing only those that changed. Where it cannot, the log says so and the
// repository goes on: the cache only spares reading a package again.
func (r *runner) keep(before, now map[string]known) {
	var changed []string
	for href, k := range now {
		if b, ok := before[href]; !ok || b.size != k.size || !b.modTime.Equal(k.modTime) {
			changed = append(changed, href)
		}
	}
	var removed []string
	for href := range before {
		if _, ok := now[href]; !ok {
			removed = append(removed, href)
		}
	}
	if len(changed) == 0 && len(removed) == 0 {
		return
	}
	err := r.cache.Write(func(w *store.Writer) error {
		// the meta goes with the packages, so that none is ever kept
		// without saying what it was read for
		if err := w.Put(metaKey, r.meta()); err != nil {
			return err
		}
		for _, href := range changed {
			k := now[href]
			e, err := json.Marshal(k.entry)
			if err != nil {
				return err
			}
			if err := w.Put(packagePrefix+href, cachedPackage{Size: k.size, ModTime: k.modTime, Entry: e}); err != nil {
				return err
			}
		}
		for _, href := range removed {
			if err := w.Delete(packagePrefix + href); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.log.Warn("cannot keep the packages read in the cache of the repository", "error", err)
	}
}

// run looks at the folder at once and then every pollInterval until ctx ends
// or stop is called.
func (r *runner) run(ctx context.Context) {
	defer close(r.done)
	r.log.Info("repository started", "type", r.repo.Type, "folder", r.repo.Folder, "key", r.kind.keyID(),
		"pollInterval", r.repo.Interval())
	// restored here rather than when the runner is made, so that a runner a
	// reload makes and drops again never touches the cache, and one that
	// replaces another reads it once that one stopped
	r.restore()
	r.look()
	ticker := time.NewTicker(time.Duration(r.repo.Interval()) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.quit:
			return
		case <-ticker.C:
			r.look()
		}
	}
}

// stop ends the runner and waits for a look in progress to finish, so that
// the one that replaces it never works on the folder at the same time.
func (r *runner) stop() {
	select {
	case <-r.quit:
	default:
		close(r.quit)
	}
	if r.launched {
		<-r.done
	}
	if k, ok := r.kind.(retirer); ok {
		k.retire()
	}
}

func (r *runner) stopped() bool {
	select {
	case <-r.quit:
		return true
	default:
		return false
	}
}

// look is one look at the folder.
func (r *runner) look() {
	err := r.update(time.Now())
	if errors.Is(err, errStopped) {
		return
	}
	r.status.Checked = time.Now()
	r.status.Error = ""
	if err != nil {
		r.status.Error = err.Error()
		r.log.Error("the repository cannot be updated", "error", err)
	}
	r.status.Skipped = maps.Clone(r.skipped)
	if !r.stopped() {
		report(r.repo.Name, r.status)
	}
}

// update brings the repository in line with its folder.
func (r *runner) update(now time.Time) error {
	found, err := r.packages()
	if err != nil {
		return err
	}

	// every package is read and checked first, and the old versions pruned,
	// so that only the packages that stay are admitted
	skipped := map[string]string{}
	var candidates []candidate
	for _, href := range found {
		if r.stopped() {
			return errStopped
		}
		c, err := r.read(href)
		if err != nil {
			skipped[href] = err.Error()
			continue
		}
		candidates = append(candidates, c)
	}
	candidates = r.prune(candidates)

	var entries []entry
	current := map[string]known{}
	for _, c := range candidates {
		if r.stopped() {
			return errStopped
		}
		k, err := r.admit(c)
		if err != nil {
			skipped[c.href] = err.Error()
			continue
		}
		current[c.href] = k
		entries = append(entries, k.entry)
	}
	r.keep(r.known, current)
	r.known = current
	r.skip(skipped)

	listing := listingOf(entries) + r.kind.inputs(entries)
	if listing == r.listed && r.kind.written() {
		return nil
	}
	if err := r.kind.write(entries, now); err != nil {
		return err
	}
	r.listed = listing
	r.status.Packages = len(entries)
	r.status.Built = now
	if k, ok := r.kind.(reporter); ok {
		k.report(&r.status)
	}
	if err := r.cache.Put(metaKey, r.meta()); err != nil {
		r.log.Warn("cannot keep what the metadata lists in the cache of the repository", "error", err)
	}
	r.log.Info("repository metadata written", "packages", len(entries))
	return nil
}

// packages lists the packages in the folder, by their path in it with
// forward slashes. Folders whose name begins with a dot, and the metadata,
// are not looked into.
func (r *runner) packages() ([]string, error) {
	info, err := logging.Stat(r.log, r.folder)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", r.repo.Folder)
	}
	var found []string
	err = logging.WalkDir(r.log, r.folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == r.folder {
				return err
			}
			// a folder that cannot be read is left out rather than
			// failing the whole repository
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == r.folder {
				return nil
			}
			rel, err := filepath.Rel(r.folder, path)
			if err != nil || strings.HasPrefix(name, ".") || r.kind.skipDir(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !r.kind.isPackage(name) || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(r.folder, path)
		if err != nil {
			return nil
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(found)
	return found, err
}

// candidate is a package found in the folder that checked out: either one
// read before, or one read now.
type candidate struct {
	href string
	// before is what was read of it before, when it did not change since,
	// and opened the package read now otherwise.
	before *known
	opened release
}

// release is the version the candidate is.
func (c candidate) release() release {
	if c.before != nil {
		return c.before.entry
	}
	return c.opened
}

// read reads and checks the package at href, from what was read of it
// before when the file did not change since.
func (r *runner) read(href string) (candidate, error) {
	path := filepath.Join(r.folder, filepath.FromSlash(href))
	info, err := logging.Stat(r.log, path)
	if err != nil {
		return candidate{}, err
	}
	if before, ok := r.known[href]; ok && before.size == info.Size() && before.modTime.Equal(info.ModTime()) {
		return candidate{href: href, before: &before}, nil
	}
	opened, err := r.kind.open(path)
	if err != nil {
		return candidate{}, err
	}
	return candidate{href: href, opened: opened}, nil
}

// admit makes a package that stays in the repository ready to be listed, as
// its kind does: an rpm is signed with the repository's key, where it was
// not, and read for the metadata.
func (r *runner) admit(c candidate) (known, error) {
	if c.before != nil {
		return *c.before, nil
	}
	e, err := r.kind.admit(c.opened, c.href)
	if err != nil {
		return known{}, err
	}
	// admitting may have changed the file, and it is known as it is now
	info, err := logging.Stat(r.log, filepath.Join(r.folder, filepath.FromSlash(c.href)))
	if err != nil {
		return known{}, err
	}
	return known{size: info.Size(), modTime: info.ModTime(), entry: e}, nil
}

// skip records the files left out at this look, and logs each the first
// time it is left out for its reason.
func (r *runner) skip(now map[string]string) {
	for href, reason := range now {
		if r.skipped[href] != reason {
			r.log.Warn("repository left out a file", "file", href, "reason", reason)
		}
	}
	r.skipped = now
}

// prune removes the versions of each package beyond keep, the oldest first,
// and answers the packages that are left.
func (r *runner) prune(candidates []candidate) []candidate {
	if r.repo.Keep <= 0 {
		return candidates
	}
	groups := map[string][]candidate{}
	for _, c := range candidates {
		group := c.release().group()
		groups[group] = append(groups[group], c)
	}
	removed := map[string]bool{}
	for _, group := range groups {
		if len(group) <= r.repo.Keep {
			continue
		}
		// the newest first; two files of one version stay in the order of
		// their names
		slices.SortStableFunc(group, func(a, b candidate) int { return b.release().compare(a.release()) })
		for _, old := range group[r.repo.Keep:] {
			path := filepath.Join(r.folder, filepath.FromSlash(old.href))
			if err := logging.Remove(r.log, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				r.log.Error("repository cannot remove an old version", "package", old.href, "error", err)
				continue
			}
			removed[old.href] = true
			r.log.Info("repository removed an old version", "package", old.href,
				"version", old.release().version(), "keep", r.repo.Keep)
		}
	}
	if len(removed) == 0 {
		return candidates
	}
	return slices.DeleteFunc(candidates, func(c candidate) bool { return removed[c.href] })
}

// listingOf identifies what metadata lists, so that it is written only when
// that changed.
func listingOf(entries []entry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.listing())
		b.WriteByte('\n')
	}
	return b.String()
}
