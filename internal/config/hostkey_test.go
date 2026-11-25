package config

import "testing"

// generateHostKey makes an SSH host key.
func generateHostKey(t *testing.T) string {
	t.Helper()
	key, _, err := Generate(KindSSHKey, "")
	if err != nil {
		t.Fatal(err)
	}
	return key
}
