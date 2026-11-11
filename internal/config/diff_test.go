package config

import (
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
