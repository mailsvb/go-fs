// Package ociauth is how go-fs logs in to another container registry, whether
// it pulls from one or pushes to one (internal/httpd) or browses one that is a
// stored server (internal/remote): the connection, with the certificates it
// trusts and the one it shows, and the login, which the registry asks for with
// a challenge on /v2/ and which docker's registries answer with a token.
package ociauth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/tlsconf"
)

// DefaultTransport is what every client shares that trusts the roots of the
// system and shows no certificate. Nothing bounds how long a request may
// take, since a layer can be gigabytes; what is bounded is how long each step
// of it may wait.
var DefaultTransport http.RoundTripper = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	ExpectContinueTimeout: time.Second,
	MaxIdleConns:          16,
	IdleConnTimeout:       90 * time.Second,
}

// InsecureTransport is DefaultTransport for a client that was told not to
// validate the connection: it takes whatever certificate it is shown, as a
// self-signed or an internal one. It is a transport of its own, so that a
// connection it opened is never reused by a client that does validate.
var InsecureTransport = withTLS(DefaultTransport, &tls.Config{InsecureSkipVerify: true})

func withTLS(base http.RoundTripper, cfg *tls.Config) http.RoundTripper {
	transport := base.(*http.Transport).Clone()
	transport.TLSClientConfig = cfg
	return transport
}

// TransportFor is the shared transport of a client, by whether it validates
// the connection.
func TransportFor(skipVerify bool) http.RoundTripper {
	if skipVerify {
		return InsecureTransport
	}
	return DefaultTransport
}

// TLS is what a connection trusts and shows beyond the defaults. The
// certificates and the key are base64 of their PEM, as the configuration
// holds them.
type TLS struct {
	// ClientCert and ClientKey are the certificate shown to a registry that
	// asks for one, for mutual TLS. Both or neither.
	ClientCert, ClientKey string
	// CACert are the certificates the registry's is checked against in place
	// of the roots of the system.
	CACert string
	// SkipVerify takes whatever certificate the registry shows.
	SkipVerify bool
}

// transports are those built for a TLS other than the defaults, by a hash of
// it, so that the connections of a stored server are reused from one request
// to the next. There are as many as there were logins, which an admin
// stores.
var transports sync.Map

