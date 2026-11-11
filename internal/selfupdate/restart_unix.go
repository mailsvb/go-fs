//go:build !windows

package selfupdate

import (
	"log/slog"
	"os"
	"syscall"

	"github.com/mailsvb/go-fs/internal/logging"
)

// Restart replaces this process with the executable, keeping the PID, the
// arguments and the environment, so that a service manager, a container or a
// terminal goes on tracking the same process. The listeners are closed by
// then, and every other descriptor Go opened is close-on-exec. It returns only
// when the exec failed.
func (u *Updater) Restart() error {
	// written before the exec, since a successful one never returns: the
	// next record is the new binary's "go-fs starting"
	logging.Syscall(u.log, "exec", nil, slog.String("path", u.exe), slog.Any("args", logging.Args(os.Args[1:])),
		slog.Int("pid", os.Getpid()))
	err := syscall.Exec(u.exe, os.Args, os.Environ())
	logging.Syscall(u.log, "exec", err, slog.String("path", u.exe))
	return err
}

func rename(log *slog.Logger, from, to string) error {
	return logging.Rename(log, from, to)
}
