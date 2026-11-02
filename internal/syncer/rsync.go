package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/store"
)

// job is everything a run of a sync needs, so that a sync whose job did not
// change keeps running as it was when the file is read again.
type job struct {
	sync   config.Sync
	server config.Server
	ssh    config.SSH
	// local is the folder on disk, trash general.baseFolderTrash, and
	// snapshots where the snapshot of a sync both ways is kept, under the
	// name of the sync; nil keeps none, and a sync both ways then removes
	// nothing.
	local     string
	trash     string
	snapshots *store.Bucket
}

// result is what a run did.
type result struct {
	uploaded   int
	downloaded int
	deleted    int
	bytes      int64
	conflicts  int
	// failures are why paths failed, as many as a status holds, and failed
	// how many did.
	failures map[string]string
	failed   int

	// made are the paths a step made alike, and broken those a step failed
	// for, which the snapshot is taken from.
	made   map[string]entry
	broken map[string]bool
}

// maxFailures is how many failed paths a status holds; the log has all of
// them.
const maxFailures = 20

func (r *result) fail(rel string, err error) {
	r.failed++
	if r.broken == nil {
		r.broken = map[string]bool{}
	}
	r.broken[rel] = true
	if r.failures == nil {
		r.failures = map[string]string{}
	}
	if len(r.failures) < maxFailures {
		r.failures["/"+rel] = err.Error()
	}
}

// runRsync is one run of a sync by rsync: both folders are listed, what
// differs is copied whole, and with delete what is gone on one side is
// removed on the other.
func runRsync(ctx context.Context, j job, log *slog.Logger) (result, error) {
	login, err := loginOf(j.server)
	if err != nil {
		return result{}, err
	}
	rem, err := openRemote(ctx, login, j.ssh, j.sync.RemoteFolder)
	if err != nil {
		return result{}, err
	}
	defer func() { _ = rem.close() }()
	loc := localSide{root: j.local, virtual: j.sync.LocalFolder, trash: j.trash}

	localFiles, err := loc.list(ctx)
	if err != nil {
		return result{}, fmt.Errorf("the local folder %s: %w", j.sync.LocalFolder, err)
	}
	remoteFiles, err := rem.list(ctx)
	if err != nil {
		return result{}, fmt.Errorf("the remote folder %s: %w", rem.root, err)
	}

	// a side that is empty while the other is not removes nothing: it is
	// more likely a folder emptied by mistake, or a disk that is not
	// mounted, than everything having been removed on purpose
	var refused error
	localEmptied := emptied(localFiles, remoteFiles)
	remoteEmptied := emptied(remoteFiles, localFiles)
	var last map[string]entry
	if j.sync.Direction == config.SyncBoth {
		last = j.readSnapshot(log)
	}
	if j.sync.Delete {
		switch j.sync.Direction {
		case config.SyncPush:
			remoteEmptied = false
		case config.SyncPull:
			localEmptied = false
		case config.SyncBoth:
			// with nothing synced before, nothing would be removed anyway
			if len(last) == 0 {
				localEmptied, remoteEmptied = false, false
			}
		}
		switch {
		case localEmptied:
			refused = fmt.Errorf("the local folder %s %w", j.sync.LocalFolder, errEmptied)
		case remoteEmptied:
			refused = fmt.Errorf("the remote folder %s %w", rem.root, errEmptied)
		}
	}
	del := j.sync.Delete && refused == nil

	var p plan
	switch j.sync.Direction {
	case config.SyncPush:
		p = planOneWay(localFiles, remoteFiles, true, del)
	case config.SyncPull:
		p = planOneWay(remoteFiles, localFiles, false, del)
	case config.SyncBoth:
		p = planBoth(localFiles, remoteFiles, last, del)
	default:
		return result{}, fmt.Errorf("direction %q is not one of push, pull, both", j.sync.Direction)
	}

	res := execute(ctx, p, loc, rem, log)
	for _, rel := range p.clashes {
		res.fail(rel, errClash)
		log.Warn("sync leaves a path alone", "path", rel, "error", errClash)
	}
	for _, rel := range p.conflicts {
		log.Warn("sync conflict: the file changed on both sides, and the newer was kept", "path", rel)
	}
	res.conflicts = len(p.conflicts)

	if err := ctx.Err(); err != nil {
		return res, err
	}
	if j.sync.Direction == config.SyncBoth {
		if err := j.writeSnapshot(p.insync, last, res); err != nil {
			log.Error("sync cannot keep what it synced; a removal is not passed on until it can", "error", err)
			if refused == nil {
				refused = fmt.Errorf("what was synced cannot be kept: %w", err)
			}
		}
	}
	return res, refused
}

