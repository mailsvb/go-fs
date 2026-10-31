package syncer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// runner runs one sync.
type runner struct {
	job job
	log *slog.Logger
	// now is the clock, which a test sets.
	now func() time.Time

	// quit is closed to stop the runner, and done once it has stopped;
	// asked is a run asked for now.
	quit     chan struct{}
	done     chan struct{}
	asked    chan struct{}
	launched bool

	mu     sync.Mutex
	status Status
}

func newRunner(j job, logger *slog.Logger) *runner {
	return &runner{
		job:   j,
		log:   logger.With("sync", j.sync.Name),
		now:   time.Now,
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
		asked: make(chan struct{}, 1),
	}
}

// run runs the sync on its schedule until ctx ends or stop is called. A run
// in progress is stopped with it.
func (r *runner) run(ctx context.Context) {
	defer close(r.done)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	s := r.job.sync
	r.log.Info("sync started", "protocol", s.Protocol, "server", s.Server, "remoteFolder", s.RemoteFolder,
		"localFolder", s.LocalFolder, "direction", s.Direction, "delete", s.Delete,
		"every", s.Every, "at", s.At, "days", s.Days)
	var last time.Time
	for {
		at := next(s, last, r.now())
		r.update(func(status *Status) { status.Next = at })
		// a sync that never comes round runs only when asked to
		var due <-chan time.Time
		var timer *time.Timer
		if !at.IsZero() {
			timer = time.NewTimer(at.Sub(r.now()))
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-due:
		case <-r.asked:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		last = r.now()
		r.once(ctx, last)
	}
}

// once is one run of the sync.
func (r *runner) once(ctx context.Context, started time.Time) {
	r.update(func(status *Status) {
		status.Running = true
		status.Started = started
	})
	res, err := protocols[r.job.sync.Protocol](ctx, r.job, r.log)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}
	finished := r.now()
	r.update(func(status *Status) {
		*status = Status{
			Started: started, Finished: finished,
			Uploaded: res.uploaded, Downloaded: res.downloaded, Deleted: res.deleted, Bytes: res.bytes,
			Conflicts: res.conflicts, Failed: res.failed, Failures: res.failures,
		}
		if err != nil {
			status.Error = err.Error()
		}
	})
	attrs := []any{"uploaded", res.uploaded, "downloaded", res.downloaded, "deleted", res.deleted,
		"bytes", res.bytes, "conflicts", res.conflicts, "failed", res.failed,
		"duration", finished.Sub(started).Round(time.Millisecond)}
	if err != nil {
		r.log.Error("sync failed", append(attrs, "error", err)...)
		return
	}
	r.log.Info("sync done", attrs...)
}

// update changes the status and reports it.
func (r *runner) update(change func(*Status)) {
	r.mu.Lock()
	change(&r.status)
	status := r.status
	r.mu.Unlock()
	report(r, status)
}

// trigger asks for a run now.
func (r *runner) trigger() error {
	r.mu.Lock()
	running := r.status.Running
	r.mu.Unlock()
	if running {
		return ErrBusy
	}
	select {
	case r.asked <- struct{}{}:
	default:
		// one is asked for already
	}
	return nil
}

// stop ends the runner and waits for a run in progress to stop, so that the
// one that replaces it never works on the folders at the same time.
func (r *runner) stop() {
	select {
	case <-r.quit:
	default:
		close(r.quit)
	}
	if r.launched {
		<-r.done
	}
}
