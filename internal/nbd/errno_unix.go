//go:build !light && !no_nbd && !windows

package nbd

import (
	"errors"
	"syscall"
)

// diskFull reports an error that says the filesystem, or the quota on it, is
// full.
func diskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
