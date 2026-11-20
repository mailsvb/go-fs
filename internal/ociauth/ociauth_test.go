package ociauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/testpki"
)

func TestParseChallenge(t *testing.T) {
	scheme, params := ParseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || params["realm"] != "https://auth.example/token" ||
		params["service"] != "registry.example" || params["scope"] != "repository:a/b:pull,push" {
		t.Errorf("%s %v", scheme, params)
	}
	scheme, params = ParseChallenge(`Basic realm=go-fs`)
	if scheme != "Basic" || params["realm"] != "go-fs" {
		t.Errorf("%s %v", scheme, params)
	}
}

func authorizer(t *testing.T, server *httptest.Server, credentials Credentials) *Authorizer {
	t.Helper()
	client := server.Client()
	client.CheckRedirect = KeepCredentialsHome
	return NewAuthorizer(client, "registry.test", credentials)
}

func TestAStaticBearerTokenIsSentAsItIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://nowhere.invalid/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	a := authorizer(t, server, Credentials{Token: "secret"})
	if err := a.Authenticate(context.Background(), server.URL, "registry:catalog:*"); err != nil {
		t.Fatal(err)
	}
	if a.Header != "Bearer secret" {
		t.Errorf("the header is %q", a.Header)
	}
}

// tokenRegistry challenges for a token of its own, which its token service
// hands out for the given login.
func tokenRegistry(t *testing.T, accept func(r *http.Request) bool) (*httptest.Server, *string) {
	t.Helper()
	var scope string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if !accept(r) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			scope = r.URL.Query().Get("scope")
			_, _ = w.Write([]byte(`{"access_token":"issued"}`))
		default:
			if r.Header.Get("Authorization") != "Bearer issued" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="test",scope="repository:x:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &scope
}

func TestATokenIsExchangedForTheScopeAsked(t *testing.T) {
	for _, c := range []struct {
		name        string
		credentials Credentials
		header      string
	}{
		{"password", Credentials{Username: "u", Password: "p"}, "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))},
		{"token with a username", Credentials{Username: "u", Token: "t"}, "Basic " + base64.StdEncoding.EncodeToString([]byte("u:t"))},
		{"token alone", Credentials{Token: "t"}, "Bearer t"},
	} {
		server, scope := tokenRegistry(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == c.header })
		a := authorizer(t, server, c.credentials)
		if err := a.Authenticate(context.Background(), server.URL, "registry:catalog:*"); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if a.Header != "Bearer issued" || *scope != "registry:catalog:*" {
			t.Errorf("%s: header %q, scope %q", c.name, a.Header, *scope)
		}
		// without a scope of its own, the challenge's is asked for
		if err := a.Answer(context.Background(), `Bearer realm="`+server.URL+`/token",scope="repository:y:pull"`, ""); err != nil ||
			*scope != "repository:y:pull" {
			t.Errorf("%s: scope %q, %v", c.name, *scope, err)
		}
	}

	server, _ := tokenRegistry(t, func(*http.Request) bool { return false })
	err := authorizer(t, server, Credentials{Token: "wrong"}).Authenticate(context.Background(), server.URL, "")
	if err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("a wrong token: %v", err)
	}
}

func TestMutualTLSAndAnOwnCA(t *testing.T) {
	ca := testpki.NewCA(t, "registry CA")
	clients := testpki.NewCA(t, "client CA")
	serverPair := ca.Server(t, "127.0.0.1")
	clientPair := clients.Client(t, "go-fs")

	pool := x509.NewCertPool()
	pool.AddCert(clients.Cert)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverPair.TLS(t)},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()

	try := func(settings TLS) error {
		transport, err := settings.Transport()
		if err != nil {
			return err
		}
		client := &http.Client{Transport: transport}
		return NewAuthorizer(client, "registry.test", Credentials{}).Authenticate(context.Background(), server.URL, "")
	}
	caCert := testpki.Encode(ca.CertPEM)
	cert, key := testpki.Encode(clientPair.CertPEM), testpki.Encode(clientPair.KeyPEM)

	if err := try(TLS{ClientCert: cert, ClientKey: key, CACert: caCert}); err != nil {
		t.Errorf("with the client certificate and the CA: %v", err)
	}
	if err := try(TLS{ClientCert: cert, ClientKey: key}); err == nil ||
		!strings.Contains(err.Error(), "untick Validate Connection") {
		t.Errorf("without the CA: %v", err)
	}
	if err := try(TLS{ClientCert: cert, ClientKey: key, SkipVerify: true}); err != nil {
		t.Errorf("without validation: %v", err)
	}
	if err := try(TLS{CACert: caCert}); err == nil {
		t.Error("without the client certificate the server was reached")
	}
	if _, err := (TLS{ClientCert: cert}).Transport(); err == nil {
		t.Error("a certificate without its key was taken")
	}
	if _, err := (TLS{ClientCert: cert, ClientKey: testpki.Encode(serverPair.KeyPEM)}).Transport(); err == nil {
		t.Error("a key of another certificate was taken")
	}
	first, _ := TLS{ClientCert: cert, ClientKey: key, CACert: caCert}.Transport()
	second, _ := TLS{ClientCert: cert, ClientKey: key, CACert: caCert}.Transport()
	if first != second {
		t.Error("the same TLS got another transport")
	}
	if shared, _ := (TLS{}).Transport(); shared != DefaultTransport {
		t.Error("no TLS options did not get the shared transport")
	}
}
