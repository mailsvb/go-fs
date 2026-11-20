package admin

import (
	"slices"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/testpki"
)

// imagesignTable finds [[http.registry.imagesign]] in the schema.
func imagesignTable(t *testing.T, schema Schema) Table {
	t.Helper()
	for _, sec := range schema.Sections {
		for _, sub := range sec.Subsections {
			for _, table := range sub.Tables {
				if sec.Key+"."+sub.Key+"."+table.Key == "http.registry.imagesign" {
					return table
				}
			}
		}
	}
	t.Fatal("http.registry.imagesign is not in the schema")
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
	if got := summaries["http.registry.imagesign.0.certificates"]; !strings.Contains(got, "CA oci-signing-ca") {
		t.Errorf("the first signer's certificates are described as %q", got)
	}
	if got := summaries["http.registry.imagesign.1.certificates"]; !strings.Contains(got, "holds no PEM") &&
		!strings.Contains(got, "neither base64 nor PEM") {
		t.Errorf("the second signer's certificates are described as %q", got)
	}
}

// verifyChain is on unless the file turns it off: the page shows an entry
// that does not set it with the switch on, a new entry starts with it on, and
// applying the page keeps it on.
func TestImageSignVerifyChainDefaultsOn(t *testing.T) {
	schema, _ := build()
	table := imagesignTable(t, schema)
	var verify Field
	for _, field := range table.Fields {
		if field.Key == "verifyChain" {
			verify = field
		}
	}
	if verify.Kind != kindBool || !verify.Default || verify.Help == "" {
		t.Fatalf("verifyChain = %+v", verify)
	}

	off := false
	cfg := config.Default()
	cfg.HTTP.ImageSign = []config.ImageSign{
		{Name: "unset", LabelPrefix: []string{"a.b"}, Certificates: "x"},
		{Name: "off", LabelPrefix: []string{"c.d"}, VerifyChain: &off},
	}
	values := schema.Values(cfg)
	records := values["http"].(map[string]any)["registry"].(map[string]any)["imagesign"].([]any)
	if got := records[0].(map[string]any)["verifyChain"]; got != true {
		t.Errorf("an entry without verifyChain shows %v", got)
	}
	if got := records[1].(map[string]any)["verifyChain"]; got != false {
		t.Errorf("an entry with verifyChain = false shows %v", got)
	}
	applied, err := schema.Apply(values)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.HTTP.ImageSign[0].ChainVerified() || applied.HTTP.ImageSign[1].ChainVerified() {
		t.Errorf("applying the page changed verifyChain: %+v", applied.HTTP.ImageSign)
	}

	// a right that is not set still reads as denied
	for _, section := range schema.Sections {
		for _, table := range section.Tables {
			for _, field := range table.Fields {
				if field.Default && field.Key != "verifyChain" {
					t.Errorf("%s.%s defaults to true", section.Key, field.Key)
				}
			}
		}
	}
}
