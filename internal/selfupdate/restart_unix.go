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
	return syscall.Exec(u.exe, os.Args, os.Environ())
}

func rename(log *slog.Logger, from, to string) error {
	return logging.Rename(log, from, to)
}
