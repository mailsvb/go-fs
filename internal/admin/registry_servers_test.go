package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
	"github.com/mailsvb/go-fs/internal/testpki"
)

// A container registry is stored once its login works, and an edit keeps
// the token and the client key unless others are given.
func TestRegistryIsStoredOnceTheLoginWorks(t *testing.T) {
	host := remotetest.NewRegistryHost(t, "", "", "", "tok")
	host.Tag("team/app", "1.0", "{}")
	path := testConfig(t)
	_, front := testServer(t, path)
	ca := testpki.NewCA(t, "registry CA")
	pair := ca.Client(t, "go-fs")
	server := serverJSON{Name: "jfrog", Type: config.ServerTypeRegistry, URL: host.URL + "/",
		Auth: config.RegistryAuthBearer, Token: "wrong"}

	status, body := postServer(t, front, ActionServerSave, serverSaveBody{Server: server})
	if status != http.StatusBadGateway || !strings.Contains(body, "refused the token") {
		t.Errorf("a wrong token answered %d: %s", status, body)
	}
	if len(loadServers(t, path)) != 0 {
		t.Error("a registry with a wrong token was stored")
	}

	server.Token = "tok"
	server.ClientCert, server.ClientKey = testpki.Encode(pair.CertPEM), testpki.Encode(pair.KeyPEM)
	status, body = postServer(t, front, ActionServerSave, serverSaveBody{Server: server})
	if status != http.StatusOK {
		t.Fatalf("the right token answered %d: %s", status, body)
	}
	stored := loadServers(t, path)
	if len(stored) != 1 || stored[0].URL != host.URL || stored[0].Auth != config.RegistryAuthBearer ||
		stored[0].Token != "tok" || stored[0].ClientKey == "" {
		t.Fatalf("the file holds %+v", stored)
	}

	// an edit without the token or the key keeps both
	server.Token, server.ClientKey = "", ""
	status, body = postServer(t, front, ActionServerSave, serverSaveBody{Was: "jfrog", Server: server})
	if status != http.StatusOK {
		t.Fatalf("an edit without a token answered %d: %s", status, body)
	}
	if stored := loadServers(t, path); stored[0].Token != "tok" || stored[0].ClientKey != testpki.Encode(pair.KeyPEM) {
		t.Errorf("after the edit the file holds %+v", stored[0])
	}

	// a certificate that is cleared takes its key with it
	server.ClientCert = ""
	status, body = postServer(t, front, ActionServerSave, serverSaveBody{Was: "jfrog", Server: server})
	if status != http.StatusOK {
		t.Fatalf("clearing the certificate answered %d: %s", status, body)
	}
	if stored := loadServers(t, path); stored[0].ClientCert != "" || stored[0].ClientKey != "" {
		t.Errorf("after clearing the certificate the file holds %+v", stored[0])
	}
}
