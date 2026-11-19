package admin

import (
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
)

// A table nested in a section, [general.ssh], is a subsection of it: the page
// shows it under a tab of its own below the section's, and its keys are read
// and written as a table inside the section's.
func TestANestedTableIsASubsection(t *testing.T) {
	skipWithout(t, features.SFTP)
	schema, skipped := build()
	if len(skipped) != 0 {
		t.Fatalf("the schema cannot edit %v", skipped)
	}
	var general Section
	for _, section := range schema.Sections {
		if section.Key == "general" {
			general = section
		}
		if section.Key != "general" && section.Key != "http" && len(section.Subsections) != 0 {
			t.Errorf("%s has subsections %v", section.Key, section.Subsections)
		}
	}
	if len(general.Subsections) != 2 || general.Subsections[0].Key != "ssh" || general.Subsections[1].Key != "tools" {
		t.Fatalf("general has the subsections %+v, want ssh and tools", general.Subsections)
	}
	ssh := general.Subsections[0]
	if ssh.Label != "ssh" || !strings.Contains(ssh.Help, "2 MB") {
		t.Errorf("ssh is labelled %q with the help %q", ssh.Label, ssh.Help)
	}
	kinds := make(map[string]string)
	for _, field := range ssh.Fields {
		kinds[field.Key] = field.Kind
		if field.Help == "" {
			t.Errorf("general.ssh.%s has no help", field.Key)
		}
	}
	for key, want := range map[string]string{"ciphers": kindLines, "banner": kindLines, "maxSessions": kindInt} {
		if kinds[key] != want {
			t.Errorf("general.ssh.%s is %q, want %q", key, kinds[key], want)
		}
	}
	// the section's own keys stay where they were, and none of the nested
	// ones is among them
	for _, field := range general.Fields {
		if field.Key == "ciphers" || field.Key == "ssh" {
			t.Errorf("general has the key %s of its own", field.Key)
		}
	}
}

func TestASubsectionRoundTrips(t *testing.T) {
	skipWithout(t, features.SFTP)
	schema, _ := build()
	cfg := config.Default()
	cfg.General.SSH.Ciphers = []string{"aes128-gcm@openssh.com"}
	cfg.General.SSH.MaxSessions = 3
	cfg.General.SSH.Banner = []string{"one", "two"}

	first := schema.Values(cfg)
	nested, ok := first["general"].(map[string]any)["ssh"].(map[string]any)
	if !ok {
		t.Fatalf("general.ssh renders as %v", first["general"])
	}
	if nested["maxSessions"] != int64(3) {
		t.Errorf("general.ssh.maxSessions renders as %v", nested["maxSessions"])
	}
	applied, err := schema.Apply(roundTripJSON(t, first))
	if err != nil {
		t.Fatal(err)
	}
	// compared as rendered, since an empty list comes back empty rather than
	// nil, which means the same
	if second := schema.Values(applied)["general"].(map[string]any)["ssh"]; !reflect.DeepEqual(nested, second) {
		t.Errorf("general.ssh came back as %v, want %v", second, nested)
	}

	// an error in it names the key by its whole path
	first["general"].(map[string]any)["ssh"].(map[string]any)["maxSessions"] = "many"
	if _, err := schema.Apply(roundTripJSON(t, first)); err == nil ||
		!strings.Contains(err.Error(), "general.ssh.maxSessions") {
		t.Errorf("err = %v, want it to name general.ssh.maxSessions", err)
	}
}

func TestApplyWritesASubsection(t *testing.T) {
	skipWithout(t, features.SFTP)
	path := testConfig(t)
	_, front := testServer(t, path)

	body := get(t, front)
	ssh := section(t, body.Values, "general")["ssh"].(map[string]any)
	ssh["ciphers"] = []string{"chacha20-poly1305@openssh.com"}
	ssh["maxSessions"] = 4
	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}

	written, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if written.General.SSH.MaxSessions != 4 ||
		!reflect.DeepEqual(written.General.SSH.Ciphers, []string{"chacha20-poly1305@openssh.com"}) {
		t.Errorf("the file says %+v", written.General.SSH)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[general.ssh]") {
		t.Errorf("the file has no [general.ssh] table:\n%s", data)
	}

	// and one the configuration refuses is refused with its path
	body = get(t, front)
	section(t, body.Values, "general")["ssh"].(map[string]any)["ciphers"] = []string{"aes128-gmc"}
	status, answer := post(t, front, body.Values, nil)
	if status != http.StatusBadRequest || !strings.Contains(answer, "general.ssh.ciphers") {
		t.Errorf("an unknown cipher answered %d: %s", status, answer)
	}
}

// [http] keeps no key of its own: everything is in its four tables, each a
// subsection with its own description, and the section itself has nothing
// for a main tab of its own to show.
func TestHTTPIsMadeOfItsTables(t *testing.T) {
	schema, _ := build()
	var httpSection Section
	for _, section := range schema.Sections {
		if section.Key == "http" {
			httpSection = section
		}
	}
	if len(httpSection.Fields) != 0 || len(httpSection.Tables) != 0 {
		t.Errorf("http has keys of its own: %+v %+v", httpSection.Fields, httpSection.Tables)
	}
	var keys []string
	for _, sub := range httpSection.Subsections {
		keys = append(keys, sub.Key)
		if sub.Help == "" {
			t.Errorf("http.%s has no help", sub.Key)
		}
		for _, field := range sub.Fields {
			if field.Help == "" {
				t.Errorf("http.%s.%s has no help", sub.Key, field.Key)
			}
		}
	}
	if strings.Join(keys, ",") != "main,connection,auth,services" {
		t.Errorf("http has the subsections %v", keys)
	}
}

// A file that still sets the keys of [http] directly, which the server moves
// into the tables when it starts, shows their values, and is written with the
// tables and without the old keys at the first apply.
func TestApplyWritesTheHTTPTables(t *testing.T) {
	path := testConfig(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// the key the server listens on stays where the test server needs it;
	// the flat one is what an older file has
	data = append(data, []byte("\n[http]\nmaxConnections = 7\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, front := testServer(t, path)

	body := get(t, front)
	connection := section(t, body.Values, "http")["connection"].(map[string]any)
	if connection["maxConnections"] != float64(7) {
		t.Errorf("the flat key was not read: maxConnections = %v", connection["maxConnections"])
	}
	connection["maxConnections"] = 42
	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"[http.main]", "[http.connection]", "[http.auth]", "[http.services]"} {
		if !strings.Contains(string(written), table) {
			t.Errorf("the file has no %s table:\n%s", table, written)
		}
	}
	if strings.Contains(string(written), "maxConnections = 7") {
		t.Errorf("the old key is still in the file:\n%s", written)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.MaxConnections != 42 {
		t.Errorf("maxConnections = %d, want 42", cfg.HTTP.MaxConnections)
	}
}
