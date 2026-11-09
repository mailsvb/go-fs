//go:build unix

package httpd

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs yt-dlp in a process group of its own, and has a
// stopped fetch kill the whole group: the ffmpeg yt-dlp runs to join or
// convert would otherwise go on writing into a folder that is gone.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
