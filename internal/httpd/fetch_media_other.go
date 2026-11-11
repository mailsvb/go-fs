//go:build !unix

package httpd

import (
	"log/slog"
	"os/exec"
)

// ownProcessGroup leaves yt-dlp as it is: a stopped fetch kills it alone.
func ownProcessGroup(*exec.Cmd, *slog.Logger) {}
