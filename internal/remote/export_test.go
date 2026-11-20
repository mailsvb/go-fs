package remote

// SetMaxRegistryNames bounds the names a listing of a registry takes, until
// the function it answers is called.
func SetMaxRegistryNames(n int) (restore func()) {
	before := maxRegistryNames
	maxRegistryNames = n
	return func() { maxRegistryNames = before }
}
