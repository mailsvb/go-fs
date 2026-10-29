package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

[http]
enabled = true
registryAnonymousRead = true
`,
			changes: []string{
				"general.basefolder renamed to general.baseFolderFiles",
				"http.registryBaseFolder moved to general.baseFolderRegistry",
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
			changes, err := Migrate(path)
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
			again, err := Migrate(path)
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
	changes, err := Migrate(path)
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
	old := strings.Replace(string(template), `baseFolderFiles = "/srv/files"`, `basefolder = "/srv/files"`, 1)
	old = strings.Replace(old, `baseFolderRegistry = ""`+newline, "", 1)
	old = strings.Replace(old, "registryAnonymousRead = true", `registryBaseFolder = "/srv/registry"`+newline+"registryAnonymousRead = true", 1)
	old = strings.Replace(old, "enableAdminInterface = true", `basefolder = "/srv/http"`+newline+"enableAdminInterface = true", 1)
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
	if _, err := Migrate(path); err != nil {
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
	if _, err := Migrate(path); err == nil {
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
