package disks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailsvb/go-fs/internal/sparse"
)

// The states of a job.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// keepFinished is how long a finished job stays in the list, so that the page
// can say how it ended.
const keepFinished = time.Hour

// Job is a clone running in the background.
type Job struct {
	ID      string
	Source  string
	Target  string
	Started time.Time

	done, total atomic.Int64
	cancel      context.CancelFunc
	finished    chan struct{}

	mu      sync.Mutex
	state   string
	message string
	ended   time.Time
}

// Progress is how many bytes of how many are done.
func (j *Job) Progress() (done, total int64) { return j.done.Load(), j.total.Load() }

// State is the state of the job, the error of one that failed, and when it
// finished.
func (j *Job) State() (state, message string, finished time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state, j.message, j.ended
}

// Cancel stops a running job.
func (j *Job) Cancel() { j.cancel() }

// Done is closed when the job is over.
func (j *Job) Done() <-chan struct{} { return j.finished }

func (j *Job) end(state, message string) {
	j.mu.Lock()
	j.state, j.message, j.ended = state, message, time.Now()
	j.mu.Unlock()
	close(j.finished)
}

// Clone starts copying the image src to the new image dst. The copy shares
// the blocks of src where the filesystem can, and is as sparse as src where
// it cannot. src may be read meanwhile, by a read-only session or a
// download, but not written: the copy would be of no moment in particular.
// dst is taken from the start, and appears under its name when it is done.
func (f *Folder) Clone(src, dst string) (*Job, error) {
	info, err := f.Stat(src)
	if err != nil {
		return nil, err
	}
	if err := ValidName(dst); err != nil {
		return nil, err
	}
	if f.key(src) == f.key(dst) {
		return nil, fmt.Errorf("%q %w", dst, ErrExists)
	}
	hold, err := f.reg.acquire(f.key(src), src, Hold, nil, "")
	if err != nil {
		return nil, err
	}
	target, err := f.reg.acquire(f.key(dst), dst, Exclusive, nil, "being cloned from "+src)
	if err != nil {
		hold.Release()
		return nil, err
	}
	if _, err := os.Lstat(f.path(dst)); err == nil {
		hold.Release()
		target.Release()
		return nil, fmt.Errorf("%q %w", dst, ErrExists)
	}
	staging := f.stagingPath("clone", dst)
	// whatever an earlier clone into the name left behind
	_ = os.Remove(staging)

	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{ID: newID(), Source: src, Target: dst, Started: time.Now(), cancel: cancel, state: JobRunning,
		finished: make(chan struct{})}
	job.total.Store(info.Size())
	f.reg.mu.Lock()
	f.reg.jobs = append(f.reg.jobs, job)
	f.reg.mu.Unlock()
	f.reg.begin("clone", f.key(dst))

	go func() {
		defer cancel()
		err := sparse.CopyFile(ctx, staging, f.path(src), func(done, total int64) {
			job.done.Store(done)
			job.total.Store(total)
		})
		if err == nil {
			// the lease on dst kept everything of this process away from
			// the name; this keeps whatever else may write to the folder
			if _, statErr := os.Lstat(f.path(dst)); statErr == nil {
				err = fmt.Errorf("%q %w", dst, ErrExists)
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				err = statErr
			}
		}
		if err == nil {
			err = os.Rename(staging, f.path(dst))
		}
		if err != nil {
			_ = os.Remove(staging)
		}
		// the disks are free again before the job says it is over, so that
		// whoever waits for it can attach them at once
		hold.Release()
		target.Release()
		f.reg.end("clone", f.key(dst))
		switch {
		case err == nil:
			job.end(JobDone, "")
		case errors.Is(err, context.Canceled):
			job.end(JobCancelled, "")
		default:
			job.end(JobFailed, err.Error())
		}
	}()
	return job, nil
}

// Jobs are the clones that run, and those that finished within the hour.
func (f *Folder) Jobs() []*Job {
	f.reg.mu.Lock()
	defer f.reg.mu.Unlock()
	kept := f.reg.jobs[:0]
	for _, job := range f.reg.jobs {
		if state, _, finished := job.State(); state == JobRunning || time.Since(finished) < keepFinished {
			kept = append(kept, job)
		}
	}
	f.reg.jobs = kept
	return append([]*Job(nil), kept...)
}

// Job is the job id, or nil.
func (f *Folder) Job(id string) *Job {
	for _, job := range f.Jobs() {
		if job.ID == id {
			return job
		}
	}
	return nil
}

// Dismiss removes a finished job from the list, reporting whether there was
// one to remove.
func (f *Folder) Dismiss(id string) bool {
	f.reg.mu.Lock()
	defer f.reg.mu.Unlock()
	for i, job := range f.reg.jobs {
		if job.ID != id {
			continue
		}
		if state, _, _ := job.State(); state == JobRunning {
			return false
		}
		f.reg.jobs = append(f.reg.jobs[:i], f.reg.jobs[i+1:]...)
		return true
	}
	return false
}
