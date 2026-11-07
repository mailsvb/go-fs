//go:build !light && !no_nbd

package nbd

import (
	"errors"

	"golang.org/x/sys/windows"
)

// diskFull reports an error that says the volume, or the quota on it, is
// full.
func diskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}
