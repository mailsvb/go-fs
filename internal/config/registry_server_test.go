package config

import (
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/testpki"
)

func TestRegistryServer(t *testing.T) {
	check := func(server Server) error {
		c := servingConfig(t.TempDir())
		c.Servers = []Server{server}
		return c.Validate()
	}
	ca := testpki.NewCA(t, "registry CA")
	client := ca.Client(t, "go-fs")
	other := ca.Client(t, "other")
	yes := true

	for name, server := range map[string]Server{
		"anonymous": {Name: "hub", Type: ServerTypeRegistry, URL: "https://registry-1.docker.io"},
		"none":      {Name: "hub", Type: ServerTypeRegistry, URL: "https://registry-1.docker.io", Auth: RegistryAuthNone},
		"basic":     {Name: "jfrog", Type: ServerTypeRegistry, URL: "https://acme.jfrog.io", Auth: RegistryAuthBasic, Username: "ci", Password: "pw"},
		"bearer":    {Name: "jfrog", Type: ServerTypeRegistry, URL: "https://acme.jfrog.io/artifactory/api/docker/local", Auth: RegistryAuthBearer, Token: "t"},
		"mtls": {Name: "jfrog", Type: ServerTypeRegistry, URL: "https://acme.jfrog.io", SkipVerify: true,
			ClientCert: testpki.Encode(client.CertPEM), ClientKey: testpki.Encode(client.KeyPEM), CACert: testpki.Encode(ca.CertPEM)},
	} {
		if err := check(server); err != nil {
			t.Errorf("a registry server with %s was refused: %v", name, err)
		}
	}

	base := Server{Name: "jfrog", Type: ServerTypeRegistry, URL: "https://acme.jfrog.io", Auth: RegistryAuthBasic, Username: "ci"}
	for name, change := range map[string]func(*Server){
		"no url":               func(s *Server) { s.URL = "" },
		"a url not read":       func(s *Server) { s.URL = "https://ACME.jfrog.io/" },
		"an unknown auth":      func(s *Server) { s.Auth = "ntlm" },
		"basic without a user": func(s *Server) { s.Username = "" },
		"basic with a token":   func(s *Server) { s.Token = "t" },
		"bearer without token": func(s *Server) { s.Auth = RegistryAuthBearer },
		"bearer with password": func(s *Server) { s.Auth, s.Token, s.Password = RegistryAuthBearer, "t", "pw" },
		"none with a login":    func(s *Server) { s.Auth = RegistryAuthNone },
		"a cert without key":   func(s *Server) { s.ClientCert = testpki.Encode(client.CertPEM) },
		"a key of another cert": func(s *Server) {
			s.ClientCert, s.ClientKey = testpki.Encode(client.CertPEM), testpki.Encode(other.KeyPEM)
		},
		"a CA that is a key": func(s *Server) { s.CACert = testpki.Encode(client.KeyPEM) },
		"a vault":            func(s *Server) { s.VaultPath, s.VaultPassword = "v", "pw" },
		"upload allowed":     func(s *Server) { s.AllowUpload = &yes },
		"delete allowed":     func(s *Server) { s.AllowDelete = &yes },
	} {
		server := base
		change(&server)
		if err := check(server); err == nil {
			t.Errorf("a registry server with %s was accepted", name)
		}
	}
}

func TestRegistryURL(t *testing.T) {
	for raw, want := range map[string]string{
		"acme.jfrog.io":                                       "https://acme.jfrog.io",
		" https://ACME.jfrog.io/ ":                            "https://acme.jfrog.io",
		"https://acme.jfrog.io/v2/":                           "https://acme.jfrog.io",
		"http://localhost:5000":                               "http://localhost:5000",
		"https://acme.jfrog.io/artifactory/api/docker/local/": "https://acme.jfrog.io/artifactory/api/docker/local",
	} {
		if got, err := RegistryURL(raw); err != nil || got != want {
			t.Errorf("RegistryURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "ftp://x.example", "https://", "https://u:p@x.example", "https://x.example/?q=1"} {
		if got, err := RegistryURL(raw); err == nil || strings.TrimSpace(got) != "" {
			t.Errorf("RegistryURL(%q) was read as %q", raw, got)
		}
	}
}
