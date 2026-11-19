package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestMigrateRenamesTheFolderKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		want    string
		changes []string
	}{
		{
			name: "both keys, as the template of 0.11 sets them",
			file: `# the files
[general]
basefolder = "/srv/files" # served
reloadConfig = true

[http]
enabled = true
registryBaseFolder = "/srv/registry"
registryAnonymousRead = true
`,
			want: `# the files
[general]
baseFolderFiles = "/srv/files" # served
baseFolderRegistry = "/srv/registry"
reloadConfig = true

[http.main]
enabled = true

[http.services]
registryAnonymousRead = true
`,
			changes: []string{
				"general.basefolder renamed to general.baseFolderFiles",
				"http.registryBaseFolder moved to general.baseFolderRegistry",
				"http.enabled moved to http.main.enabled",
				"http.registryAnonymousRead moved to http.services.registryAnonymousRead",
			},
		},
		{
			name: "the folder of every server is removed",
			file: `[general]
basefolder = "/srv/files"

[ftp]
enabled = true
basefolder = "/srv/ftp"
# basefolder = "/srv/example"

[sftp]
  basefolder = '/srv/sftp'

[[users]]
username = "john"
basefolder = "/srv/files/john"

[http]
basefolder = "/srv/files"

[tftp]
basefolder="/srv/tftp"
`,
			want: `[general]
baseFolderFiles = "/srv/files"

[ftp]
enabled = true
# basefolder = "/srv/example"

[sftp]

[[users]]
username = "john"
basefolder = "/srv/files/john"

[http]

[tftp]
`,
			changes: []string{
				`ftp.basefolder = "/srv/ftp" removed, every server serves general.baseFolderFiles`,
				`sftp.basefolder = '/srv/sftp' removed, every server serves general.baseFolderFiles`,
				`http.basefolder = "/srv/files" removed, every server serves general.baseFolderFiles`,
				`tftp.basefolder = "/srv/tftp" removed, every server serves general.baseFolderFiles`,
				"general.basefolder renamed to general.baseFolderFiles",
			},
		},
		{
			name: "the registry goes to the top of a general without a files folder",
			file: `[general]
logLevel = "info"

[http]
registryBaseFolder = ""
`,
			want: `[general]
baseFolderRegistry = ""
logLevel = "info"

[http]
`,
			changes: []string{"http.registryBaseFolder moved to general.baseFolderRegistry"},
		},
		{
			name: "the registry goes into a general of its own",
			file: `[http]
registryBaseFolder = "/srv/registry"
`,
			want: `[http]

[general]
baseFolderRegistry = "/srv/registry"
`,
			changes: []string{"http.registryBaseFolder moved to general.baseFolderRegistry"},
		},
		{
			name: "a new key that is set wins",
			file: `[general]
basefolder = "/old"
baseFolderFiles = "/new"
baseFolderRegistry = "/new-registry"

[http]
registryBaseFolder = "/old-registry"
`,
			want: `[general]
baseFolderFiles = "/new"
baseFolderRegistry = "/new-registry"

[http]
`,
			changes: []string{
				"general.basefolder removed, general.baseFolderFiles is set",
				"http.registryBaseFolder removed, general.baseFolderRegistry is set",
			},
		},
		{
			name: "a new key that is empty gives way",
			file: `[general]
baseFolderFiles = ""
baseFolderRegistry = ""
basefolder = "/old"

[http]
registryBaseFolder = "/old-registry"
`,
			want: `[general]
baseFolderFiles = "/old"
baseFolderRegistry = "/old-registry"

[http]
`,
			changes: []string{
				"general.basefolder moved into the empty general.baseFolderFiles",
				"http.registryBaseFolder moved into the empty general.baseFolderRegistry",
			},
		},
		{
			name: "the staging folder goes below the registry",
			file: `[general]
basefolder = "/srv/files"
reloadConfig = true

[http]
registryBaseFolder = "/srv/registry"
uploadStagingFolder = "/srv/temp"
maxChunkSize = 0
`,
			want: `[general]
baseFolderFiles = "/srv/files"
baseFolderRegistry = "/srv/registry"
baseFolderTemp = "/srv/temp"
reloadConfig = true

[http.connection]
maxChunkSize = 0
`,
			changes: []string{
				"general.basefolder renamed to general.baseFolderFiles",
				"http.registryBaseFolder moved to general.baseFolderRegistry",
				"http.uploadStagingFolder moved to general.baseFolderTemp",
				"http.maxChunkSize moved to http.connection.maxChunkSize",
			},
		},
		{
			name: "the staging folder goes below a registry already in general",
			file: `[general]
baseFolderFiles = "/srv/files"
baseFolderRegistry = "/srv/registry"
reloadConfig = true

[http]
uploadStagingFolder = "/srv/temp"
`,
			want: `[general]
baseFolderFiles = "/srv/files"
baseFolderRegistry = "/srv/registry"
baseFolderTemp = "/srv/temp"
reloadConfig = true

[http]
`,
			changes: []string{"http.uploadStagingFolder moved to general.baseFolderTemp"},
		},
		{
			name: "the staging folder goes to the top of a general without folders",
			file: `[general]
logLevel = "info"

[http]
uploadStagingFolder = "/srv/temp"
`,
			want: `[general]
baseFolderTemp = "/srv/temp"
logLevel = "info"

[http]
`,
			changes: []string{"http.uploadStagingFolder moved to general.baseFolderTemp"},
		},
		{
			name: "the registry and the staging folder go into a general of their own",
			file: `[http]
registryBaseFolder = "/srv/registry"
uploadStagingFolder = "/srv/temp"
`,
			want: `[http]

[general]
baseFolderRegistry = "/srv/registry"
baseFolderTemp = "/srv/temp"
`,
			changes: []string{
				"http.registryBaseFolder moved to general.baseFolderRegistry",
				"http.uploadStagingFolder moved to general.baseFolderTemp",
			},
		},
		{
			name: "an empty temp folder gives way",
			file: `[general]
baseFolderTemp = ""

[http]
uploadStagingFolder = "/old-temp"
`,
			want: `[general]
baseFolderTemp = "/old-temp"

[http]
`,
			changes: []string{"http.uploadStagingFolder moved into the empty general.baseFolderTemp"},
		},
		{
			name: "a set temp folder wins",
			file: `[general]
baseFolderTemp = "/new-temp"

[http]
uploadStagingFolder = "/old-temp"
`,
			want: `[general]
baseFolderTemp = "/new-temp"

[http]
`,
			changes: []string{"http.uploadStagingFolder removed, general.baseFolderTemp is set"},
		},
		{
			name: "line endings are kept",
			file: "[general]\r\nbasefolder = \"/srv/files\"\r\n\r\n[http]\r\nregistryBaseFolder = \"\"\r\n",
			want: "[general]\r\nbaseFolderFiles = \"/srv/files\"\r\nbaseFolderRegistry = \"\"\r\n\r\n[http]\r\n",
			changes: []string{
				"general.basefolder renamed to general.baseFolderFiles",
				"http.registryBaseFolder moved to general.baseFolderRegistry",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "go-fs.toml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			changes, err := Migrate(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(changes, tc.changes) {
				t.Errorf("changes =\n%q\nwant\n%q", changes, tc.changes)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("file =\n%s\nwant\n%s", got, tc.want)
			}
			backup, err := os.ReadFile(path + ".bak")
			if err != nil || string(backup) != tc.file {
				t.Errorf("the backup holds %q, %v; want the file as it was", backup, err)
			}

			// a second run has nothing left to do
			again, err := Migrate(path, nil)
			if err != nil || len(again) != 0 {
				t.Errorf("a second run changed %q, %v", again, err)
			}
		})
	}
}

