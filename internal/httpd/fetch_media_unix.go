//go:build unix

package httpd

import (
	"log/slog"
	"os/exec"
	"syscall"

	"github.com/mailsvb/go-fs/internal/logging"
)

// ownProcessGroup runs yt-dlp in a process group of its own, and has a
// stopped fetch kill the whole group: the ffmpeg yt-dlp runs to join or
// convert would otherwise go on writing into a folder that is gone.
func ownProcessGroup(cmd *exec.Cmd, log *slog.Logger) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		logging.Syscall(log, "kill", err, slog.Int("processGroup", cmd.Process.Pid), slog.String("signal", "SIGKILL"))
		return err
	}
}
