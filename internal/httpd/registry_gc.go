package httpd

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/mailsvb/go-fs/internal/logging"
)

// sweepRegistry keeps the registry folder from growing without bound: it
// removes the uploads nobody finished, and the blobs no manifest refers to
// any more. It runs with the rest of the hourly cleanup.
func (s *Server) sweepRegistry() {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	store := s.settings().registry
	if store == nil {
		return
	}
	cutoff := time.Now().Add(-stagingMaxAge)
	s.sweepRegistryUploads(store, cutoff)
	s.collectRegistryGarbage(store, cutoff)
}

// registryCleanupGrace is how young a blob or a link may be and still be kept
// when the cleanup is run from the registry page. Someone who just deleted a
// tag wants its blobs gone now, not a day later, so it is far shorter than
// stagingMaxAge; it still covers a push under way, whose layers arrive
// minutes before the manifest that refers to them.
const registryCleanupGrace = 10 * time.Minute

// cleanRegistryNow is the cleanup the registry page asks for, and reports how
// many blobs it removed and how many bytes that freed. Uploads are swept as
// the hourly cleanup sweeps them: a slow one is still arriving.
func (s *Server) cleanRegistryNow(store *registryStore) (int, int64) {
	now := time.Now()
	s.sweepRegistryUploads(store, now.Add(-stagingMaxAge))
	return s.collectRegistryGarbage(store, now.Add(-registryCleanupGrace))
}

// sweepRegistryUploads removes the uploads nothing has been added to since
// cutoff. Every chunk touches the data file and rewrites the record, so an
// upload that is still arriving is never taken for an abandoned one.
func (s *Server) sweepRegistryUploads(store *registryStore, cutoff time.Time) {
	folder := store.uploadsPath()
	entries, err := logging.ReadDir(s.log, folder)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(folder, entry.Name())
		if !newestChange(path).Before(cutoff) {
			continue
		}
		// a loose file is what a manifest being stored left behind when the
		// server stopped halfway through writing it
		if err := logging.RemoveAll(s.log, path); err != nil {
			s.log.Error("registry cleanup cannot remove an abandoned upload", "path", path, "error", err)
			continue
		}
		s.log.Info("registry cleanup removed an abandoned upload", "path", path, "age", stagingMaxAge)
	}
}

