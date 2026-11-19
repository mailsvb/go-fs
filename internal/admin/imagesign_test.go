package admin

import (
	"slices"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/testpki"
)

// imagesignTable finds [[http.services.imagesign]] in the schema.
func imagesignTable(t *testing.T, schema Schema) Table {
	t.Helper()
	for _, sec := range schema.Sections {
		for _, sub := range sec.Subsections {
			for _, table := range sub.Tables {
				if sec.Key+"."+sub.Key+"."+table.Key == "http.services.imagesign" {
					return table
				}
			}
		}
	}
	t.Fatal("http.services.imagesign is not in the schema")
	return Table{}
}

func TestImageSignOnThePage(t *testing.T) {
	schema, _ := build()
	table := imagesignTable(t, schema)
	byKey := map[string]Field{}
	for _, field := range table.Fields {
		byKey[field.Key] = field
	}
	if mode := byKey["mode"]; !slices.Equal(mode.Choices, []string{"optional", "mandatory", "enforced"}) {
		t.Errorf("mode offers %v", mode.Choices)
	}
	if prefixes := byKey["labelPrefix"]; prefixes.Kind != kindLines {
		t.Errorf("labelPrefix is %q, want lines", prefixes.Kind)
	}
	if !byKey["name"].Summary {
		t.Error("a folded signer is not named by its name")
	}
	if certificates := byKey["certificates"]; certificates.Placeholder == "" || certificates.Help == "" {
		t.Errorf("certificates = %+v", certificates)
	}

	ca := testpki.NewCA(t, "oci-signing-ca")
	cfg := config.Default()
	cfg.HTTP.ImageSign = []config.ImageSign{
		{Name: "first", LabelPrefix: []string{"a.b"}, Certificates: string(ca.CertPEM)},
		{Name: "second", LabelPrefix: []string{"c.d"}, Certificates: "garbage"},
	}
	summaries := schema.Summaries(schema.Values(cfg))
	if got := summaries["http.services.imagesign.0.certificates"]; !strings.Contains(got, "CA oci-signing-ca") {
		t.Errorf("the first signer's certificates are described as %q", got)
	}
	if got := summaries["http.services.imagesign.1.certificates"]; !strings.Contains(got, "holds no PEM") &&
		!strings.Contains(got, "neither base64 nor PEM") {
		t.Errorf("the second signer's certificates are described as %q", got)
	}
}
