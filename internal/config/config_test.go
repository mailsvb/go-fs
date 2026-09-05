package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) (path string, folder string) {
	t.Helper()
	folder = t.TempDir()
	path = filepath.Join(t.TempDir(), "go-fs.toml")
	body = strings.ReplaceAll(body, "{{folder}}", strings.ReplaceAll(folder, `\`, `\\`))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, folder
}

func TestLoadAppliesDefaults(t *testing.T) {
	path, _ := writeConfig(t, `
[ftp]
basefolder = "{{folder}}"
port = 2121

[tftp]
enabled = false
basefolder = "{{folder}}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FTP.Port != 2121 {
		t.Errorf("port = %d, want 2121", cfg.FTP.Port)
	}
	// everything not mentioned keeps its default
	if cfg.FTP.MinDataPort != 1024 {
		t.Errorf("minDataPort = %d, want the default 1024", cfg.FTP.MinDataPort)
	}
	if cfg.FTP.IdleTimeout != 600 || cfg.FTP.MaxCommandLength != 4096 {
		t.Errorf("timeouts lost their defaults: %+v", cfg.FTP)
	}
	if cfg.FTP.AllowFtpBounce || cfg.FTP.AllowForeignDataConnection {
		t.Error("the protective defaults have to stay off")
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("log defaults lost: %+v", cfg.Log)
	}
}

func TestUserPermissionDefaults(t *testing.T) {
	path, _ := writeConfig(t, `
[ftp]
basefolder = "{{folder}}"

[[ftp.users]]
username = "john"
password = "doe"

[[ftp.users]]
username = "jane"
allowLoginWithoutPassword = true
allowUserFileRetrieve = true

[tftp]
enabled = false
basefolder = "{{folder}}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.FTP.Users) != 2 {
		t.Fatalf("got %d users, want 2", len(cfg.FTP.Users))
	}

	// an entry that sets nothing is granted nothing
	john := cfg.FTP.Users[0].Permissions()
	if john.FileCreate || john.FileRetrieve || john.FileOverwrite ||
		john.FileDelete || john.FolderCreate || john.FolderDelete {
		t.Errorf("john should have no permission by default: %+v", john)
	}
	if john.LoginNoPassword {
		t.Error("allowLoginWithoutPassword defaults to false")
	}

	// and what is granted explicitly is honoured
	jane := cfg.FTP.Users[1].Permissions()
	if !jane.LoginNoPassword {
		t.Error("jane should be allowed to log in without a password")
	}
	if !jane.FileRetrieve {
		t.Error("jane had allowUserFileRetrieve = true")
	}
	if jane.FileCreate {
		t.Error("jane's unset permissions stay false")
	}
}

func TestValidateRejectsBadConfiguration(t *testing.T) {
	folder := t.TempDir()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad log level", func(c *Config) { c.Log.Level = "chatty" }, "log.level"},
		{"admin without a name", func(c *Config) {
			c.General.AdminInterfaceEnabled = true
			c.General.AdminPassword = "secret"
		}, "general.adminUsername"},
		{"admin without a password", func(c *Config) {
			c.General.AdminInterfaceEnabled = true
			c.General.AdminUsername = "admin"
		}, "general.adminPassword"},
		{"admin port", func(c *Config) {
			c.General.AdminInterfaceEnabled = true
			c.General.AdminUsername = "admin"
			c.General.AdminPassword = "secret"
			c.General.AdminInterfacePort = 0
		}, "general.adminInterfacePort"},
		{"admin address", func(c *Config) {
			c.General.AdminInterfaceEnabled = true
			c.General.AdminUsername = "admin"
			c.General.AdminPassword = "secret"
			c.General.AdminInterfaceAddress = "the loopback"
		}, "general.adminInterfaceAddress"},
		{"half an admin tls pair", func(c *Config) {
			c.General.AdminInterfaceEnabled = true
			c.General.AdminUsername = "admin"
			c.General.AdminPassword = "secret"
			c.General.AdminCert = "cert.pem"
		}, "together"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"nothing enabled", func(c *Config) { c.FTP.Enabled = false; c.TFTP.Enabled = false }, "nothing to do"},
		{"ftp port", func(c *Config) { c.FTP.Port = 0 }, "ftp.port"},
		{"data port range", func(c *Config) { c.FTP.MinDataPort = 65530; c.FTP.MaxConnections = 100 }, "port range"},
		{"missing basefolder", func(c *Config) { c.FTP.Basefolder = filepath.Join(folder, "nope") }, "ftp.basefolder"},
		{"ftps port", func(c *Config) { c.FTPS.Enabled = true; c.FTPS.Port = 0 }, "ftps.port"},
		{"half a tls pair", func(c *Config) { c.FTPS.Enabled = true; c.FTPS.Cert = "cert.pem" }, "together"},
		{"user without name", func(c *Config) { c.FTP.Users = []User{{Password: "x"}} }, "no username"},
		{"sftp port", func(c *Config) { c.SFTP.Enabled = true; c.SFTP.Port = 0 }, "sftp.port"},
		{"sftp basefolder", func(c *Config) { c.SFTP.Enabled = true; c.SFTP.Basefolder = "" }, "sftp.basefolder"},
		{"sftp host key", func(c *Config) {
			c.SFTP.Enabled = true
			c.SFTP.Basefolder = folder
			c.SFTP.HostKey = "not a key"
		}, "sftp.hostkey"},
		{"sftp user without name", func(c *Config) {
			c.SFTP.Enabled = true
			c.SFTP.Basefolder = folder
			c.SFTP.Users = []User{{Password: "x"}}
		}, "no username"},
		{"broken authorized key", func(c *Config) {
			c.SFTP.Enabled = true
			c.SFTP.Basefolder = folder
			c.SFTP.Users = []User{{Username: "max", AuthorizedKeys: []string{
				"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExample max@laptop",
			}}}
		}, "sftp.users[0].authorizedKeys[0]"},
		{"sftp account with no way in", func(c *Config) {
			c.SFTP.Enabled = true
			c.SFTP.Basefolder = folder
			c.SFTP.Users = []User{{Username: "max"}}
		}, "never log in"},
		{"http port", func(c *Config) { c.HTTP.Enabled = true; c.HTTP.Port = 0 }, "http.port"},
		{"https port", func(c *Config) { c.HTTPS.Enabled = true; c.HTTPS.Port = 0 }, "https.port"},
		{"half an https pair", func(c *Config) {
			c.HTTP.Enabled = true
			c.HTTP.Basefolder = folder
			c.HTTPS.Enabled = true
			c.HTTPS.Cert = "cert.pem"
		}, "together"},
		{"http basefolder", func(c *Config) { c.HTTP.Enabled = true }, "http.basefolder"},
		{"http user without a password", func(c *Config) {
			c.HTTP.Enabled = true
			c.HTTP.Basefolder = folder
			c.HTTP.Users = []HTTPUser{{Username: "john"}}
		}, "no password"},
		{"broken user path pattern", func(c *Config) {
			c.HTTP.Enabled = true
			c.HTTP.Basefolder = folder
			c.HTTP.Users = []HTTPUser{{Username: "john", Password: "doe", Paths: []string{"([bad"}}}
		}, "http.users[0].paths[0]"},
		{"broken protected path pattern", func(c *Config) {
			c.HTTP.Enabled = true
			c.HTTP.Basefolder = folder
			c.HTTP.PathsRequireAuth = []string{"([bad"}
		}, "http.pathsRequireAuth[0]"},
		{"cleanup without a path", func(c *Config) {
			c.HTTP.Enabled = true
			c.HTTP.Basefolder = folder
			c.HTTP.Cleanup = []Cleanup{{Keep: 3}}
		}, "http.cleanup[0] has no path"},
		{"tftp type", func(c *Config) { c.TFTP.Type = "sctp" }, "tftp.type"},
		{"tftp block size", func(c *Config) { c.TFTP.MaxBlockSize = 4 }, "tftp.maxBlockSize"},
		{"tftp maxTimeout below timeout", func(c *Config) { c.TFTP.Timeout = 30; c.TFTP.MaxTimeout = 10 }, "maxTimeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.FTP.Basefolder = folder
			cfg.TFTP.Basefolder = folder
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTemplateRoundTrips(t *testing.T) {
	folder := t.TempDir()
	// the shipped template points at /srv, redirect it at a folder that exists
	body := strings.ReplaceAll(string(Template()), "/srv/files", strings.ReplaceAll(folder, `\`, `\\`))

	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped template has to load: %v", err)
	}

	// the template has to state the defaults it documents
	defaults := Default()
	if cfg.FTP.Port != defaults.FTP.Port || cfg.FTP.MinDataPort != defaults.FTP.MinDataPort ||
		cfg.TFTP.MaxBlockSize != defaults.TFTP.MaxBlockSize || cfg.TFTP.MaxTimeout != defaults.TFTP.MaxTimeout {
		t.Error("the template disagrees with the built-in defaults")
	}

	// the template leaves every server on general.basefolder
	if cfg.General.Basefolder != folder {
		t.Errorf("general.basefolder = %q, want %q", cfg.General.Basefolder, folder)
	}
	for name, got := range map[string]string{
		"ftp":  cfg.FTP.Basefolder,
		"sftp": cfg.SFTP.Basefolder,
		"http": cfg.HTTP.Basefolder,
		"tftp": cfg.TFTP.Basefolder,
	} {
		if got != folder {
			t.Errorf("%s.basefolder = %q, want the general %q", name, got, folder)
		}
	}

	// and writing it back has to produce something that loads again
	out := filepath.Join(t.TempDir(), "written.toml")
	if err := Save(out, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(out)
	if err != nil {
		t.Fatalf("a saved configuration has to load again: %v", err)
	}
	if reloaded.FTP.Port != cfg.FTP.Port || reloaded.TFTP.MaxBlockSize != cfg.TFTP.MaxBlockSize {
		t.Error("values were lost writing the configuration back")
	}
}

func TestSaveKeepsExplicitUserFlags(t *testing.T) {
	folder := t.TempDir()
	cfg := Default()
	cfg.FTP.Basefolder = folder
	cfg.TFTP.Basefolder = folder
	yes := true
	cfg.FTP.Users = []User{{Username: "john", Password: "doe", AllowUserFileDelete: &yes}}

	path := filepath.Join(t.TempDir(), "saved.toml")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	permissions := reloaded.FTP.Users[0].Permissions()
	if !permissions.FileDelete {
		t.Error("an explicit true was lost on the way through the file")
	}
	if permissions.FileCreate {
		t.Error("an unset permission should still deny after a round trip")
	}
}

func TestDecodeHostKey(t *testing.T) {
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nbody\n-----END OPENSSH PRIVATE KEY-----\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(pem))

	for _, value := range []string{encoded, pem, encoded[:20] + "\n" + encoded[20:]} {
		decoded, err := DecodeHostKey(value)
		if err != nil {
			t.Fatalf("%.20q: %v", value, err)
		}
		if string(decoded) != strings.TrimSpace(pem) && string(decoded) != pem {
			t.Errorf("%.20q decoded to %q", value, decoded)
		}
	}

	for _, value := range []string{"", "not base64 !!", base64.StdEncoding.EncodeToString([]byte("hello"))} {
		if _, err := DecodeHostKey(value); err == nil {
			t.Errorf("%q has to be refused", value)
		}
	}
}

func TestGeneralBasefolderIsTheFallback(t *testing.T) {
	shared := t.TempDir()
	own := t.TempDir()
	path, _ := writeConfig(t, `
[general]
basefolder = "`+strings.ReplaceAll(shared, `\`, `\\`)+`"

[ftp]
basefolder = "`+strings.ReplaceAll(own, `\`, `\\`)+`"

[sftp]
enabled = true
port = 2222

[[sftp.users]]
username = "john"
password = "doe"

[tftp]
enabled = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// a section that names its own folder keeps it
	if cfg.FTP.Basefolder != own {
		t.Errorf("ftp.basefolder = %q, want its own %q", cfg.FTP.Basefolder, own)
	}
	// the others fall back
	if cfg.SFTP.Basefolder != shared || cfg.TFTP.Basefolder != shared {
		t.Errorf("sftp %q and tftp %q should both be the general %q",
			cfg.SFTP.Basefolder, cfg.TFTP.Basefolder, shared)
	}
}

func TestGeneralBasefolderHasToBeAbsolute(t *testing.T) {
	cfg := Default()
	cfg.General.Basefolder = "relative/path"
	cfg.FTP.Basefolder = t.TempDir()
	cfg.TFTP.Basefolder = cfg.FTP.Basefolder
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("error = %v, want one about an absolute path", err)
	}
}

// TestAdminInterfaceCanStandAlone checks that a host brought up with only the
// web interface on is a valid configuration: it is how the rest of the file
// gets filled in.
func TestAdminInterfaceCanStandAlone(t *testing.T) {
	cfg := Default()
	cfg.FTP.Enabled = false
	cfg.TFTP.Enabled = false
	cfg.General.Basefolder = t.TempDir()
	cfg.General.AdminInterfaceEnabled = true
	cfg.General.AdminUsername = "admin"
	cfg.General.AdminPassword = "secret"

	if err := cfg.Resolved().Validate(); err != nil {
		t.Fatalf("a host with only the admin interface on was refused: %v", err)
	}
}

// TestParseLeavesTheFallbackAlone is what the admin interface depends on:
// Parse says what the file says, and only Resolved hands the fallback out.
func TestParseLeavesTheFallbackAlone(t *testing.T) {
	folder := t.TempDir()
	cfg, err := Parse([]byte("[general]\nbasefolder = " + strconv.Quote(folder) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FTP.Basefolder != "" {
		t.Errorf("Parse resolved ftp.basefolder to %q", cfg.FTP.Basefolder)
	}
	if resolved := cfg.Resolved(); resolved.FTP.Basefolder != folder {
		t.Errorf("Resolved left ftp.basefolder as %q", resolved.FTP.Basefolder)
	}
	// the copy is not shared with the original
	if cfg.FTP.Basefolder != "" {
		t.Error("Resolved changed the configuration it was called on")
	}
}
