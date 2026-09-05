package config

import "testing"

// The admin interface labels its generated form with these, so an empty map
// would leave the whole page undocumented.
func TestTemplateDocs(t *testing.T) {
	docs := TemplateDocs()
	for key, want := range map[string]string{
		"ftp.maxConnections":     "maximum simultaneous control connections",
		"tftp.allowOverwrite":    "allow a write request to replace an existing file",
		"general.reloadInterval": "seconds between two checks of the file",
	} {
		if docs[key] != want {
			t.Errorf("%s is documented as %q, want %q", key, docs[key], want)
		}
	}
	// a table header carries the description of the table itself
	if docs["ftp.users"] == "" || docs["sftp"] == "" {
		t.Error("a section or a repeated table has no description")
	}
	// the fields of a commented out example are read as well
	if docs["sftp.users.basefolder"] == "" {
		t.Error("a key of a commented out example is not documented")
	}
	// a multi-line array value is not read as prose
	if got := docs["sftp.users.allowUserFileRetrieve"]; got != "" {
		t.Errorf("the lines of an array leaked into a description: %q", got)
	}
	if len(docs) < 50 {
		t.Errorf("only %d keys are documented", len(docs))
	}
}

// TestDocs covers the fallback: a field the template does not mention is
// described by the comment above it in the source.
func TestDocs(t *testing.T) {
	docs := Docs()
	if docs["HTTPUser.Paths"] == "" {
		t.Error("HTTPUser.Paths has no description")
	}
	if len(docs) < 20 {
		t.Errorf("only %d fields are documented", len(docs))
	}
}