// A file that names the folders by the new keys alone is not touched, and no
// backup of it is made.
func TestMigrateLeavesACurrentFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	file := "[general]\nbaseFolderFiles = \"/srv/files\"\nbaseFolderRegistry = \"\"\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	changes, err := Migrate(path, nil)
	if err != nil || len(changes) != 0 {
		t.Fatalf("Migrate = %q, %v; want nothing", changes, err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made: %v", err)
	}
}

// The shipped template of the previous release, with its comments, comes out
// as a file that loads to what it did and keeps every comment.
func TestMigrateKeepsTheConfiguration(t *testing.T) {
	// a checkout on Windows may have turned the template's newlines into \r\n
	newline := "\n"
	if strings.Contains(string(template), "\r\n") {
		newline = "\r\n"
	}
	old := strings.Replace(string(template), `baseFolderFiles = "/var/lib/go-fs/files"`, `basefolder = "/srv/files"`, 1)
	old = strings.Replace(old, `baseFolderRegistry = "/var/lib/go-fs/registry"`+newline, "", 1)
	old = strings.Replace(old, `baseFolderTemp = "/var/lib/go-fs/temp"`+newline, "", 1)
	// the folder keys of the old file were keys of [http] itself
	old = strings.Replace(old, newline+"[http]"+newline, newline+"[http]"+newline+
		`registryBaseFolder = "/srv/registry"`+newline+
		`uploadStagingFolder = "/srv/temp"`+newline+
		`basefolder = "/srv/http"`+newline, 1)
	if old == string(template) || !setsOldKeys([]byte(old)) {
		t.Fatal("the template no longer has the lines this test turns back into the old keys")
	}
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("the migrated file loads to something else")
	}
	if after.General.BaseFolderRegistry != "/srv/registry" {
		t.Errorf("baseFolderRegistry = %q", after.General.BaseFolderRegistry)
	}
	if after.General.BaseFolderTemp != "/srv/temp" {
		t.Errorf("baseFolderTemp = %q", after.General.BaseFolderTemp)
	}
	if strings.Count(string(data), "\n#") != strings.Count(old, "\n#") {
		t.Error("comments were lost")
	}
}

