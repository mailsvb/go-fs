//go:build !light && !no_nbd

package nbd

import "golang.org/x/sys/windows"

// errDiskFull is what a write to a full volume fails with here. syscall.ENOSPC
// exists on Windows too, but only as a number Go made up: no write fails with
// it, and diskFull rightly does not know it.
var errDiskFull error = windows.ERROR_DISK_FULL
