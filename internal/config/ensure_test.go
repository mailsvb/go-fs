package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ensureIn writes body as a configuration file, ensures its generated secrets
// and returns what the file says afterwards. The file has none at the old
// place directly in [http], so none may be moved.
func ensureIn(t *testing.T, body string) (Config, string, []string) {
	t.Helper()
	cfg, written, generated, moved := ensureMoving(t, body)
	if len(moved) != 0 {
		t.Fatalf("moved = %v", moved)
	}
	return cfg, written, generated
}

// ensureMoving is ensureIn for a file that may keep its secrets directly in
// [http], and returns what was moved from there as well.
func ensureMoving(t *testing.T, body string) (Config, string, []string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	ensured, generated, moved, err := EnsureSecrets(path, cfg, nil)
	if err != nil {
		t.Fatalf("EnsureSecrets: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ensured, string(written), generated, moved
}

// rereadSecrets parses what was written and checks that it holds the secrets
// EnsureSecrets returned, each one usable.
func rereadSecrets(t *testing.T, written string, cfg Config) Config {
	t.Helper()
	reread, err := Parse([]byte(written))
	if err != nil {
		t.Fatalf("the written file does not parse: %v\n%s", err, written)
	}
	if reread.HTTP.ShareLinkSecret != cfg.HTTP.ShareLinkSecret ||
		reread.HTTP.SessionTokenSecret != cfg.HTTP.SessionTokenSecret {
		t.Errorf("the file holds other secrets than were returned:\n%s", written)
	}
	if len(cfg.HTTP.ShareLinkSecret) < MinShareSecret {
		t.Errorf("share secret = %q", cfg.HTTP.ShareLinkSecret)
	}
	if _, err := DecodeSessionSecret(cfg.HTTP.SessionTokenSecret); err != nil {
		t.Errorf("session secret %q: %v", cfg.HTTP.SessionTokenSecret, err)
	}
	return reread
}

func TestEnsureSecretsFillsTheEmptyKeys(t *testing.T) {
	body := "# top comment\n[http.main]\n# the key below\nshareLinkSecret = \"\"\nport = 9080\n" +
		"[http.auth]\n# and this one\nhttpSessionTokenSecret = \"\"\n\n[[users]]\nusername = \"john\"\n"
	cfg, written, generated := ensureIn(t, body)
	if !slices.Equal(generated, []string{"http.main.shareLinkSecret", "http.auth.httpSessionTokenSecret"}) {
		t.Fatalf("generated = %v", generated)
	}
	want := strings.Replace(body, `shareLinkSecret = ""`,
		`shareLinkSecret = "`+cfg.HTTP.ShareLinkSecret+`"`, 1)
	want = strings.Replace(want, `httpSessionTokenSecret = ""`,
		`httpSessionTokenSecret = "`+cfg.HTTP.SessionTokenSecret+`"`, 1)
	if written != want {
		t.Errorf("the file is\n%s\nwant\n%s", written, want)
	}
	if reread := rereadSecrets(t, written, cfg); reread.HTTP.Port != 9080 {
		t.Errorf("port = %d", reread.HTTP.Port)
	}
}

func TestEnsureSecretsAddsTheKeysToTheSection(t *testing.T) {
	body := "[general]\nlogLevel = \"info\"\n\n[http.main] # served\nport = 9081\n\n" +
		"[[general.cleanup]]\npath = \"/tmp\"\n\n[http.auth]\nloginAttempts = 3\n"
	cfg, written, _ := ensureIn(t, body)
	if !strings.Contains(written, "[http.main] # served\n"+
		"shareLinkSecret = \""+cfg.HTTP.ShareLinkSecret+"\"\n"+
		"port = 9081\n") {
		t.Errorf("the key is not the first of [http.main]:\n%s", written)
	}
	if !strings.Contains(written, "[http.auth]\n"+
		"httpSessionTokenSecret = \""+cfg.HTTP.SessionTokenSecret+"\"\n"+
		"loginAttempts = 3\n") {
		t.Errorf("the key is not the first of [http.auth]:\n%s", written)
	}
	if reread := rereadSecrets(t, written, cfg); len(reread.General.Cleanup) != 1 {
		t.Errorf("cleanup = %+v", reread.General.Cleanup)
	}
}

func TestEnsureSecretsAppendsTheSection(t *testing.T) {
	body := "# only general\n[general]\nlogLevel = \"info\""
	cfg, written, _ := ensureIn(t, body)
	if !strings.HasPrefix(written, body+"\n") {
		t.Errorf("what was there changed:\n%s", written)
	}
	if strings.Count(written, "[http.main]") != 1 || strings.Count(written, "[http.auth]") != 1 {
		t.Errorf("[http.main] and [http.auth] were not each added once:\n%s", written)
	}
	if reread := rereadSecrets(t, written, cfg); reread.General.LogLevel != "info" {
		t.Errorf("logLevel = %q", reread.General.LogLevel)
	}
}

func TestEnsureSecretsInTheTemplate(t *testing.T) {
	cfg, written, generated := ensureIn(t, string(Template()))
	if len(generated) != 2 {
		t.Errorf("generated = %v", generated)
	}
	for _, key := range []string{"shareLinkSecret = ", "httpSessionTokenSecret = "} {
		if strings.Count(written, key) != 1 {
			t.Errorf("the template now sets %q %d times", key, strings.Count(written, key))
		}
	}
	rereadSecrets(t, written, cfg)
}

func TestEnsureSecretsLeavesSetSecretsAlone(t *testing.T) {
	session, err := GenerateSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := "[http.main]\nshareLinkSecret = \"" + strings.Repeat("s", 40) + "\"\n" +
		"[http.auth]\nhttpSessionTokenSecret = \"" + session + "\"\n"
	cfg, written, generated := ensureIn(t, body)
	if generated != nil || written != body || cfg.HTTP.ShareLinkSecret != strings.Repeat("s", 40) ||
		cfg.HTTP.SessionTokenSecret != session {
		t.Errorf("generated = %v, file = %q", generated, written)
	}
}

// a file that set only one of them, as one written before the session secret
// was generated does, is given the other and keeps the one it has
func TestEnsureSecretsFillsOnlyTheMissingOne(t *testing.T) {
	body := "[http.main]\nshareLinkSecret = \"" + strings.Repeat("s", 40) + "\"\n[http.auth]\nhttpSessionTokenSecret = \"\"\n"
	cfg, written, generated := ensureIn(t, body)
	if !slices.Equal(generated, []string{"http.auth.httpSessionTokenSecret"}) {
		t.Fatalf("generated = %v", generated)
	}
	if cfg.HTTP.ShareLinkSecret != strings.Repeat("s", 40) {
		t.Errorf("the share secret changed to %q", cfg.HTTP.ShareLinkSecret)
	}
	rereadSecrets(t, written, cfg)
}

func TestEnsureSecretsKeepsTheFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte("[http]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := EnsureSecrets(path, Default(), nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

// The secrets of a file written before [http] had its tables sit directly in
// [http], where they are no longer read. They are moved into their tables
// rather than replaced, so that the share links handed out and the browsers
// logged in survive the upgrade; every other key of [http] stays where it is.
func TestEnsureSecretsMovesTheFlatKeys(t *testing.T) {
	share := strings.Repeat("s", 40)
	session, err := GenerateSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	body := "# the server\n[http]\nport = 9082\nshareLinkSecret = \"" + share + "\"\n" +
		"# signs the logins\nhttpSessionTokenSecret = \"" + session + "\"\n\n[[users]]\nusername = \"john\"\n"
	cfg, written, generated, moved := ensureMoving(t, body)
	if len(generated) != 0 {
		t.Errorf("generated = %v", generated)
	}
	if !slices.Equal(moved, []string{"http.main.shareLinkSecret", "http.auth.httpSessionTokenSecret"}) {
		t.Errorf("moved = %v", moved)
	}
	if cfg.HTTP.ShareLinkSecret != share || cfg.HTTP.SessionTokenSecret != session {
		t.Errorf("the secrets are %q and %q, want those of [http]", cfg.HTTP.ShareLinkSecret, cfg.HTTP.SessionTokenSecret)
	}
	reread := rereadSecrets(t, written, cfg)
	if reread.HTTP.Port != 8080 {
		t.Errorf("port = %d, want the default: only the secrets are moved", reread.HTTP.Port)
	}
	if retired := RetiredKeys([]byte(written)); !slices.Equal(retired,
		[]string{"http.port is ignored, use http.main.port"}) {
		t.Errorf("retired keys left = %v in\n%s", retired, written)
	}
	for _, kept := range []string{"# the server\n[http]\nport = 9082\n# signs the logins\n", "[[users]]\nusername = \"john\"\n"} {
		if !strings.Contains(written, kept) {
			t.Errorf("the file lost %q:\n%s", kept, written)
		}
	}
}

// go-toml read a key of [http] in any case, so the old place is matched in any
// case as well.
func TestEnsureSecretsMovesAFlatKeyInAnyCase(t *testing.T) {
	share := strings.Repeat("s", 40)
	cfg, written, generated, moved := ensureMoving(t, "[http]\nShareLinkSecret = \""+share+"\"\n")
	if !slices.Equal(moved, []string{"http.main.shareLinkSecret"}) ||
		!slices.Equal(generated, []string{"http.auth.httpSessionTokenSecret"}) {
		t.Fatalf("moved = %v, generated = %v", moved, generated)
	}
	if cfg.HTTP.ShareLinkSecret != share || strings.Contains(written, "ShareLinkSecret") {
		t.Errorf("the file is\n%s", written)
	}
	rereadSecrets(t, written, cfg)
}

// A secret at the old place that could not be signed with is not moved: the
// table is given a new one, and the old line is left for the warning about
// retired keys to point at.
func TestEnsureSecretsDoesNotMoveAnUnusableFlatKey(t *testing.T) {
	body := "[http]\nshareLinkSecret = \"short\"\nhttpSessionTokenSecret = \"not base64\"\n"
	cfg, written, generated, moved := ensureMoving(t, body)
	if len(moved) != 0 ||
		!slices.Equal(generated, []string{"http.main.shareLinkSecret", "http.auth.httpSessionTokenSecret"}) {
		t.Fatalf("moved = %v, generated = %v", moved, generated)
	}
	if !strings.HasPrefix(written, body) {
		t.Errorf("the old lines have to stay:\n%s", written)
	}
	rereadSecrets(t, written, cfg)
}