// A layout the line editing does not understand, such as a table written
// inline, is reported and left as it was; it still loads through the old keys.
func TestMigrateRefusesWhatItCannotEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	file := "general = { basefolder = \"/srv/files\" }\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, nil); err == nil {
		t.Fatal("Migrate edited a file it cannot have understood")
	}
	if got, _ := os.ReadFile(path); string(got) != file {
		t.Errorf("the file was changed to %q", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made: %v", err)
	}
	cfg, err := Parse([]byte(file))
	if err != nil || cfg.General.BaseFolderFiles != "/srv/files" {
		t.Errorf("the file no longer loads its folder: %q, %v", cfg.General.BaseFolderFiles, err)
	}
}

func TestMigrateMovesTheHTTPKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		want    string
		changes []string
	}{
		{
			name: "each key goes into its table with its comments",
			file: `[general]
baseFolderFiles = "/srv/files"

[http]
# serve the plain port
enabled = true

port = 9000
# who may read without an account
pathsWithoutAuth = [
  "^/public/", # the public folder
  "^/media/",
]
# for instance "^/"

loginAttempts = 3
webdav = true

# the TLS listener
[https]
enabled = false
`,
			want: `[general]
baseFolderFiles = "/srv/files"

[http.main]
# serve the plain port
enabled = true

port = 9000

[http.connection]
# who may read without an account
pathsWithoutAuth = [
  "^/public/", # the public folder
  "^/media/",
]
# for instance "^/"

[http.auth]
loginAttempts = 3

[http.services]
webdav = true

# the TLS listener
[https]
enabled = false
`,
			changes: []string{
				"http.enabled moved to http.main.enabled",
				"http.port moved to http.main.port",
				"http.pathsWithoutAuth moved to http.connection.pathsWithoutAuth",
				"http.loginAttempts moved to http.auth.loginAttempts",
				"http.webdav moved to http.services.webdav",
			},
		},
		{
			name: "a table the file has gets the key at its end, and a key it sets wins",
			file: `[http]
port = 9000
address = "0.0.0.0"
maxConnections = 7

[ftp]
enabled = false

[http.main]
port = 9100
shareLinkSecret = "kept"

`,
			want: `[http.connection]
maxConnections = 7

[ftp]
enabled = false

[http.main]
port = 9100
shareLinkSecret = "kept"
address = "0.0.0.0"

`,
			changes: []string{
				"http.port removed, http.main.port is set",
				"http.address moved to http.main.address",
				"http.maxConnections moved to http.connection.maxConnections",
			},
		},
		{
			name: "a key in any case is written as its table spells it",
			file: "[http]\nPORT = 9000\nEnableS3 = false\n",
			want: "[http.main]\nport = 9000\n\n[http.services]\nenableS3 = false\n",
			changes: []string{
				"http.PORT moved to http.main.port",
				"http.EnableS3 moved to http.services.enableS3",
			},
		},
		{
			name: "a retired key stays in [http], which stays with it",
			file: `[http]
port = 9000
# no longer read
methodsRequireAuth = ["PUT"]

[[users]]
username = "max"
`,
			want: `[http]
# no longer read
methodsRequireAuth = ["PUT"]

[http.main]
port = 9000

[[users]]
username = "max"
`,
			changes: []string{"http.port moved to http.main.port"},
		},
		{
			name: "a file with Windows line ends keeps them",
			file: "[http]\r\nport = 9000\r\nwebdav = true\r\n",
			want: "[http.main]\r\nport = 9000\r\n\r\n[http.services]\r\nwebdav = true\r\n",
			changes: []string{
				"http.port moved to http.main.port",
				"http.webdav moved to http.services.webdav",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "go-fs.toml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			changes, err := Migrate(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(changes, tc.changes) {
				t.Errorf("changes =\n%q\nwant\n%q", changes, tc.changes)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("file =\n%s\nwant\n%s", got, tc.want)
			}
			if again, err := Migrate(path, nil); err != nil || len(again) != 0 {
				t.Errorf("a second run changed %q, %v", again, err)
			}
		})
	}
}

