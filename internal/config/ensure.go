package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
}

// generatedSecrets are the keys EnsureSecrets fills: the share link key, so a
// link handed out survives a restart, and the session token key, so a browser
// that logged in stays logged in across a restart, an update and a change
// that rebuilds the HTTP service.
var generatedSecrets = []generatedSecret{
	{"http.main", "shareLinkSecret", func(c *Config) *string { return &c.HTTP.ShareLinkSecret }, GenerateShareSecret},
	{"http.auth", "httpSessionTokenSecret", func(c *Config) *string { return &c.HTTP.SessionTokenSecret }, GenerateSessionSecret},
}

// a table or an array of tables: [[general.cleanup]] ends [http.main] as well
var sectionLine = regexp.MustCompile(`^\s*\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)

// EnsureSecrets makes sure the file at path sets every secret of [http] that
// is generated rather than configured: http.main.shareLinkSecret and
// http.auth.httpSessionTokenSecret. cfg is what was loaded from the file; each key
// it already sets is left alone, and so is one that a file written before
// [http] had its tables sets directly in [http]: Parse reads it from there and
// Migrate moves it into its table, since a new one would end every share link
// handed out and log out every browser at the upgrade. The others are
// generated and written into the file in one go, and their names are returned.
//
// The file is edited as text rather than written back by the toml marshaller,
// which would drop every comment in it: an empty line for the key in its table
// gets the value, a table without the key gets it as its first line, and a
// file without the table gets both appended. The result is parsed again
// before anything is written, so a file whose layout this does not understand
// is left alone and reported rather than broken.
func EnsureSecrets(path string, cfg Config, log *slog.Logger) (Config, []string, error) {
	var missing []generatedSecret
	for _, secret := range generatedSecrets {
		if *secret.get(&cfg) == "" {
			missing = append(missing, secret)
		}
	}
	if len(missing) == 0 {
		return cfg, nil, nil
	}

	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	info, err := logging.Stat(log, target)
	if err != nil {
		return cfg, nil, err
	}
	data, err := logging.ReadFile(log, target)
	if err != nil {
		return cfg, nil, err
	}

	edited := string(data)
	values := make([]string, len(missing))
	names := make([]string, len(missing))
	for i, secret := range missing {
		value, err := secret.generate()
		if err != nil {
			return cfg, nil, err
		}
		values[i], names[i] = value, secret.section+"."+secret.key
		edited = withKey(edited, secret.section, secret.key, value)
	}
	check, err := Parse([]byte(edited))
	if err != nil {
		return cfg, nil, fmt.Errorf("%s: adding %s: %w", path, strings.Join(names, ", "), err)
	}
	for i, secret := range missing {
		if *secret.get(&check) != values[i] {
			return cfg, nil, fmt.Errorf("%s: cannot tell where to add %s", path, names[i])
		}
	}

	if err := replaceFile(log, target, []byte(edited), info.Mode().Perm()); err != nil {
		return cfg, nil, err
	}
	for i, secret := range missing {
		*secret.get(&cfg) = values[i]
	}
	return cfg, names, nil
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
