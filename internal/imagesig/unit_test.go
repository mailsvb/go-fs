package imagesig

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		p, s string
		ok   bool
	}{
		{"https://github.com/o/r/.github/workflows/release.yml@refs/tags/*", "https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1.0.0", true},
		{"https://github.com/o/r/.github/workflows/release.yml@refs/tags/*", "https://github.com/o/r/.github/workflows/release.yml@refs/tags/rel/2", true},
		{"https://github.com/o/r/.github/workflows/release.yml@refs/tags/*", "https://github.com/o/r/.github/workflows/release.yml@refs/heads/main", false},
		{"https://github.com/o/*/.github/workflows/*.yml@*", "https://github.com/o/r/.github/workflows/ci.yml@refs/heads/main", true},
		{"https://github.com/o/r", "https://github.com/o/r/x", false},
		{"exact", "exact", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"*", "", true},
		{"", "x", false},
		{"**a", "bba", true},
	} {
		if got := globMatch(tc.p, tc.s); got != tc.ok {
			t.Errorf("globMatch(%q, %q) = %v", tc.p, tc.s, got)
		}
	}
}

// merkle computes the RFC 9162 tree hash of leaves[lo:hi] and the
// inclusion path of leaf i in it.
func merkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 1 {
		return hashLeaf(leaves[0])
	}
	k := 1
	for k*2 < len(leaves) {
		k *= 2
	}
	return hashChildren(merkleRoot(leaves[:k]), merkleRoot(leaves[k:]))
}

func merklePath(i int, leaves [][]byte) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	k := 1
	for k*2 < len(leaves) {
		k *= 2
	}
	if i < k {
		return append(merklePath(i, leaves[:k]), merkleRoot(leaves[k:]))
	}
	return append(merklePath(i-k, leaves[k:]), merkleRoot(leaves[:k]))
}

func TestVerifyInclusion(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	for n := 1; n <= 33; n++ {
		var leaves [][]byte
		for i := 0; i < n; i++ {
			leaves = append(leaves, []byte(fmt.Sprintf("leaf %d", i)))
		}
		root := b64(merkleRoot(leaves))
		for i := 0; i < n; i++ {
			var path []string
			for _, h := range merklePath(i, leaves) {
				path = append(path, b64(h))
			}
			if err := verifyInclusion(leaves[i], int64(i), int64(n), root, path); err != nil {
				t.Fatalf("leaf %d of %d: %v", i, n, err)
			}
			if err := verifyInclusion([]byte("other"), int64(i), int64(n), root, path); err == nil {
				t.Fatalf("leaf %d of %d: another leaf verified", i, n)
			}
			if n > 1 {
				if err := verifyInclusion(leaves[i], int64((i+1)%n), int64(n), root, path); err == nil {
					t.Fatalf("leaf %d of %d: verified at another index", i, n)
				}
			}
		}
	}
	sum := sha256.Sum256(nil)
	if err := verifyInclusion(nil, 5, 5, b64(sum[:]), nil); err == nil {
		t.Fatal("index outside the tree accepted")
	}
}

func TestDecodeB64(t *testing.T) {
	for _, s := range []string{"aGk/Pz8+", "aGk_Pz8-", "aGk/Pz8+\n"} {
		if b, err := decodeB64(s); err != nil || string(b) != "hi???>" {
			t.Errorf("decodeB64(%q) = %q, %v", s, b, err)
		}
	}
	if b, err := decodeB64("aGk="); err != nil || string(b) != "hi" {
		t.Errorf("padded: %q, %v", b, err)
	}
}

func TestTrustErrors(t *testing.T) {
	var te trustErrors
	te.what = "keys"
	if got := te.String(); got != "no trust policy with keys is configured" {
		t.Fatal(got)
	}
	te.add("a", errBadSignature)
	te.add("b", errBadSignature)
	if got := te.String(); got != errBadSignature.Error() {
		t.Fatalf("the same reason twice: %q", got)
	}
	te.add("", fmt.Errorf("other"))
	if got := te.String(); got != "a: signature does not verify; b: signature does not verify; trust 3: other" {
		t.Fatal(got)
	}
}