// Transport is the transport for the TLS: one of the shared ones where it
// asks for nothing more than whether to validate.
func (t TLS) Transport() (http.RoundTripper, error) {
	if t.ClientCert == "" && t.ClientKey == "" && t.CACert == "" {
		return TransportFor(t.SkipVerify), nil
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%q %q %q %t", t.ClientCert, t.ClientKey, t.CACert, t.SkipVerify)))
	if found, ok := transports.Load(key); ok {
		return found.(http.RoundTripper), nil
	}
	cfg, err := t.config()
	if err != nil {
		return nil, err
	}
	transport, _ := transports.LoadOrStore(key, withTLS(DefaultTransport, cfg))
	return transport.(http.RoundTripper), nil
}

func (t TLS) config() (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: t.SkipVerify}
	switch {
	case t.ClientCert != "" && t.ClientKey != "":
		pair, err := tlsconf.Pair(t.ClientCert, t.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("the client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	case t.ClientCert != "" || t.ClientKey != "":
		return nil, errors.New("a client certificate needs its key, and a key its certificate")
	}
	if t.CACert != "" {
		pemBytes, err := config.DecodeCertificate(t.CACert)
		if err != nil {
			return nil, fmt.Errorf("the CA certificate %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pemBytes)
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// WithCertificateHint says, of a certificate that was not trusted, how to
// accept it all the same.
func WithCertificateHint(err error) error {
	var untrusted *tls.CertificateVerificationError
	if errors.As(err, &untrusted) {
		return fmt.Errorf("%w; untick Validate Connection to accept this certificate", err)
	}
	return err
}

// KeepCredentialsHome drops the Authorization header on a redirect to any
// other host or port. net/http only drops it for another host name, and a
// bucket on the same host behind another port is someone else all the same.
func KeepCredentialsHome(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
		req.Header.Del("Authorization")
	}
	return nil
}

// Credentials are what a registry is logged in to with.
type Credentials struct {
	// Username and Password are sent as Basic, to the registry or to its
	// token service, whichever asks.
	Username, Password string
	// Token is sent as a Bearer token. A registry that turns it down with a
	// challenge for a token of its own has it exchanged at its token service:
	// as the password of Username where there is one, as a Bearer token where
	// there is not.
	Token string
}

// Empty reports anonymous credentials.
func (c Credentials) Empty() bool { return c.Username == "" && c.Token == "" }

func basic(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// secret is what is sent with Username: the token where there is one.
func (c Credentials) secret() string {
	if c.Token != "" {
		return c.Token
	}
	return c.Password
}

// Authorizer logs in to one registry and keeps the Authorization header that
// every request then carries.
type Authorizer struct {
	// Client sends the requests of the login.
	Client *http.Client
	// Host names the registry in what goes wrong.
	Host        string
	Credentials Credentials
	// Header is the Authorization header there is, "" for none yet.
	Header string
}

// NewAuthorizer is an Authorizer that starts with the token, where there is
// one: a registry that takes it directly is never asked for a challenge.
func NewAuthorizer(client *http.Client, host string, credentials Credentials) *Authorizer {
	a := &Authorizer{Client: client, Host: host, Credentials: credentials}
	if credentials.Token != "" {
		a.Header = "Bearer " + credentials.Token
	}
	return a
}

// Do sends a request, telling a certificate that is not trusted apart.
func (a *Authorizer) Do(req *http.Request) (*http.Response, error) {
	res, err := a.Client.Do(req)
	return res, WithCertificateHint(err)
}

// Authenticate asks base/v2/ what the registry wants, and gets it: a token
// for scope, or the Basic credentials on every request.
func (a *Authorizer) Authenticate(ctx context.Context, base, scope string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return err
	}
	if a.Header != "" {
		req.Header.Set("Authorization", a.Header)
	}
	res, err := a.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Host, err)
	}
	Drain(res)
	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		challenge := res.Header.Get("WWW-Authenticate")
		if challenge == "" && a.Header != "" {
			return fmt.Errorf("%s refused the token", a.Host)
		}
		return a.Answer(ctx, challenge, scope)
	default:
		return fmt.Errorf("%s answered %d at /v2/; is it a container registry?", a.Host, res.StatusCode)
	}
}

// Answer meets a challenge of the registry. scope is what a token is asked
// for, the scope of the challenge where it is "".
func (a *Authorizer) Answer(ctx context.Context, challenge, scope string) error {
	scheme, params := ParseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if a.Credentials.Username == "" {
			return fmt.Errorf("%s needs a username and password", a.Host)
		}
		a.Header = basic(a.Credentials.Username, a.Credentials.secret())
		return nil
	case "bearer":
		if scope == "" {
			scope = params["scope"]
		}
		return a.fetchToken(ctx, params, scope)
	default:
		return fmt.Errorf("%s asks for a login this server does not know: %q", a.Host, challenge)
	}
}

// fetchToken gets a token for scope from where the challenge says, with the
// credentials if there are any, anonymously if not.
func (a *Authorizer) fetchToken(ctx context.Context, params map[string]string, scope string) error {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return fmt.Errorf("%s sends to a token service that is not a URL: %q", a.Host, params["realm"])
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	switch c := a.Credentials; {
	case c.Username != "":
		req.Header.Set("Authorization", basic(c.Username, c.secret()))
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := a.Do(req)
	if err != nil {
		return fmt.Errorf("the token service of %s: %w", a.Host, err)
	}
	defer Drain(res)
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		switch {
		case a.Credentials.Username != "":
			return fmt.Errorf("%s refused the username and password", a.Host)
		case a.Credentials.Token != "":
			return fmt.Errorf("%s refused the token", a.Host)
		}
		return fmt.Errorf("%s needs a username and password", a.Host)
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("the token service of %s: %w", a.Host, Failure(res))
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&answer); err != nil {
		return fmt.Errorf("the token service of %s answered no token: %w", a.Host, err)
	}
	token := answer.Token
	if token == "" {
		token = answer.AccessToken
	}
	if token == "" {
		return fmt.Errorf("the token service of %s answered no token", a.Host)
	}
	a.Header = "Bearer " + token
	return nil
}

// ParseChallenge reads a WWW-Authenticate header: its scheme and the
// parameters after it, whose quoted values may hold commas.
func ParseChallenge(header string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, found := strings.Cut(rest, "=")
		if !found {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var value string
		if strings.HasPrefix(after, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				b.WriteByte(after[i])
			}
			value, rest = b.String(), after[min(i+1, len(after)):]
		} else {
			value, rest, _ = strings.Cut(after, ",")
			value = strings.TrimSpace(value)
			rest = "," + rest
		}
		params[key] = value
		rest = strings.TrimLeft(rest, ", ")
	}
	return scheme, params
}

// Failure reads the errors of an answer as the specification writes them,
// or whatever else the body says.
func Failure(res *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var answer struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &answer) == nil && len(answer.Errors) > 0 {
		var parts []string
		for _, entry := range answer.Errors {
			part := entry.Code
			if entry.Message != "" {
				part += ": " + entry.Message
			}
			parts = append(parts, part)
		}
		return fmt.Errorf("%d %s", res.StatusCode, strings.Join(parts, "; "))
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 200 || strings.HasPrefix(text, "<") {
		text = ""
	}
	if text == "" {
		text = http.StatusText(res.StatusCode)
	}
	return fmt.Errorf("%d %s", res.StatusCode, text)
}

// Drain reads what is left of a body and closes it, so the connection can be
// used again.
func Drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
}