// emptied reports a side that is empty when the other is not.
func emptied(side, other map[string]entry) bool {
	return len(side) == 0 && len(other) > 0
}

// execute does the steps of a plan, in their order, and goes on past one
// that fails.
func execute(ctx context.Context, p plan, loc localSide, rem *remoteSide, log *slog.Logger) result {
	res := result{made: map[string]entry{}}
	on := func(remote bool) side {
		if remote {
			return rem
		}
		return loc
	}
	where := func(remote bool) string {
		if remote {
			return "remote"
		}
		return "local"
	}
	for _, s := range p.mkdirs {
		if ctx.Err() != nil {
			return res
		}
		if err := on(s.remote).mkdir(s.path); err != nil {
			res.fail(s.path, err)
			log.Error("sync cannot make a folder", "side", where(s.remote), "path", s.path, "error", err)
			continue
		}
		res.made[s.path] = s.entry
	}
	for _, s := range p.copies {
		if ctx.Err() != nil {
			return res
		}
		n, err := copyOne(on(!s.remote), on(s.remote), s)
		res.bytes += n
		if err != nil {
			res.fail(s.path, err)
			log.Error("sync cannot copy a file", "to", where(s.remote), "path", s.path, "error", err)
			continue
		}
		if s.remote {
			res.uploaded++
		} else {
			res.downloaded++
		}
		res.made[s.path] = s.entry
		log.Debug("sync copied a file", "to", where(s.remote), "path", s.path, "size", s.entry.Size)
	}
	for _, s := range append(p.removes, p.rmdirs...) {
		if ctx.Err() != nil {
			return res
		}
		if err := on(s.remote).remove(s.path, s.entry.Dir); err != nil {
			res.fail(s.path, err)
			log.Error("sync cannot remove", "side", where(s.remote), "path", s.path, "error", err)
			continue
		}
		res.deleted++
		// every removal is reported, as the cleanup reports its own
		log.Info("sync removed", "side", where(s.remote), "path", s.path, "folder", s.entry.Dir)
	}
	return res
}

// copyOne copies the file of a step from one side to the other, and reports
// how much of it was read.
func copyOne(from, to side, s step) (int64, error) {
	in, err := from.open(s.path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	counted := &counter{r: in}
	err = to.put(s.path, counted, s.entry.Size, s.entry.Mod)
	return counted.n, err
}

// counter counts what is read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// snapshot is what was alike on both sides after a run both ways, by which
// the next run tells a file removed on one side from one that is new on the
// other. It is for the folders it was taken of, and one of other folders is
// not used.
type snapshot struct {
	Server       string           `json:"server"`
	RemoteFolder string           `json:"remoteFolder"`
	LocalFolder  string           `json:"localFolder"`
	Taken        time.Time        `json:"taken"`
	Files        map[string]entry `json:"files"`
}

// readSnapshot reads what was alike after the last run, nil when nothing is
// known of these folders, which removes nothing.
func (j job) readSnapshot(log *slog.Logger) map[string]entry {
	var last snapshot
	found, err := j.snapshots.Get(j.sync.Name, &last)
	if err != nil {
		log.Warn("sync cannot read what it synced last, and removes nothing this time", "error", err)
		return nil
	}
	if !found {
		return nil
	}
	if !strings.EqualFold(last.Server, j.sync.Server) || last.RemoteFolder != j.sync.RemoteFolder ||
		last.LocalFolder != j.sync.LocalFolder {
		return nil
	}
	return last.Files
}

// writeSnapshot keeps what is alike on both sides now: what was before the
// run, and what the run made alike. A path a step failed for keeps what the
// last snapshot said of it, so that the next run decides it the same way.
func (j job) writeSnapshot(insync, last map[string]entry, res result) error {
	files := make(map[string]entry, len(insync)+len(res.made))
	for rel, e := range insync {
		files[rel] = e
	}
	for rel, e := range res.made {
		files[rel] = e
	}
	for rel := range res.broken {
		if e, ok := last[rel]; ok {
			files[rel] = e
		}
	}
	return j.snapshots.Put(j.sync.Name, snapshot{Server: j.sync.Server, RemoteFolder: j.sync.RemoteFolder,
		LocalFolder: j.sync.LocalFolder, Taken: time.Now(), Files: files})
}
