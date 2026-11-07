//go:build !light && !no_nbd && !windows

package nbd

import "syscall"

// errDiskFull is what a write to a full filesystem fails with here.
var errDiskFull error = syscall.ENOSPC
