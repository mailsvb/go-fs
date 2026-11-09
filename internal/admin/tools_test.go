package admin

import (
	"strings"
	"testing"
)

// general.tools is a subsection whatever the binary was built with, its keys
// are paths typed in as text, and the empty box of each says what leaving it
// empty does on this machine.
func TestToolsAreASubsectionOfGeneral(t *testing.T) {
	schema, _ := build()
	var tools *Section
	for _, section := range schema.Sections {
		if section.Key != "general" {
			continue
		}
		for i := range section.Subsections {
			if section.Subsections[i].Key == "tools" {
				tools = &section.Subsections[i]
			}
		}
	}
	if tools == nil {
		t.Fatal("general has no subsection tools")
	}
	if !strings.Contains(tools.Help, "version flag") {
		t.Errorf("general.tools has the help %q", tools.Help)
	}
	keys := map[string]bool{}
	for _, field := range tools.Fields {
		keys[field.Key] = true
		if field.Kind != kindText || field.Help == "" {
			t.Errorf("general.tools.%s is %q with the help %q", field.Key, field.Kind, field.Help)
		}
	}
	if !keys["ytDlp"] || !keys["ffmpeg"] || len(keys) != 2 {
		t.Errorf("general.tools has the keys %v", keys)
	}

	placeholders := toolPlaceholders()
	for _, key := range []string{"general.tools.ytDlp", "general.tools.ffmpeg"} {
		if !strings.HasPrefix(placeholders[key], "empty: ") {
			t.Errorf("the empty box of %s says %q", key, placeholders[key])
		}
	}
}