// newestChange is the time a file, or anything directly in a folder, was last
// changed.
func newestChange(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	newest := info.ModTime()
	if !info.IsDir() {
		return newest
	}
	entries, _ := os.ReadDir(path)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// collectRegistryGarbage removes the blobs nothing refers to. A blob is kept
// while a manifest of any repository refers to it, directly or through an
// index, and while it or a link to it is younger than cutoff: a push uploads
// the layers before the manifest that refers to them, and this must never
// take them in between. A link of a repository is removed once no manifest of
// that repository refers to it and it is older than cutoff, so the blob can
// go once no repository needs it.
//
// It holds the registry's lock exclusively for the whole run, so no push adds
// a reference to what it is about to remove. Uploads keep arriving meanwhile;
// only finishing one waits. It returns how many blobs it removed and how many
// bytes that freed.
func (s *Server) collectRegistryGarbage(store *registryStore, cutoff time.Time) (int, int64) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()

	repositories, err := store.repositoryFolders()
	if err != nil {
		s.log.Error("registry cleanup cannot list the repositories", "error", err)
		return 0, 0
	}
	s.dropDanglingArtifacts(store, repositories, cutoff)
	marked := map[digest.Digest]bool{}
	for _, name := range repositories {
		referenced := map[digest.Digest]bool{}
		revisions, err := store.revisions(name)
		if err != nil {
			// what cannot be read cannot be known to be unreferenced
			s.log.Error("registry cleanup cannot read a repository, nothing is collected",
				"repository", name, "error", err)
			return 0, 0
		}
		for _, d := range revisions {
			markManifest(store, d, referenced, 0)
		}
		for d := range referenced {
			marked[d] = true
		}
		for _, link := range store.links(name) {
			switch {
			case referenced[link.digest]:
			case link.changed.After(cutoff):
				marked[link.digest] = true
			default:
				if err := logging.Remove(s.log, store.layerPath(name, link.digest)); err == nil {
					s.log.Debug("registry cleanup removed an unreferenced link",
						"repository", name, "digest", link.digest.String())
				}
			}
		}
	}

	removed, freed := 0, int64(0)
	blobs := filepath.Join(store.base, "blobs")
	_ = logging.WalkDir(s.log, blobs, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(blobs, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		d := digest.NewDigestFromEncoded(digest.Algorithm(parts[0]), parts[2])
		if d.Validate() != nil || marked[d] {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if err := logging.Remove(s.log, path); err != nil {
			s.log.Error("registry cleanup cannot remove a blob", "digest", d.String(), "error", err)
			return nil
		}
		// the fan-out folder goes with its last blob; a folder that still
		// holds one is not removed
		_ = logging.Remove(s.log, filepath.Dir(path))
		removed++
		freed += info.Size()
		s.log.Info("registry cleanup removed an unreferenced blob", "digest", d.String(),
			"bytes", info.Size())
		return nil
	})
	if removed > 0 {
		s.log.Info("registry cleanup done", "blobs", removed, "bytes", freed)
	}
	return removed, freed
}

// dropDanglingArtifacts forgets the signatures, attestations, SBOMs and other
// artifacts whose image is gone, so the collection can take their blobs. Every
// manifest a repository holds is otherwise kept, tagged or not, since someone
// may pull it by its digest; an artifact that belongs to an image nobody can
// pull any more is of no use to anyone.
//
// An artifact belongs to an image in one of two ways: by its subject, or by a
// fallback tag named after the image's digest, sha256-<hex>.sig and the like.
// Such a tag is removed once no repository holds the manifest it names, and
// with it the manifest it pointed to unless another tag still does. A manifest
// with a subject no repository holds is removed when no tag points to it.
// Either way, only what is older than cutoff: a copy may bring the signature
// before the image it is for. Whatever an index of the registry still names is
// kept. Removing one artifact can leave another dangling, a signature of an
// SBOM say, so it repeats until nothing more is found.
//
// The subject counts as held when any repository holds it: cosign stores
// signatures in another repository than their image when COSIGN_REPOSITORY is
// set. The caller holds registryMu exclusively, so nothing is pushed
// meanwhile.
func (s *Server) dropDanglingArtifacts(store *registryStore, repositories []string, cutoff time.Time) {
	type repository struct {
		revisions []digest.Digest
		tags      map[string]digest.Digest
		// the fallback tags given up and what they pointed to, and the
		// manifests given up
		droppedTags map[string]digest.Digest
		dropped     map[digest.Digest]bool
	}
	held := map[digest.Digest]int{}
	docs := map[digest.Digest]*manifestDoc{}
	named := map[digest.Digest]bool{}
	repos := map[string]*repository{}
	for _, name := range repositories {
		revisions, err := store.revisions(name)
		if err != nil {
			// the collection that follows says so, and keeps everything
			return
		}
		tags, err := store.readTags(name)
		if err != nil {
			s.log.Error("registry cleanup cannot read the tags, no artifact is dropped",
				"repository", name, "error", err)
			return
		}
		repos[name] = &repository{revisions: revisions, tags: tags,
			droppedTags: map[string]digest.Digest{}, dropped: map[digest.Digest]bool{}}
		for _, d := range revisions {
			held[d]++
			if _, read := docs[d]; read {
				continue
			}
			docs[d] = nil
			data, err := store.readBlob(d, maxManifestSize)
			var doc manifestDoc
			if err != nil || json.Unmarshal(data, &doc) != nil {
				continue
			}
			docs[d] = &doc
			for _, entry := range doc.Manifests {
				named[entry.Digest] = true
			}
		}
	}

	old := func(name string, d digest.Digest) bool {
		changed, err := store.revisionTime(name, d)
		return err == nil && !changed.After(cutoff)
	}
	for found := true; found; {
		found = false
		for _, name := range repositories {
			repo := repos[name]
			for tag, target := range repo.tags {
				subject, _, ok := fallbackTagSubject(tag)
				if !ok || held[subject] > 0 || !old(name, target) {
					continue
				}
				delete(repo.tags, tag)
				repo.droppedTags[tag] = target
				found = true
			}
			tagged := map[digest.Digest]bool{}
			for _, target := range repo.tags {
				tagged[target] = true
			}
			fellBack := map[digest.Digest]bool{}
			for _, target := range repo.droppedTags {
				fellBack[target] = true
			}
			for _, d := range repo.revisions {
				if repo.dropped[d] || tagged[d] || named[d] || !old(name, d) {
					continue
				}
				doc := docs[d]
				orphan := doc != nil && doc.Subject != nil && held[doc.Subject.Digest] == 0
				if !orphan && !fellBack[d] {
					continue
				}
				repo.dropped[d] = true
				held[d]--
				found = true
			}
		}
	}

	for _, name := range repositories {
		repo := repos[name]
		if len(repo.droppedTags) == 0 && len(repo.dropped) == 0 {
			continue
		}
		lock := registryLock(&s.registryRepositoryLocks, name)
		lock.Lock()
		s.dropArtifacts(store, name, repo.droppedTags, repo.dropped, docs)
		lock.Unlock()
	}
}

// dropArtifacts removes what dropDanglingArtifacts found in one repository.
// The tags are read again, under the repository's lock: a tag deleted
// meanwhile is not written back.
func (s *Server) dropArtifacts(store *registryStore, name string, droppedTags map[string]digest.Digest,
	dropped map[digest.Digest]bool, docs map[digest.Digest]*manifestDoc) {
	tags, err := store.readTags(name)
	if err != nil {
		s.log.Error("registry cleanup cannot read the tags, no artifact is dropped",
			"repository", name, "error", err)
		return
	}
	untagged := false
	for tag, target := range droppedTags {
		if tags[tag] == target {
			delete(tags, tag)
			untagged = true
		}
	}
	if untagged {
		if err := store.writeTags(name, tags); err != nil {
			s.log.Error("registry cleanup cannot remove the tags of an image that is gone",
				"repository", name, "error", err)
			return
		}
	}
	for tag, target := range droppedTags {
		s.log.Info("registry cleanup removed a tag whose image is gone", "repository", name,
			"tag", tag, "digest", target.String())
	}
	tagged := map[digest.Digest]bool{}
	for _, target := range tags {
		tagged[target] = true
	}
	for d := range dropped {
		if tagged[d] {
			continue
		}
		attrs := []any{"repository", name, "digest", d.String()}
		if doc := docs[d]; doc != nil {
			if doc.Subject != nil {
				store.removeReferrer(name, doc.Subject.Digest, d)
				attrs = append(attrs, "subject", doc.Subject.Digest.String())
			}
			if kind := artifactKindOf(*doc); kind != "" {
				attrs = append(attrs, "artifact", kind)
			}
		}
		if err := store.removeRevision(name, d); err != nil && !errors.Is(err, errRegistryNotFound) {
			s.log.Error("registry cleanup cannot remove an artifact whose image is gone",
				append(attrs, "error", err)...)
			continue
		}
		s.log.Info("registry cleanup removed an artifact whose image is gone", attrs...)
	}
}

// markManifest marks a manifest and everything it refers to. depth keeps an
// index that names itself, through whatever chain, from recursing forever.
func markManifest(store *registryStore, d digest.Digest, marked map[digest.Digest]bool, depth int) {
	if marked[d] && depth > 0 || depth > 8 {
		return
	}
	marked[d] = true
	data, err := store.readBlob(d, maxManifestSize)
	if err != nil {
		return
	}
	var doc manifestDoc
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	// the digests were checked when the manifest was pushed, and are again,
	// since one is about to name a file
	if doc.Config != nil {
		marked[doc.Config.Digest] = true
	}
	for _, layer := range doc.Layers {
		marked[layer.Digest] = true
	}
	for _, entry := range doc.Manifests {
		if _, ok := parseBlobDigest(entry.Digest.String()); ok {
			markManifest(store, entry.Digest, marked, depth+1)
		}
	}
}

// blobLink is a blob a repository links to, and when the link was made or
// last touched.
type blobLink struct {
	digest  digest.Digest
	changed time.Time
}

// links lists the blobs a repository links to.
func (st *registryStore) links(name string) []blobLink {
	root := filepath.Join(st.repoPath(name), "_layers")
	algorithms, err := logging.ReadDir(st.log, root)
	if err != nil {
		return nil
	}
	var found []blobLink
	for _, algorithm := range algorithms {
		entries, err := logging.ReadDir(st.log, filepath.Join(root, algorithm.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			d := digest.NewDigestFromEncoded(digest.Algorithm(algorithm.Name()), entry.Name())
			info, err := entry.Info()
			if d.Validate() != nil || err != nil {
				continue
			}
			found = append(found, blobLink{digest: d, changed: info.ModTime()})
		}
	}
	return found
}

// repositoryFolders lists every repository anything was pushed to, whether it
// holds a manifest yet or only blobs.
func (st *registryStore) repositoryFolders() ([]string, error) {
	root := filepath.Join(st.base, "repositories")
	var names []string
	err := logging.WalkDir(st.log, root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return filepath.SkipDir
			}
			return err
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		if strings.HasPrefix(entry.Name(), "_") {
			return filepath.SkipDir
		}
		for _, own := range []string{"_manifests", "_layers", registryTagsFile} {
			if _, err := logging.Stat(st.log, filepath.Join(path, own)); err == nil {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				names = append(names, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	return names, err
}
