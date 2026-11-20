// Package webui holds the parts of the browser pages that the file server's
// pages and the admin interface share: the Credentials overlay, which changes
// the signed in account's password. The admin interface cannot read the file
// server's assets, since the file server imports it, so what both show lives
// here.
//
// The overlay is a template, a style sheet and a script. A page parses the
// template beside its own, puts the style after its own and the script after
// its own, and executes {{template "credentials" .}} with a value that has the
// fields Credentials, where the form posts to, and User, whose password it
// changes. A button with the attribute data-credentials opens it; a page whose
// body has the class profile opens it at once and goes to / once it closes.
package webui

import (
	_ "embed"
	"html/template"
)

var (
	//go:embed credentials.html
	credentialsHTML string
	//go:embed credentials.css
	credentialsCSS string
	//go:embed credentials.js
	credentialsJS string
)

// CredentialsTemplate is the template that defines "credentials".
func CredentialsTemplate() string { return credentialsHTML }

// CredentialsStyle is the overlay's style, to follow the page's own.
func CredentialsStyle() template.CSS { return template.CSS(credentialsCSS) }

// CredentialsScript is the overlay's script, to follow the page's own.
func CredentialsScript() template.JS { return template.JS(credentialsJS) }
