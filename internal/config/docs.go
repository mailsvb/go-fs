package config

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"sync"
)

// The source of this file's own package is embedded so that the doc comments
// above every field can be read at runtime. They already document every key of
// the configuration file, and the admin interface shows them as the help text
// of the form field it generates, which keeps one description rather than two
// that have to be kept in step.
//
//go:embed config.go
var source []byte

var (
	docsOnce sync.Once
	docs     map[string]string
)

// Docs maps "StructName.FieldName" to the doc comment above that field, as a
// single line. A field with no comment is absent, and a source that cannot be
// parsed yields an empty map rather than an error: help text is a convenience,
// and its absence must not stop the server.
func Docs() map[string]string {
	docsOnce.Do(func() {
		docs = parseDocs(source)
	})
	return docs
}

func parseDocs(src []byte) map[string]string {
	found := make(map[string]string)
	file, err := parser.ParseFile(token.NewFileSet(), "config.go", src, parser.ParseComments)
	if err != nil {
		return found
	}

	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok {
			return true
		}
		structure, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range structure.Fields.List {
			// a comment above the field, or failing that one after it
			text := commentText(field.Doc)
			if text == "" {
				text = commentText(field.Comment)
			}
			if text == "" {
				continue
			}
			// several names on one line share the comment, as
			// "AdminUsername and AdminPassword are..." does
			for _, name := range field.Names {
				found[spec.Name.Name+"."+name.Name] = text
			}
		}
		return true
	})
	return found
}

// commentText joins a comment group into one line, so that a description
// wrapped over four source lines reads as a sentence in the browser.
func commentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return strings.Join(strings.Fields(group.Text()), " ")
}

// assignment matches a key line of the template, whether it is live or
// commented out: "port = 21" and "# basefolder = \"/srv/ftp\"" both name a key.
var assignment = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\s*=`)

var (
	templateOnce sync.Once
	templateDocs map[string]string
)

// TemplateDocs maps a key of the configuration file to the comment above it in
// the shipped template, as a single line. Keys are the path the file uses, so
// "ftp.maxConnections", and a repeated table contributes its fields once,
// "ftp.users.username".
//
// The template is the fuller of the two descriptions and the one written for
// whoever edits the file, so the admin interface prefers it and falls back to
// the Go doc comment of the field.
func TemplateDocs() map[string]string {
	templateOnce.Do(func() {
		templateDocs = parseTemplateDocs(template)
	})
	return templateDocs
}

func parseTemplateDocs(src []byte) map[string]string {
	found := make(map[string]string)
	section := ""
	var pending []string
	// an array value spans several lines; they are values, not prose
	inArray := false

	keep := func(key string) {
		if len(pending) > 0 && key != "" {
			if _, seen := found[key]; !seen {
				found[key] = strings.Join(pending, " ")
			}
		}
		pending = nil
	}
	// record attaches the description above a key line to that key, and notes
	// whether the value it opens continues on the following lines
	record := func(key, line string) {
		if section != "" {
			keep(section + "." + key)
		}
		pending = nil
		inArray = strings.Count(line, "[") > strings.Count(line, "]")
	}
	// enter attaches the description above a table header to the table itself,
	// which is what the tab and the record list are labelled with
	enter := func(name string) {
		keep(name)
		section = name
		inArray = false
	}

	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		commented := strings.HasPrefix(strings.TrimSpace(line), "#")

		switch {
		case inArray:
			if strings.Contains(trimmed, "]") {
				inArray = false
			}

		case trimmed == "":
			pending = nil

		case strings.HasPrefix(trimmed, "["):
			enter(strings.Trim(trimmed, "[]"))

		case assignment.MatchString(trimmed):
			record(assignment.FindStringSubmatch(trimmed)[1], trimmed)

		case commented:
			pending = append(pending, trimmed)

		default:
			pending = nil
		}
	}
	return found
}