// The shipped template of the last release before [http] had its tables, with
// values of its own, comes out with every key of [http] in its table and the
// value it had, every comment kept, and the file as it was beside it.
func TestMigrateMovesTheHTTPKeysOfTheLastRelease(t *testing.T) {
	file, err := os.ReadFile(filepath.Join("testdata", "template-0.19.2.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// a checkout on Windows may have turned its newlines into \r\n
	old := strings.ReplaceAll(string(file), "\r\n", "\n")
	for from, to := range map[string]string{
		"port = 8080\n":                     "port = 9090\n",
		"webdav = false\n":                  "webdav = true\n",
		"loginAttempts = 5\n":               "loginAttempts = 9\n",
		`shareLinkSecret = ""` + "\n":       `shareLinkSecret = "` + strings.Repeat("s", 40) + `"` + "\n",
		"pathsWithoutAuth = []\n":           "pathsWithoutAuth = [\n  \"^/public/\",\n]\n",
		"trustedProxies = []\n":             "trustedProxies = [\"10.0.0.0/8\"]\n",
		"maxChunkSize = 52428800\n":         "maxChunkSize = 0\n",
		"httpSessionMaxLifetime = 604800\n": "httpSessionMaxLifetime = 86400\n",
	} {
		if !strings.Contains(old, from) {
			t.Fatalf("the template of 0.19.2 has no %q", from)
		}
		old = strings.Replace(old, from, to, 1)
	}
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Migrate(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != len(httpKeys) {
		t.Errorf("changes = %q, want one for each of the %d keys", changes, len(httpKeys))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("the migrated file loads to something else")
	}
	if after.HTTP.Port != 9090 || !after.HTTP.WebDAV || after.HTTP.LoginAttempts != 9 ||
		after.HTTP.ShareLinkSecret != strings.Repeat("s", 40) || after.HTTP.MaxChunkSize != 0 ||
		after.HTTP.SessionMaxLifetime != 86400 || !reflect.DeepEqual(after.HTTP.PathsWithoutAuth, []string{"^/public/"}) ||
		!reflect.DeepEqual(after.HTTP.TrustedProxies, []string{"10.0.0.0/8"}) {
		t.Errorf("a value was lost: %+v", after.HTTP)
	}
	// the values are where this version reads them, without the fallback
	var strict Config
	if err := toml.Unmarshal(data, &strict); err != nil || strict.HTTP.Port != 9090 || !strict.HTTP.WebDAV {
		t.Errorf("the tables do not hold the values: %+v, %v", strict.HTTP, err)
	}
	if setsOldKeys(data) || len(RetiredKeys(data)) != 0 {
		t.Errorf("old keys are left: %q", RetiredKeys(data))
	}
	for _, table := range []string{"[http]", "[http.main]", "[http.connection]", "[http.auth]", "[http.services]"} {
		want := 1
		if table == "[http]" {
			want = 0
		}
		if got := strings.Count(string(data), "\n"+table+"\n"); got != want {
			t.Errorf("%s is in the file %d times, want %d", table, got, want)
		}
	}
	if strings.Count(string(data), "\n#") != strings.Count(old, "\n#") {
		t.Error("comments were lost")
	}
	// the comment about [https] is still right above it
	if !strings.Contains(string(data), "limits of [http] above.\n[https]\n") {
		t.Error("the comment of [https] was moved away from it")
	}
	if backup, err := os.ReadFile(path + ".bak"); err != nil || string(backup) != old {
		t.Errorf("the backup is not the file as it was: %v", err)
	}
}
