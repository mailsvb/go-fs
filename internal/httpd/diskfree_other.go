//go:build !unix && !windows

package httpd

// diskFree cannot tell how much room there is here, and so says nothing.
func diskFree(string) int64 {
	return -1
}
