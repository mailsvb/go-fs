package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mailsvb/go-fs/internal/logging"
)

// generatedSecret is a key of a table of [http] that the file is given at the
// first start when it does not set one, because a key that changes at every
// restart breaks whatever was signed with the previous one.
type generatedSecret struct {
	section  string
	key      string
	get      func(*Config) *string
	generate func() (string, error)
	// valid reports whether a value found at the key's old place, directly in
	// [http], may be signed with, as Validate would have it
	valid func(string) bool
}

// generatedSecrets are the keys EnsureSecrets fills: the share link key, so a
// link handed out survives a restart, and the session token key, so a browser
// that logged in stays logged in across a restart, an update and a change
// that rebuilds the HTTP service.
var generatedSecrets = []generatedSecret{
	{"http.main", "shareLinkSecret", func(c *Config) *string { return &c.HTTP.ShareLinkSecret }, GenerateShareSecret,
		func(value string) bool { return len(value) >= MinShareSecret }},
	{"http.auth", "httpSessionTokenSecret", func(c *Config) *string { return &c.HTTP.SessionTokenSecret }, GenerateSessionSecret,
		func(value string) bool { _, err := DecodeSessionSecret(value); return err == nil }},
}

// a table or an array of tables: [[general.cleanup]] ends [http.main] as well
var sectionLine = regexp.MustCompile(`^\s*\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)

// EnsureSecrets makes sure the file at path sets every secret of [http] that
// is generated rather than configured: http.main.shareLinkSecret and
// http.auth.httpSessionTokenSecret. cfg is what was loaded from the file; each key
// it already sets is left alone. The others are written into the file in one
// go, and their names are returned: those generated, and those moved.
//
// A file written before [http] had its tables keeps its secrets directly in
// [http], where they are no longer read. Such a secret is moved into its table
// rather than replaced by a new one, which would end every share link handed
// out and log out every browser at the upgrade. Its old line is removed; the
// value is kept, so there is nothing for a .bak to hold.
//
// The file is edited as text rather than written back by the toml marshaller,
// which would drop every comment in it: an empty line for the key in its table
// gets the value, a table without the key gets it as its first line, and a
// file without the table gets both appended. The result is parsed again
// before anything is written, so a file whose layout this does not understand
// is left alone and reported rather than broken.
func EnsureSecrets(path string, cfg Config, log *slog.Logger) (ensured Config, generated, moved []string, err error) {
	var missing []generatedSecret
	for _, secret := range generatedSecrets {
		if *secret.get(&cfg) == "" {
			missing = append(missing, secret)
		}
	}
	if len(missing) == 0 {
		return cfg, nil, nil, nil
	}

	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	info, err := logging.Stat(log, target)
	if err != nil {
		return cfg, nil, nil, err
	}
	data, err := logging.ReadFile(log, target)
	if err != nil {
		return cfg, nil, nil, err
	}

	edited := string(data)
	flat := flatSecrets(data)
	values := make([]string, len(missing))
	names := make([]string, len(missing))
	movedKeys := map[string]bool{}
	for i, secret := range missing {
		names[i] = secret.section + "." + secret.key
		if value, found := flat[secret.key]; found && secret.valid(value) {
			values[i] = value
			moved = append(moved, names[i])
			movedKeys[secret.key] = true
			edited = withoutKey(edited, "http", secret.key)
		} else {
			value, err := secret.generate()
			if err != nil {
				return cfg, nil, nil, err
			}
			values[i] = value
			generated = append(generated, names[i])
		}
		edited = withKey(edited, secret.section, secret.key, values[i])
	}
	check, err := Parse([]byte(edited))
	if err != nil {
		return cfg, nil, nil, fmt.Errorf("%s: adding %s: %w", path, strings.Join(names, ", "), err)
	}
	for i, secret := range missing {
		if *secret.get(&check) != values[i] {
			return cfg, nil, nil, fmt.Errorf("%s: cannot tell where to add %s", path, names[i])
		}
	}
	for key := range flatSecrets([]byte(edited)) {
		if movedKeys[key] {
			return cfg, nil, nil, fmt.Errorf("%s: cannot tell how to remove http.%s", path, key)
		}
	}

	if err := replaceFile(log, target, []byte(edited), info.Mode().Perm()); err != nil {
		return cfg, nil, nil, err
	}
	for i, secret := range missing {
		*secret.get(&cfg) = values[i]
	}
	return cfg, generated, moved, nil
}

// flatSecrets are the generated secrets a file sets directly in [http], where
// a version before its tables read them, by their key. go-toml read a key
// there in any case, so it is matched in any case here as well.
func flatSecrets(data []byte) map[string]string {
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return nil
	}
	table, _ := tree["http"].(map[string]any)
	found := map[string]string{}
	for key, value := range table {
		text, ok := value.(string)
		if !ok || text == "" {
			continue
		}
		for _, secret := range generatedSecrets {
			if strings.EqualFold(key, secret.key) {
				found[secret.key] = text
			}
		}
	}
	return found
}

// withoutKey is the file without the line that sets key, in any case, in the
// table section.
func withoutKey(data, section, key string) string {
	newline := "\n"
	if strings.Contains(data, "\r\n") {
		newline = "\r\n"
	}
	keyLine := regexp.MustCompile(`(?i)^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	lines := strings.Split(data, newline)
	current := ""
	for i, line := range lines {
		if m := sectionLine.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if current == section && keyLine.MatchString(line) {
			return strings.Join(slices.Delete(lines, i, i+1), newline)
		}
	}
	return data
}

// replaceFile writes data beside target and renames it over it, so that the
// watcher never reads the file half written.
func replaceFile(log *slog.Logger, target string, data []byte, mode os.FileMode) error {
	temp, err := logging.CreateTemp(log, filepath.Dir(target), ".go-fs-config-*")
	if err != nil {
		return err
	}
	defer func() { _ = logging.Remove(log, temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := logging.Chmod(log, temp.Name(), mode); err != nil {
		return err
	}
	return logging.Rename(log, temp.Name(), target)
}

// withKey is the file with key in the table section, "http.main", set to
// value.
func withKey(data, section, key, value string) string {
	newline := "\n"
	if strings.Contains(data, "\r\n") {
		newline = "\r\n"
	}
	entry := key + " = " + strconv.Quote(value)
	keyLine := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	lines := strings.Split(data, newline)

	current, header := "", -1
	for i, line := range lines {
		if m := sectionLine.FindStringSubmatch(line); m != nil {
			if current == section {
				// the table ended without the key
				break
			}
			current = m[1]
			if current == section {
				header = i
			}
			continue
		}
		if current == section && keyLine.MatchString(line) {
			lines[i] = entry
			return strings.Join(lines, newline)
		}
	}
	if header >= 0 {
		lines = append(lines[:header+1], append([]string{entry}, lines[header+1:]...)...)
		return strings.Join(lines, newline)
	}
	if data != "" && !strings.HasSuffix(data, newline) {
		data += newline
	}
	return data + newline + "[" + section + "]" + newline + entry + newline
}
