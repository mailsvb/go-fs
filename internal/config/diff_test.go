package config

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestChangedKeysNamesKeysNotValues(t *testing.T) {
	before := Default()
	before.Users = []User{{Username: "john", Password: "old"}}
	after := Default()
	after.Users = []User{{Username: "john", Password: "newsecret"}}
	after.HTTP.Port = before.HTTP.Port + 1

	got := ChangedKeys(before, after)
	if !slices.Contains(got, "users[0].password") || !slices.Contains(got, "http.port") || len(got) != 2 {
		t.Errorf("ChangedKeys = %q", got)
	}
	if strings.Contains(strings.Join(got, " "), "newsecret") {
		t.Errorf("a value is named: %q", got)
	}

	after.Users = append(after.Users, User{Username: "jane"})
	if got := ChangedKeys(before, after); !slices.Contains(got, "users") {
		t.Errorf("a list that grew is not named whole: %q", got)
	}
	if got := ChangedKeys(before, before); len(got) != 0 {
		t.Errorf("nothing changed, but ChangedKeys = %q", got)
	}
}

// The configuration in effect can be logged whole: every key is there, and
// the value of every key that holds a credential is masked.
func TestTheConfigurationLogsWithoutCredentials(t *testing.T) {
	cfg := Default()
	cfg.Users = []User{{Username: "john", Password: "userpassword", HTTP: true}}
	cfg.Servers = []Server{{Name: "backup", Type: ServerTypeSFTP, Host: "h", Password: "serverpassword", VaultPassword: "vaultpassword"}}
	cfg.HTTP.ShareLinkSecret = "sharesecret"
	cfg.Tokens = []Token{{Name: "phone", Hash: "tokenhash"}}

	var out strings.Builder
	slog.New(slog.NewTextHandler(&out, nil)).Info("configuration in effect", "config", cfg)
	logged := out.String()
	for _, secret := range []string{"userpassword", "serverpassword", "vaultpassword", "sharesecret", "tokenhash"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the record holds %q", secret)
		}
	}
	for _, want := range []string{"config.users[0].username=john", "config.users[0].password=***", "config.http.port=",
		"config.servers[0].host=h", "config.tokens[0].name=phone"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the record lacks %q:\n%s", want, logged)
		}
	}
}
