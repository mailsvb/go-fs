package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Process is an external program on its way from its start to its end, both
// of which are recorded at debug: "exec start" with the program, its
// arguments as Args shows them, the folder and the process id, and "exec
// exit" with how it ended, how long it ran, the CPU it took and, when the
// caller did not take stderr itself, the end of what the program said there.
type Process struct {
	log     *slog.Logger
	cmd     *exec.Cmd
	started time.Time
	tail    *tailBuffer
}

// stderrTail is how much of a program's stderr "exec exit" keeps.
const stderrTail = 2 << 10

// Start starts cmd as cmd.Start does and records it.
func Start(log *slog.Logger, cmd *exec.Cmd) (*Process, error) {
	return start(log, cmd, false, 2)
}

// start is Start for a caller skip frames above it. keepStderr keeps the end
// of stderr whether or not debug is on.
func start(log *slog.Logger, cmd *exec.Cmd, keepStderr bool, skip int) (*Process, error) {
	p := &Process{log: OrDiscard(log), cmd: cmd}
	debug := p.log.Enabled(context.Background(), slog.LevelDebug)
	if (debug || keepStderr) && cmd.Stderr == nil {
		p.tail = &tailBuffer{limit: stderrTail}
		cmd.Stderr = p.tail
	}
	p.started = time.Now()
	if err := cmd.Start(); err != nil {
		Debug(context.Background(), p.log, skip, "exec failed",
			slog.String("path", cmd.Path),
			slog.Any("args", Args(cmd.Args[1:])),
			slog.Any("error", err))
		return nil, err
	}
	if debug {
		attrs := []slog.Attr{
			slog.String("path", cmd.Path),
			slog.Any("args", Args(cmd.Args[1:])),
			slog.Int("pid", cmd.Process.Pid),
		}
		if cmd.Dir != "" {
			attrs = append(attrs, slog.String("dir", cmd.Dir))
		}
		Debug(context.Background(), p.log, skip, "exec start", attrs...)
	}
	return p, nil
}

// Wait waits for the program as cmd.Wait does and records how it ended.
func (p *Process) Wait() error {
	return p.wait(2)
}

func (p *Process) wait(skip int) error {
	err := p.cmd.Wait()
	if !p.log.Enabled(context.Background(), slog.LevelDebug) {
		return err
	}
	attrs := []slog.Attr{
		slog.String("path", p.cmd.Path),
		slog.Duration("took", time.Since(p.started).Round(time.Millisecond)),
	}
	if state := p.cmd.ProcessState; state != nil {
		attrs = append(attrs,
			slog.Int("pid", state.Pid()),
			slog.String("state", state.String()),
			slog.Int("exitCode", state.ExitCode()),
			slog.Duration("userTime", state.UserTime().Round(time.Millisecond)),
			slog.Duration("systemTime", state.SystemTime().Round(time.Millisecond)))
	}
	if p.tail != nil {
		if said := strings.TrimSpace(p.tail.String()); said != "" {
			attrs = append(attrs, slog.String("stderr", said))
		}
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	Debug(context.Background(), p.log, skip, "exec exit", attrs...)
	return err
}

// Run runs cmd to its end as cmd.Run does, recording its start and its end.
func Run(log *slog.Logger, cmd *exec.Cmd) error {
	p, err := start(log, cmd, false, 2)
	if err != nil {
		return err
	}
	return p.wait(2)
}

// Output runs cmd as cmd.Output does and returns what it wrote to stdout.
// When the caller does not take stderr, its end is kept for the record and
// for the *exec.ExitError, as cmd.Output keeps it.
func Output(log *slog.Logger, cmd *exec.Cmd) ([]byte, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	p, err := start(log, cmd, true, 2)
	if err != nil {
		return nil, err
	}
	err = p.wait(2)
	var exit *exec.ExitError
	if errors.As(err, &exit) && p.tail != nil {
		exit.Stderr = []byte(p.tail.String())
	}
	return stdout.Bytes(), err
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
