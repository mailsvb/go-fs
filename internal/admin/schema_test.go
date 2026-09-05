package admin

import (
	"reflect"
	"testing"

	"go-fs/internal/config"
)

// TestSchemaCoversTheWholeFile is the test that keeps the interface flexible:
// the form is generated, so every section and every repeated table of the
// configuration has to appear in it without anyone adding it by hand.
func TestSchemaCoversTheWholeFile(t *testing.T) {
	schema, skipped := build()
	if len(skipped) != 0 {
		t.Fatalf("the schema cannot edit %v", skipped)
	}

	sections := make(map[string]Section, len(schema.Sections))
	for _, section := range schema.Sections {
		sections[section.Key] = section
	}
	for _, name := range []string{"general", "log", "ftp", "ftps", "sftp", "http", "https", "tftp"} {
		if _, ok := sections[name]; !ok {
			t.Errorf("the schema has no %s section", name)
		}
	}
	if len(sections) != reflect.TypeOf(config.Config{}).NumField() {
		t.Errorf("the schema has %d sections, the configuration has %d",
			len(sections), reflect.TypeOf(config.Config{}).NumField())
	}

	tables := map[string]int{"ftp": 1, "sftp": 1, "http": 2, "general": 0, "log": 0}
	for name, want := range tables {
		if got := len(sections[name].Tables); got != want {
			t.Errorf("%s has %d repeated tables, want %d", name, got, want)
		}
	}
}

// TestFieldKinds checks the four shapes the page renders, including that a
// password is masked and a public key is not.
func TestFieldKinds(t *testing.T) {
	schema, _ := build()
	kinds := make(map[string]string)
	for _, section := range schema.Sections {
		for _, field := range section.Fields {
			kinds[section.Key+"."+field.Key] = field.Kind
		}
		for _, table := range section.Tables {
			for _, field := range table.Fields {
				kinds[section.Key+"."+table.Key+"."+field.Key] = field.Kind
			}
		}
	}

	for key, want := range map[string]string{
		"ftp.enabled":                   kindBool,
		"ftp.port":                      kindInt,
		"ftp.basefolder":                kindText,
		"http.maxUploadSize":            kindInt,
		"http.methodsRequireAuth":       kindLines,
		"general.adminPassword":         kindSecret,
		"general.adminCert":             kindText,
		"sftp.hostkey":                  kindSecret,
		"ftp.users.password":            kindSecret,
		"ftp.users.allowUserFileCreate": kindBool,
		"sftp.users.authorizedKeys":     kindLines,
		"http.users.paths":              kindLines,
		"http.cleanup.keep":             kindInt,
	} {
		if kinds[key] != want {
			t.Errorf("%s is %q, want %q", key, kinds[key], want)
		}
	}
}

// TestHelpComesFromTheFile checks that the description a field carries is the
// comment that documents it, so that documenting a key once documents it in the
// browser too.
func TestHelpComesFromTheFile(t *testing.T) {
	schema, _ := build()
	for _, section := range schema.Sections {
		if section.Key != "ftp" {
			continue
		}
		for _, field := range section.Fields {
			if field.Key == "maxConnections" {
				if field.Help != "maximum simultaneous control connections" {
					t.Errorf("ftp.maxConnections help is %q", field.Help)
				}
				return
			}
		}
	}
	t.Fatal("ftp.maxConnections is not in the schema")
}

// TestValuesRoundTrip is what Apply relies on: what the page is given, posted
// back unchanged, has to describe the same configuration.
func TestValuesRoundTrip(t *testing.T) {
	schema, _ := build()

	yes := true
	cfg := config.Default()
	cfg.General.Basefolder = "/srv/files"
	cfg.HTTP.MaxUploadSize = 1 << 30
	cfg.FTP.Users = []config.User{
		{Username: "john", Password: "doe", AllowUserFileRetrieve: &yes},
		{Username: "anonymous", AllowLoginWithoutPassword: &yes},
	}
	cfg.HTTP.Users = []config.HTTPUser{{Username: "max", Password: "m", Paths: []string{"^/public/"}}}
	cfg.HTTP.Cleanup = []config.Cleanup{{Path: "/iso", Keep: 10}}
	cfg.SFTP.Users = []config.User{{Username: "max", AuthorizedKeys: []string{"ssh-ed25519 AAAA max@laptop"}}}

	first := schema.Values(cfg)
	applied, err := schema.Apply(roundTripJSON(t, first))
	if err != nil {
		t.Fatal(err)
	}
	if second := schema.Values(applied); !reflect.DeepEqual(first, second) {
		t.Errorf("the values changed on the way through\nbefore %v\nafter  %v", first, second)
	}

	// the parts a round trip must not lose
	if len(applied.FTP.Users) != 2 || applied.FTP.Users[0].Username != "john" {
		t.Errorf("the accounts did not survive: %+v", applied.FTP.Users)
	}
	if !applied.FTP.Users[0].Permissions().FileRetrieve {
		t.Error("a granted permission did not survive")
	}
	if applied.FTP.Users[0].Permissions().FileCreate {
		t.Error("a permission that was never granted came back granted")
	}
	if applied.HTTP.MaxUploadSize != 1<<30 {
		t.Errorf("http.maxUploadSize is %d", applied.HTTP.MaxUploadSize)
	}
	if applied.HTTP.Cleanup[0].Keep != 10 {
		t.Errorf("http.cleanup did not survive: %+v", applied.HTTP.Cleanup)
	}
	if applied.SFTP.Users[0].AuthorizedKeys[0] != "ssh-ed25519 AAAA max@laptop" {
		t.Errorf("the authorized key did not survive: %+v", applied.SFTP.Users[0])
	}
}

// TestInheritedBasefolderStaysInherited guards the reason the interface parses
// the file rather than loading it: a section that leans on general.basefolder
// must not come back with its own copy of it.
func TestInheritedBasefolderStaysInherited(t *testing.T) {
	schema, _ := build()
	cfg := config.Default()
	cfg.General.Basefolder = "/srv/files"

	applied, err := schema.Apply(roundTripJSON(t, schema.Values(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	for name, folder := range map[string]string{
		"ftp":  applied.FTP.Basefolder,
		"sftp": applied.SFTP.Basefolder,
		"http": applied.HTTP.Basefolder,
		"tftp": applied.TFTP.Basefolder,
	} {
		if folder != "" {
			t.Errorf("%s.basefolder became %q, it should still be inherited", name, folder)
		}
	}
	if applied.Resolved().FTP.Basefolder != "/srv/files" {
		t.Error("the fallback is no longer applied")
	}
}

// TestApplyCoercesNumbers covers what JSON does to an integer: it arrives as a
// float, or as the text of an input box.
func TestApplyCoercesNumbers(t *testing.T) {
	schema, _ := build()
	values := schema.Values(config.Default())
	values["ftp"].(map[string]any)["port"] = "2121"
	values["http"].(map[string]any)["maxConnections"] = float64(50)
	values["tftp"].(map[string]any)["port"] = ""

	applied, err := schema.Apply(values)
	if err != nil {
		t.Fatal(err)
	}
	if applied.FTP.Port != 2121 || applied.HTTP.MaxConnections != 50 || applied.TFTP.Port != 0 {
		t.Errorf("ftp.port %d, http.maxConnections %d, tftp.port %d",
			applied.FTP.Port, applied.HTTP.MaxConnections, applied.TFTP.Port)
	}

	values["ftp"].(map[string]any)["port"] = "twentyone"
	if _, err := schema.Apply(values); err == nil {
		t.Error("a port that is not a number was accepted")
	}
}
