package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/mailsvb/go-fs/internal/logging"
)

// ErrPasswordPlace is what SetPassword returns for a file whose layout it does
// not understand well enough to change one password in it.
var ErrPasswordPlace = errors.New("cannot tell where to change the password")

// IsExampleSecret reports a password this project prints in its own
// documentation, which protects nothing.
func IsExampleSecret(password string) bool {
	return exampleSecrets[password]
}

var passwordLine = regexp.MustCompile(`^(\s*)password\s*=`)

// SetPassword changes the password of the [[users]] entry named username in
// the file at path, and nothing else.
//
// The file is edited as text, as EnsureSecrets does, so that its comments
// survive: the password line of one [[users]] table is replaced. The result
// has to parse to the configuration the file held with only that password
// changed before anything is written; a file whose layout this does not
// understand — the users written as an inline array, a password spread over
// several lines — is left alone and ErrPasswordPlace reported. No copy of the
// previous file is kept: it would be one more copy of the old password.
func SetPassword(path, username, password string, log *slog.Logger) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	info, err := logging.Stat(log, target)
	if err != nil {
		return err
	}
	data, err := logging.ReadFile(log, target)
	if err != nil {
		return err
	}
	before, err := Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	index := slices.IndexFunc(before.Users, func(u User) bool { return u.Username == username })
	if index < 0 {
		return fmt.Errorf("%s: users %q is not configured", path, username)
	}
	want := before
	want.Users = slices.Clone(before.Users)
	want.Users[index].Password = password

	edited, ok := withPassword(string(data), password, func(candidate string) bool {
		after, err := Parse([]byte(candidate))
		return err == nil && reflect.DeepEqual(after, want)
	})
	if !ok {
		return fmt.Errorf("%s: %w of %q", path, ErrPasswordPlace, username)
	}
	return replaceFile(log, target, []byte(edited), info.Mode().Perm())
}

// withPassword tries the password line of each [[users]] table in turn, and
// returns the first edit that accept takes.
func withPassword(data, password string, accept func(string) bool) (string, bool) {
	newline := "\n"
	if strings.Contains(data, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(data, newline)
	inUsers := false
	for i, line := range lines {
		if m := sectionLine.FindStringSubmatch(line); m != nil {
			inUsers = m[1] == "users" && strings.HasPrefix(strings.TrimSpace(line), "[[")
			continue
		}
		if !inUsers {
			continue
		}
		m := passwordLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		candidate := slices.Clone(lines)
		candidate[i] = m[1] + "password = " + tomlString(password)
		if joined := strings.Join(candidate, newline); accept(joined) {
			return joined, true
		}
	}
	return "", false
}

// tomlString is s as a TOML basic string. Go's own quoting is not used, since
// TOML knows none of its \x and \a escapes.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
