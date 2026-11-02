//go:build unix

package httpd

import "golang.org/x/sys/unix"

// diskFree is how many bytes the filesystem that holds dir has free for this
// process, or -1 when that cannot be told.
func diskFree(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize))
}
