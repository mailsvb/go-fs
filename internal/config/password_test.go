package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func setPasswordIn(t *testing.T, body, name, password string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	err := SetPassword(path, name, password, nil)
	written, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	// Windows has no such bits to keep: the mode it reports is synthesised
	if info, _ := os.Stat(path); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	return string(written), err
}

func TestSetPasswordChangesOneLineAndKeepsTheComments(t *testing.T) {
	body := "# top\n[[users]]\n# first\nusername = \"ann\"\npassword = \"same\"\nhttp = true\n\n" +
		"[[users]]\nusername = 'bob' # second\n  password = 'same' # bob's\nftp = true\n"
	written, err := setPasswordIn(t, body, "bob", `new "pass" \ word`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(written))
	if err != nil {
		t.Fatalf("the result does not parse: %v\n%s", err, written)
	}
	if cfg.Users[0].Password != "same" || cfg.Users[1].Password != `new "pass" \ word` {
		t.Errorf("passwords = %q, %q", cfg.Users[0].Password, cfg.Users[1].Password)
	}
	for _, want := range []string{"# top", "# first", "# second", `  password = "new \"pass\" \\ word"`} {
		if !strings.Contains(written, want) {
			t.Errorf("the file has no %s:\n%s", want, written)
		}
	}
}

func TestSetPasswordKeepsCRLF(t *testing.T) {
	body := "[[users]]\r\nusername = \"ann\"\r\npassword = \"old\"\r\n"
	written, err := setPasswordIn(t, body, "ann", "brand-new")
	if err != nil {
		t.Fatal(err)
	}
	if written != "[[users]]\r\nusername = \"ann\"\r\npassword = \"brand-new\"\r\n" {
		t.Errorf("written = %q", written)
	}
}

func TestSetPasswordLeavesWhatItCannotPlace(t *testing.T) {
	cases := map[string]string{
		"inline":    "users = [{username = \"ann\", password = \"old\"}]\n",
		"multiline": "[[users]]\nusername = \"ann\"\npassword = \"\"\"\nold\"\"\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			written, err := setPasswordIn(t, body, "ann", "brand-new")
			if !errors.Is(err, ErrPasswordPlace) {
				t.Errorf("err = %v, want ErrPasswordPlace", err)
			}
			if written != body {
				t.Errorf("the file was changed:\n%s", written)
			}
		})
	}
	if _, err := setPasswordIn(t, "[[users]]\nusername = \"ann\"\npassword = \"old\"\n", "bob", "x"); err == nil {
		t.Error("an account that is not there was changed")
	}
}

func TestTomlStringEscapesControlCharacters(t *testing.T) {
	quoted := tomlString("a\tb\x01")
	cfg, err := Parse([]byte("[[users]]\nusername = \"a\"\npassword = " + quoted + "\n"))
	if err != nil || cfg.Users[0].Password != "a\tb\x01" {
		t.Errorf("%s reads back as %q, %v", quoted, cfg.Users[0].Password, err)
	}
}
