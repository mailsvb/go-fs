package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mailsvb/go-fs/internal/logging"
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

// Migrate brings the file at path to the keys this version reads. The folders:
// general.basefolder becomes general.baseFolderFiles,
// http.registryBaseFolder becomes general.baseFolderRegistry,
// http.uploadStagingFolder becomes general.baseFolderTemp, and the basefolder
// of ftp, sftp, http and tftp is removed. And [http]: a key a file written
// before its tables sets directly in [http] is moved into its table,
// http.port into [http.main] and so on, unless that table sets it as well,
// which wins. It returns what it changed, nothing for a file that has nothing
// to change.
//
// The file is edited as text, as EnsureSecrets does, so that its comments
// survive. The result has to parse to the very configuration the file did, and
// to set none of the old keys any more, before anything is written; a file
// whose layout this does not understand is left alone and reported, and goes
// on loading as it did. Because lines are removed, the file as it was is kept
// beside it with .bak appended to its name.
func Migrate(path string, log *slog.Logger) ([]string, error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	info, err := logging.Stat(log, target)
	if err != nil {
		return nil, err
	}
	data, err := logging.ReadFile(log, target)
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
	edited, moved := migratedHTTP(edited)
	changes = append(changes, moved...)
	after, err := Parse([]byte(edited))
	if err != nil {
		return nil, fmt.Errorf("%s: migrating: %w", path, err)
	}
	if setsOldKeys([]byte(edited)) || !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("%s: cannot tell how to move the old keys", path)
	}

	mode := info.Mode().Perm()
	if err := logging.WriteFile(log, target+".bak", data, mode); err != nil {
		return nil, err
	}
	if err := replaceFile(log, target, []byte(edited), mode); err != nil {
		return nil, err
	}
	return changes, nil
}

