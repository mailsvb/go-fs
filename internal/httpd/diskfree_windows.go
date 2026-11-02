//go:build windows

package httpd

import "golang.org/x/sys/windows"

// diskFree is how many bytes the volume that holds dir has free for this
// process, or -1 when that cannot be told.
func diskFree(dir string) int64 {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &free, &total, &totalFree); err != nil {
		return -1
	}
	return int64(free)
}
