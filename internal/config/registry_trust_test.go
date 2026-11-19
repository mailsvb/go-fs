package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryTrust(t *testing.T) {
	folder := t.TempDir()
	keys := t.TempDir()
	key := filepath.Join(keys, "cosign.pub")
	root := filepath.Join(keys, "trusted_root.json")
	garbage := filepath.Join(keys, "garbage.pem")
	for file, from := range map[string]string{key: "cosign-release.pub", root: "trusted_root-public-good.json"} {
		data, err := os.ReadFile(filepath.Join("..", "imagesig", "testdata", from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(garbage, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := []RegistryTrust{
		{Name: "release", PublicKey: key, Repositories: []string{"acme/*"}},
		{Name: "github", TrustedRoot: root,
			Identity: "https://github.com/acme/app/.github/workflows/release.yml@refs/tags/*",
			Issuer:   "https://token.actions.githubusercontent.com"},
	}
	valid := servingConfig(folder)
	valid.HTTP.RegistryTrust = good
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid trust is refused: %v", err)
	}

	cases := []struct {
		name   string
		change func(*RegistryTrust)
		want   string
	}{
		{"no name", func(r *RegistryTrust) { r.Name = "" }, "has no name"},
		{"a space in the name", func(r *RegistryTrust) { r.Name = "my key" }, "letters, digits"},
		{"the name of the other", func(r *RegistryTrust) { r.Name = "GitHub" }, "configured twice"},
		{"nothing to verify with", func(r *RegistryTrust) { r.PublicKey = "" }, "exactly one of"},
		{"two things to verify with", func(r *RegistryTrust) { r.Certificates = key }, "exactly one of"},
		{"a relative key", func(r *RegistryTrust) { r.PublicKey = "cosign.pub" }, "absolute path"},
		{"a missing key", func(r *RegistryTrust) { r.PublicKey = filepath.Join(keys, "nope") }, "publicKey"},
		{"a folder for a key", func(r *RegistryTrust) { r.PublicKey = keys }, "is not a file"},
		{"a file that is no key", func(r *RegistryTrust) { r.PublicKey = garbage }, `"release"`},
		{"an identity with a key", func(r *RegistryTrust) { r.Identity = "x" }, "are for a trustedRoot"},
		{"a keyless trust without its issuer", func(r *RegistryTrust) {
			r.PublicKey, r.TrustedRoot, r.Identity = "", root, "https://github.com/acme/*"
		}, "needs the identity and the issuer"},
		{"an empty repository", func(r *RegistryTrust) { r.Repositories = []string{""} }, "repositories[0] is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := servingConfig(folder)
			trust := good[0]
			c.change(&trust)
			cfg.HTTP.RegistryTrust = []RegistryTrust{trust, good[1]}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want %q", err, c.want)
			}
			if err != nil && !strings.Contains(err.Error(), "http.services.registryTrust[") {
				t.Errorf("the error does not say where: %v", err)
			}
		})
	}
}

func TestRegistryTrustRoundTrips(t *testing.T) {
	file := []byte("[general]\nbaseFolderFiles = \"/x\"\n\n[http.services]\nregistryAnonymousRead = true\n\n" +
		"[[http.services.registryTrust]]\nname = \"release\"\npublicKey = \"/etc/go-fs/cosign.pub\"\n" +
		"repositories = [\"acme/*\"]\n")
	cfg, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	trust := cfg.HTTP.RegistryTrust
	if len(trust) != 1 || trust[0].Name != "release" || trust[0].PublicKey != "/etc/go-fs/cosign.pub" ||
		len(trust[0].Repositories) != 1 {
		t.Errorf("trust = %+v", trust)
	}
}
