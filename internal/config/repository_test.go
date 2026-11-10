package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mailsvb/go-fs/internal/features"
)

// testSigningKey is the key the tests of internal/rpm sign with, as the file
// holds it.
func testSigningKey(t *testing.T) string {
	t.Helper()
	armored, err := os.ReadFile(filepath.Join("..", "rpm", "testdata", "foreign.asc"))
	if err != nil {
		t.Fatal(err)
	}
	return Encode(armored)
}

func TestRepositories(t *testing.T) {
	skipWithout(t, features.RPM)
	folder := t.TempDir()
	mkdir(t, folder, "rpm")
	mkdir(t, filepath.Join(folder, "rpm"), "inner")
	mkdir(t, folder, "other")
	key := testSigningKey(t)
	good := Repository{Name: "go-fs", Type: RepositoryTypeRPM, Folder: "/rpm", SigningKey: key, Keep: 5}

	valid := servingConfig(folder)
	valid.Repositories = []Repository{good, {Name: "other.repo", Type: "rpm", Folder: "/other", SigningKey: key,
		PollInterval: 10, URL: "https://files.example.com/other"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid repository is refused: %v", err)
	}

	cases := []struct {
		name   string
		change func(*Repository)
		want   string
	}{
		{"no name", func(r *Repository) { r.Name = "" }, "has no name"},
		{"a space in the name", func(r *Repository) { r.Name = "go fs" }, "letters, digits"},
		{"unknown type", func(r *Repository) { r.Type = "deb" }, `type "deb" is not one of rpm`},
		{"no folder", func(r *Repository) { r.Folder = "" }, "name the folder"},
		{"the top", func(r *Repository) { r.Folder = "/" }, "a folder of its own"},
		{"out of the served folder", func(r *Repository) { r.Folder = "/rpm/../.." }, "leaves the served folder"},
		{"not normalized", func(r *Repository) { r.Folder = "rpm/" }, `has to be "/rpm"`},
		{"missing folder", func(r *Repository) { r.Folder = "/missing" }, "missing"},
		{"no key", func(r *Repository) { r.SigningKey = "" }, "has no signingKey"},
		{"bad key", func(r *Repository) {
			r.SigningKey = Encode([]byte("-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxx\n-----END PGP PRIVATE KEY BLOCK-----\n"))
		}, "signingKey"},
		{"negative keep", func(r *Repository) { r.Keep = -1 }, "keep cannot be negative"},
		{"short interval", func(r *Repository) { r.PollInterval = 1 }, "at least 5 seconds"},
		{"bad url", func(r *Repository) { r.URL = "ftp://x" }, "https:// or http://"},
		{"url with a slash", func(r *Repository) { r.URL = "https://x/rpm/" }, `has to be "https://x/rpm"`},
	}
	for _, c := range cases {
		cfg := servingConfig(folder)
		repo := good
		c.change(&repo)
		cfg.Repositories = []Repository{repo}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}

	twice := servingConfig(folder)
	twice.Repositories = []Repository{good, good}
	twice.Repositories[1].Name = "GO-FS"
	twice.Repositories[1].Folder = "/other"
	if err := twice.Validate(); err == nil || !strings.Contains(err.Error(), "configured twice") {
		t.Errorf("a name twice: %v", err)
	}

	nested := servingConfig(folder)
	nested.Repositories = []Repository{good, good}
	nested.Repositories[1].Name = "inner"
	nested.Repositories[1].Folder = "/rpm/inner"
	if err := nested.Validate(); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("a repository inside another: %v", err)
	}
}

func TestRepositoryInterval(t *testing.T) {
	if got := (Repository{}).Interval(); got != DefaultPollInterval {
		t.Errorf("unset interval = %d", got)
	}
	if got := (Repository{PollInterval: 7}).Interval(); got != 7 {
		t.Errorf("interval = %d", got)
	}
}