// setsOldKeys reports whether a file sets any key Migrate renames, moves or
// removes, ignoring case as go-toml does when it reads one.
func setsOldKeys(data []byte) bool {
	if len(flatHTTPKeys(data)) > 0 {
		return true
	}
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

// a line that sets a bare key, with what is in front of it and the key
var bareKeyLine = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)(\s*=)`)

// httpBlock is a key of a table of [http] that the file sets directly in
// [http]: its lines, the comment above it and the one that follows it
// included, the key spelled as its table has it.
type httpBlock struct {
	flatKey
	lines []string
	// blankBefore is whether an empty line stood before it
	blankBefore bool
}

// migratedHTTP is the file with the keys of the tables of [http] that it sets
// directly in [http] moved into their tables, and what was done, one entry
// each. A key goes with the comment directly above it and with one that
// follows it up to an empty line, so that the file keeps documenting it. A
// table the file has already gets the key at its end; the others are written
// where [http] was, in the order of the tables, and [http] itself goes once
// nothing but comments is left in it. A key its table sets as well is dropped,
// since the table wins. A file whose layout this does not understand comes
// back as it was, which Migrate reports.
func migratedHTTP(data string) (string, []string) {
	flat := flatHTTPKeys([]byte(data))
	if len(flat) == 0 {
		return data, nil
	}
	byName := map[string]flatKey{}
	for _, key := range flat {
		byName[key.spelled] = key
	}
	newline := "\n"
	if strings.Contains(data, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(data, newline)

	// where [http] is, and where the tables of [http] the file has already end
	header, end := -1, len(lines)
	tableEnds := map[string]int{}
	inHTTP, table := false, ""
	for i, line := range lines {
		m := sectionLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if inHTTP {
			end = i
		}
		if table != "" {
			tableEnds[table] = i
		}
		inHTTP, table = false, ""
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "[["):
		case strings.EqualFold(m[1], "http"):
			inHTTP, header = true, i
		default:
			for _, key := range httpKeys {
				if strings.EqualFold(m[1], "http."+key.table) {
					table = key.table
					tableEnds[table] = len(lines)
				}
			}
		}
	}
	if header < 0 {
		return data, nil
	}

	blank := func(line string) bool { return strings.TrimSpace(line) == "" }
	comment := func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "#") }

	// the blocks of the keys that move, and the lines of [http] they take
	var blocks []httpBlock
	taken := map[int]bool{}
	for i := header + 1; i < end; i++ {
		m := bareKeyLine.FindStringSubmatch(lines[i])
		if m == nil || taken[i] {
			continue
		}
		key, ok := byName[m[2]]
		if !ok {
			continue
		}
		// the value ends at the first line that completes it
		last := -1
		for j := i; j < end; j++ {
			var value map[string]any
			if toml.Unmarshal([]byte(strings.Join(lines[i:j+1], "\n")), &value) == nil {
				last = j
				break
			}
		}
		if last < 0 {
			return data, nil
		}
		first := i
		for first-1 > header && !taken[first-1] && comment(lines[first-1]) {
			first--
		}
		// a comment that follows the value up to an empty line is about it
		after := last + 1
		for after < end && comment(lines[after]) {
			after++
		}
		if after > last+1 && after < end && blank(lines[after]) {
			last = after - 1
		}
		block := httpBlock{flatKey: key, blankBefore: first-1 == header || blank(lines[first-1])}
		for j := first; j <= last; j++ {
			taken[j] = true
			block.lines = append(block.lines, lines[j])
		}
		block.lines[i-first] = bareKeyLine.ReplaceAllString(lines[i], "${1}"+key.key+"${3}")
		blocks = append(blocks, block)
		i = last
	}
	if len(blocks) != len(flat) {
		return data, nil
	}

	// the blocks for the tables the file has, and those written where [http]
	// was, table by table
	var changes []string
	insert := map[int][]string{}
	tables := map[string][]httpBlock{}
	var order []string
	for _, key := range slices.Concat(httpKeys, droppedHTTPKeys) {
		for _, block := range blocks {
			if block.key != key.key {
				continue
			}
			from, to := "http."+block.spelled, "http."+block.table+"."+block.key
			if block.replacement != "" {
				changes = append(changes, from+" removed, "+block.replacement+" takes its default")
				continue
			}
			if block.shadowed {
				changes = append(changes, from+" removed, "+to+" is set")
				continue
			}
			changes = append(changes, from+" moved to "+to)
			if tableEnd, ok := tableEnds[block.table]; ok {
				at := tableEnd
				for at-1 >= 0 && blank(lines[at-1]) {
					at--
				}
				if block.blankBefore {
					insert[at] = append(insert[at], "")
				}
				insert[at] = append(insert[at], block.lines...)
				continue
			}
			if _, ok := tables[block.table]; !ok {
				order = append(order, block.table)
			}
			tables[block.table] = append(tables[block.table], block)
		}
	}
	var written []string
	for _, table := range order {
		if len(written) > 0 {
			written = append(written, "")
		}
		written = append(written, "[http."+table+"]")
		for i, block := range tables[table] {
			if i > 0 && block.blankBefore {
				written = append(written, "")
			}
			written = append(written, block.lines...)
		}
	}

	// what is left of [http]: its header while a key is left in it, the rest
	// without the runs of empty lines the blocks leave behind, and the
	// comments after its last key, which are about what follows it and stay
	// in front of that
	tail := end
	for tail-1 > header && !taken[tail-1] && (blank(lines[tail-1]) || comment(lines[tail-1])) {
		tail--
	}
	var rest []string
	keyLeft := false
	for i := header + 1; i < tail; i++ {
		if taken[i] {
			continue
		}
		if blank(lines[i]) && (len(rest) == 0 || blank(rest[len(rest)-1])) {
			continue
		}
		if !blank(lines[i]) && !comment(lines[i]) {
			keyLeft = true
		}
		rest = append(rest, lines[i])
	}
	for len(rest) > 0 && blank(rest[len(rest)-1]) {
		rest = rest[:len(rest)-1]
	}
	var region []string
	if keyLeft || len(written) == 0 {
		region = append(region, lines[header])
	}
	region = append(region, rest...)
	if len(region) > 0 && len(written) > 0 {
		region = append(region, "")
	}
	region = append(region, written...)
	if tail < end && !blank(lines[tail]) || tail == end && end < len(lines) {
		region = append(region, "")
	}
	region = append(region, lines[tail:end]...)

	var out []string
	for i := 0; i <= len(lines); i++ {
		out = append(out, insert[i]...)
		if i == header {
			out = append(out, region...)
			i = end - 1
			continue
		}
		if i < len(lines) {
			out = append(out, lines[i])
		}
	}
	return strings.Join(out, newline), changes
}
