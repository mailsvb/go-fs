package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// movedKeys are the keys an older file names the folders by, which Parse reads
// until Migrate has renamed them.
var movedKeys = []struct{ section, key string }{
	{"general", "basefolder"},
	{"http", "registryBaseFolder"},
	{"http", "uploadStagingFolder"},
}

// droppedSections are the sections that named a folder of their own, which
// every server now takes from general.baseFolderFiles. Their basefolder is
// retired rather than moved: what it said is not used.
var droppedSections = []string{"ftp", "sftp", "http", "tftp"}

// Migrate brings the file at path to the keys this version names the folders
// by: general.basefolder becomes general.baseFolderFiles,
// http.registryBaseFolder becomes general.baseFolderRegistry,
// http.uploadStagingFolder becomes general.baseFolderTemp, and the basefolder
// of ftp, sftp, http and tftp is removed. It returns what it
// changed, nothing for a file that has nothing to change.
//
// The file is edited as text, as EnsureSecrets does, so that its comments
// survive. The result has to parse to the very configuration the file did, and
// to set none of the old keys any more, before anything is written; a file
// whose layout this does not understand is left alone and reported, and goes
// on loading as it did. Because lines are removed, the file as it was is kept
// beside it with .bak appended to its name.
func Migrate(path string) ([]string, error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	if !setsOldKeys(data) {
		return nil, nil
	}

	before, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	edited, changes := migrated(string(data))
	after, err := Parse([]byte(edited))
	if err != nil {
		return nil, fmt.Errorf("%s: migrating: %w", path, err)
	}
	if setsOldKeys([]byte(edited)) || !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("%s: cannot tell how to rename the folder keys", path)
	}

	mode := info.Mode().Perm()
	if err := os.WriteFile(target+".bak", data, mode); err != nil {
		return nil, err
	}
	if err := replaceFile(target, []byte(edited), mode); err != nil {
		return nil, err
	}
	return changes, nil
}

// setsOldKeys reports whether a file sets any key Migrate renames or removes,
// ignoring case as go-toml does when it reads one.
func setsOldKeys(data []byte) bool {
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return false
	}
	for _, moved := range movedKeys {
		if setsKeyFold(tree[moved.section], moved.key) {
			return true
		}
	}
	for _, section := range droppedSections {
		if setsKeyFold(tree[section], "basefolder") {
			return true
		}
	}
	return false
}

// setsKeyFold reports whether a loosely parsed table sets name in any case.
func setsKeyFold(section any, name string) bool {
	table, _ := section.(map[string]any)
	for key := range table {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// a key of the folders, with what is in front of it, and what follows its =
var folderKeyLine = regexp.MustCompile(
	`(?i)^(\s*)(basefolder|registryBaseFolder|uploadStagingFolder|baseFolderFiles|baseFolderRegistry|baseFolderTemp)\s*=\s*(.*)$`)

// migrated is the file with the folder keys renamed, moved and removed, and
// what was done, one entry each.
func migrated(data string) (string, []string) {
	newline := "\n"
	if strings.Contains(data, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(data, newline)

	// where the header of [general] and the keys are, by the index of their
	// line, -1 for one that is not there
	general, files, oldFiles, registry, oldRegistry, temp, oldTemp := -1, -1, -1, -1, -1, -1, -1
	drop := map[int]bool{}
	var changes []string

	section := ""
	for i, line := range lines {
		if m := sectionLine.FindStringSubmatch(line); m != nil {
			section = m[1]
			if section == "general" && general < 0 {
				general = i
			}
			continue
		}
		m := folderKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[2]
		switch {
		case section == "general" && strings.EqualFold(key, "baseFolderFiles"):
			files = i
		case section == "general" && strings.EqualFold(key, "baseFolderRegistry"):
			registry = i
		case section == "general" && strings.EqualFold(key, "basefolder"):
			oldFiles = i
		case section == "http" && strings.EqualFold(key, "registryBaseFolder"):
			oldRegistry = i
		case section == "general" && strings.EqualFold(key, "baseFolderTemp"):
			temp = i
		case section == "http" && strings.EqualFold(key, "uploadStagingFolder"):
			oldTemp = i
		case strings.EqualFold(key, "basefolder") && slices.Contains(droppedSections, section):
			drop[i] = true
			changes = append(changes, fmt.Sprintf("%s.%s = %s removed, every server serves general.baseFolderFiles",
				section, key, m[3]))
		}
	}

	// a new key that is there and set wins, as it does in Parse; one that is
	// empty is given the old key's value
	if oldFiles >= 0 {
		old := folderKeyLine.FindStringSubmatch(lines[oldFiles])
		entry := old[1] + "baseFolderFiles = " + old[3]
		switch {
		case files >= 0 && !holdsEmptyString(lines[files]):
			drop[oldFiles] = true
			changes = append(changes, "general.basefolder removed, general.baseFolderFiles is set")
		case files >= 0:
			lines[files] = entry
			drop[oldFiles] = true
			changes = append(changes, "general.basefolder moved into the empty general.baseFolderFiles")
		default:
			lines[oldFiles] = entry
			files = oldFiles
			changes = append(changes, "general.basefolder renamed to general.baseFolderFiles")
		}
	}

	// an old key of [http] is moved to its key in [general]: one that is
	// there and set wins, as it does in Parse, an empty one is given the old
	// key's value, and one that is not there yet goes below anchor, the
	// folder key before it, or at the top of [general], or into a [general]
	// of its own at the end of the file. It returns the line the key is on.
	var appended []string
	move := func(old, current, anchor int, oldName, name string) int {
		if old < 0 {
			return current
		}
		drop[old] = true
		entry := name + " = " + folderKeyLine.FindStringSubmatch(lines[old])[3]
		from, to := "http."+oldName, "general."+name
		switch {
		case current >= 0 && !holdsEmptyString(lines[current]):
			changes = append(changes, from+" removed, "+to+" is set")
		case current >= 0:
			lines[current] = folderKeyLine.FindStringSubmatch(lines[current])[1] + entry
			changes = append(changes, from+" moved into the empty "+to)
		default:
			switch {
			case anchor >= 0:
				lines[anchor] += newline + entry
				current = anchor
			case general >= 0:
				lines[general] += newline + entry
				current = general
			default:
				appended = append(appended, entry)
			}
			changes = append(changes, from+" moved to "+to)
		}
		return current
	}
	registry = move(oldRegistry, registry, files, "registryBaseFolder", "baseFolderRegistry")
	anchor := registry
	if anchor < 0 {
		anchor = files
	}
	move(oldTemp, temp, anchor, "uploadStagingFolder", "baseFolderTemp")

	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	out := strings.Join(kept, newline)
	if len(appended) > 0 {
		if out != "" && !strings.HasSuffix(out, newline) {
			out += newline
		}
		out += newline + "[general]" + newline + strings.Join(appended, newline) + newline
	}
	return out, changes
}

// holdsEmptyString reports a line that sets its key to the empty string.
func holdsEmptyString(line string) bool {
	var value map[string]any
	if err := toml.Unmarshal([]byte(line), &value); err != nil {
		return false
	}
	for _, v := range value {
		return v == ""
	}
	return false
}