func TestPGPKeyMaterial(t *testing.T) {
	skipWithout(t, features.RPM)
	key := testSigningKey(t)
	if _, err := Decode(KindPGPKey, key); err != nil {
		t.Fatal(err)
	}
	if got := Describe(KindPGPKey, key); !strings.HasPrefix(got, "RSA 2048 OpenPGP key ") {
		t.Errorf("describe = %q", got)
	}
	if got := Describe(KindPGPKey, Encode([]byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"))); !strings.HasPrefix(got, "this value ") {
		t.Errorf("describe of something else = %q", got)
	}
}

// testJARKey is one signing key of an fdroid repository for every test,
// since making one takes a while.
var testJARKey = sync.OnceValues(func() (string, error) {
	key, _, err := Generate(KindJARKey, "test")
	return key, err
})

func TestFDroidRepositories(t *testing.T) {
	skipWithout(t, features.FDroid, features.RPM)
	folder := t.TempDir()
	mkdir(t, folder, "android/repo")
	key, err := testJARKey()
	if err != nil {
		t.Fatal(err)
	}
	good := Repository{Name: "android", Type: RepositoryTypeFDroid, Folder: "/android/repo", SigningKey: key,
		URL: "https://files.example.com/android/repo", Title: "Example apps", Icon: "/icon.png"}
	valid := servingConfig(folder)
	valid.Repositories = []Repository{good}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid fdroid repository is refused: %v", err)
	}

	cases := []struct {
		name   string
		change func(*Repository)
		want   string
	}{
		{"no url", func(r *Repository) { r.URL = "" }, "needs its url"},
		{"url not ending in /repo", func(r *Repository) { r.URL = "https://files.example.com/android" }, "end in /repo"},
		{"an OpenPGP key", func(r *Repository) { r.SigningKey = testSigningKey(t) }, "signingKey"},
		{"a long title", func(r *Repository) { r.Title = strings.Repeat("x", 51) }, "at most 50"},
		{"an icon out of the folder", func(r *Repository) { r.Icon = "/../icon.png" }, "leaves the served folder"},
		{"an icon that is no image", func(r *Repository) { r.Icon = "/icon.svg" }, ".png, .jpg or .webp"},
		{"title on rpm", func(r *Repository) { r.Type = RepositoryTypeRPM; r.SigningKey = testSigningKey(t) },
			"for an fdroid, a jellyfin or a subsonic repository only"},
		{"description on rpm", func(r *Repository) {
			r.Type = RepositoryTypeRPM
			r.SigningKey = testSigningKey(t)
			r.Title = ""
			r.Icon = ""
			r.Description = "x"
		}, "description is for an fdroid repository only"},
	}
	for _, c := range cases {
		cfg := servingConfig(folder)
		repo := good
		c.change(&repo)
		cfg.Repositories = []Repository{repo}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	if got := good.DisplayName(); got != "Example apps" {
		t.Errorf("display name %q", got)
	}
	if got := (Repository{Name: "android"}).DisplayName(); got != "android" {
		t.Errorf("display name without a title %q", got)
	}
}

func TestJellyfinRepositories(t *testing.T) {
	skipWithout(t, features.Jellyfin, features.RPM)
	folder := t.TempDir()
	mkdir(t, folder, "music")
	mkdir(t, folder, "other")
	good := Repository{Name: "music", Type: RepositoryTypeJellyfin, Folder: "/music", Title: "Our music"}
	valid := servingConfig(folder)
	valid.Repositories = []Repository{good}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid jellyfin repository is refused: %v", err)
	}
	if got := valid.Resolved().HTTP.JellyfinFolders; len(got) != 1 || got[0] != "/music" {
		t.Errorf("the folders the http server protects = %q", got)
	}
	rpm := Repository{Name: "rpm", Type: RepositoryTypeRPM, Folder: "/other", SigningKey: testSigningKey(t)}
	valid.Repositories = append(valid.Repositories, rpm)
	if got := valid.JellyfinFolders(); len(got) != 1 || got[0] != "/music" {
		t.Errorf("the folders of the jellyfin repositories = %q", got)
	}
	// clients are told the names of the libraries and the first url one sets
	more := Repository{Name: "talks", Type: RepositoryTypeJellyfin, Folder: "/talks", URL: "https://files.example.com"}
	valid.Repositories = append(valid.Repositories, more)
	resolved := valid.Resolved().HTTP
	if resolved.JellyfinName != "Our music, talks" || resolved.JellyfinURL != "https://files.example.com" {
		t.Errorf("the server is %q at %q", resolved.JellyfinName, resolved.JellyfinURL)
	}

	cases := []struct {
		name   string
		change func(*Repository)
		want   string
	}{
		{"a signing key", func(r *Repository) { r.SigningKey = testSigningKey(t) }, "has no signingKey"},
		{"keep", func(r *Repository) { r.Keep = 3 }, "keep is 0"},
		{"a long title", func(r *Repository) { r.Title = strings.Repeat("x", 51) }, "at most 50"},
		{"an icon", func(r *Repository) { r.Icon = "/icon.png" }, "icon is for an fdroid repository only"},
		{"a description", func(r *Repository) { r.Description = "x" }, "description is for an fdroid"},
		{"bad url", func(r *Repository) { r.URL = "ftp://x" }, "https:// or http://"},
	}
	for _, c := range cases {
		cfg := servingConfig(folder)
		repo := good
		c.change(&repo)
		cfg.Repositories = []Repository{repo}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	if NeedsSigningKey(RepositoryTypeJellyfin) || SigningKeyKind(RepositoryTypeJellyfin) != "" {
		t.Error("a jellyfin repository asks for a signing key")
	}
}

// TestSubsonicRepositories checks a subsonic repository is a music library
// as a jellyfin one is, and that the two may share a folder, which no two
// other repositories may.
func TestSubsonicRepositories(t *testing.T) {
	skipWithout(t, features.Subsonic, features.Jellyfin)
	folder := t.TempDir()
	mkdir(t, folder, "music")
	mkdir(t, folder, "music/live")
	good := Repository{Name: "sub", Type: RepositoryTypeSubsonic, Folder: "/music", Title: "Our music"}
	valid := servingConfig(folder)
	valid.Repositories = []Repository{good}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid subsonic repository is refused: %v", err)
	}
	if got := valid.Resolved().HTTP.SubsonicFolders; len(got) != 1 || got[0] != "/music" {
		t.Errorf("the folders the http server protects = %q", got)
	}
	if NeedsSigningKey(RepositoryTypeSubsonic) || SigningKeyKind(RepositoryTypeSubsonic) != "" {
		t.Error("a subsonic repository asks for a signing key")
	}

	jellyfin := Repository{Name: "jf", Type: RepositoryTypeJellyfin, Folder: "/music"}
	valid.Repositories = []Repository{good, jellyfin}
	if err := valid.Validate(); err != nil {
		t.Errorf("a jellyfin and a subsonic repository cannot share a folder: %v", err)
	}
	resolved := valid.Resolved().HTTP
	if len(resolved.JellyfinFolders) != 1 || len(resolved.SubsonicFolders) != 1 {
		t.Errorf("the music folders are %q and %q", resolved.JellyfinFolders, resolved.SubsonicFolders)
	}
	nested := jellyfin
	nested.Folder = "/music/live"
	valid.Repositories = []Repository{good, nested}
	if err := valid.Validate(); err != nil {
		t.Errorf("a jellyfin repository in the folder of a subsonic one is refused: %v", err)
	}

	twice := good
	twice.Name = "again"
	refused := servingConfig(folder)
	refused.Repositories = []Repository{good, twice}
	if err := refused.Validate(); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("two subsonic repositories of one folder: got %v", err)
	}

	cases := []struct {
		name   string
		change func(*Repository)
		want   string
	}{
		{"a signing key", func(r *Repository) { r.SigningKey = testSigningKey(t) }, "has no signingKey"},
		{"keep", func(r *Repository) { r.Keep = 3 }, "keep is 0 for a subsonic repository"},
		{"a long title", func(r *Repository) { r.Title = strings.Repeat("x", 51) }, "at most 50"},
		{"an icon", func(r *Repository) { r.Icon = "/icon.png" }, "icon is for an fdroid repository only"},
	}
	for _, c := range cases {
		cfg := servingConfig(folder)
		repo := good
		c.change(&repo)
		cfg.Repositories = []Repository{repo}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestJARKeyMaterial(t *testing.T) {
	skipWithout(t, features.FDroid)
	key, err := testJARKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(KindJARKey, key); err != nil {
		t.Fatal(err)
	}
	if got := Describe(KindJARKey, key); !strings.HasPrefix(got, "RSA 4096 key, fingerprint ") {
		t.Errorf("describe = %q", got)
	}
	if _, err := Decode(KindJARKey, testSigningKey(t)); err == nil {
		t.Error("an OpenPGP key is taken for the key of an fdroid repository")
	}
	if SigningKeyKind(RepositoryTypeFDroid) != KindJARKey || SigningKeyKind(RepositoryTypeRPM) != KindPGPKey {
		t.Error("the kinds of signing key")
	}
}

// The folder go-fs keeps for itself at the top of the served tree is never a
// folder a sync or a repository works in, whatever its case.
func TestFoldersRefuseTheReservedFolder(t *testing.T) {
	for _, raw := range []string{"/.go-fs", ".go-fs/x", "/.GO-FS/y", "/a/../.go-fs"} {
		if _, err := RepositoryFolder(raw); err == nil {
			t.Errorf("RepositoryFolder(%q) is accepted", raw)
		}
		if _, err := SyncFolder(raw); err == nil {
			t.Errorf("SyncFolder(%q) is accepted", raw)
		}
	}
	for _, raw := range []string{"/sub/.go-fs", "/.go-fs-other"} {
		if _, err := RepositoryFolder(raw); err != nil {
			t.Errorf("RepositoryFolder(%q): %v", raw, err)
		}
		if _, err := SyncFolder(raw); err != nil {
			t.Errorf("SyncFolder(%q): %v", raw, err)
		}
	}
}

func TestReservesFolderOfTheServedTree(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".go-fs", "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	var c Config
	c.General.BaseFolderFiles = base
	if !c.reservesFolder(filepath.Join(base, ".go-fs", "inside")) {
		t.Error("a folder inside the reserved folder is not reserved")
	}
	if c.reservesFolder(filepath.Join(base, "other")) || c.reservesFolder(t.TempDir()) {
		t.Error("an ordinary folder is reserved")
	}
}
