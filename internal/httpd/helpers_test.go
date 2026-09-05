package httpd

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go-fs/internal/config"
)

// logStore collects the records a server writes.
type logStore struct {
	mu      sync.Mutex
	records []map[string]any
}

func (s *logStore) add(record map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
}

func (s *logStore) find(message string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.records {
		if record["msg"] == message {
			return record
		}
	}
	return nil
}

type recorder struct {
	store *logStore
	attrs []slog.Attr
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, record slog.Record) error {
	fields := map[string]any{"msg": record.Message}
	for _, attr := range r.attrs {
		fields[attr.Key] = attr.Value.Any()
	}
	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value.Any()
		return true
	})
	r.store.add(fields)
	return nil
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, r.attrs...), attrs...)
	return &recorder{store: r.store, attrs: merged}
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testServer struct {
	*Server
	base string
	logs *logStore
}

// fullUser is an account that may reach everything and do everything.
func fullUser(name, password string) config.HTTPUser {
	return config.HTTPUser{
		Username:            name,
		Password:            password,
		Paths:               []string{"^/.*"},
		AllowUserFileUpload: true,
		AllowUserFileDelete: true,
	}
}

// newServer starts a server on an ephemeral port. By default nothing is
// public: every method needs the account "john"/"doe".
func newServer(t *testing.T, tune func(*config.HTTP)) *testServer {
	t.Helper()
	return newServerWith(t, tune, nil)
}

func newServerWith(t *testing.T, tune func(*config.HTTP), tuneTLS func(*config.HTTPS)) *testServer {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default().HTTP
	cfg.Enabled = true
	cfg.Port = 0
	cfg.Basefolder = base
	cfg.LoginFailureDelay = 0
	cfg.PathsRequireAuth = []string{"^/private/.*"}
	cfg.Users = []config.HTTPUser{fullUser("john", "doe")}
	if tune != nil {
		tune(&cfg)
	}
	https := config.Default().HTTPS
	if tuneTLS != nil {
		https.Port = 0
		tuneTLS(&https)
	}
	if cfg.Basefolder != base {
		base = cfg.Basefolder
	}

	logs := &logStore{}
	server, err := New(cfg, https, slog.New(&recorder{store: logs}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = server.Shutdown(context.Background())
	})
	return &testServer{Server: server, base: base, logs: logs}
}

// write puts a file into the served folder, creating the folders above it.
func (s *testServer) write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(s.base, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (s *testServer) read(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(s.base, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func (s *testServer) url(path string) string {
	port := 0
	if addr, ok := s.Addr().(*net.TCPAddr); ok {
		port = addr.Port
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path
}

// do sends a request and returns the response, leaving the body to the caller.
func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// get is a plain request with no credentials.
func get(t *testing.T, server *testServer, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := do(t, req)
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// basic sends a request authenticated with Basic.
func basic(t *testing.T, server *testServer, method, path, name, password string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.url(path), body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(name+":"+password)))
	return do(t, req)
}

// digestRequest answers a challenge the way a browser does: it sends the
// request once, reads the WWW-Authenticate header and sends it again.
func digestRequest(t *testing.T, server *testServer, method, path, name, password, userAgent string, body io.Reader) *http.Response {
	t.Helper()
	first, err := http.NewRequest(method, server.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if userAgent != "" {
		first.Header.Set("User-Agent", userAgent)
	}
	challenge := do(t, first)
	if challenge.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a challenge, got %d", challenge.StatusCode)
	}
	params := parseDigest(challenge.Header.Get("WWW-Authenticate"))

	req, err := http.NewRequest(method, server.url(path), body)
	if err != nil {
		t.Fatal(err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Authorization", digestHeader(t, params, method, path, name, password, true))
	return do(t, req)
}

// digestHeader builds an Authorization header for the challenge in params.
func digestHeader(t *testing.T, params map[string]string, method, uri, name, password string, withQop bool) string {
	t.Helper()
	digest, ok := hasher(params["algorithm"])
	if !ok {
		t.Fatalf("unsupported algorithm %q", params["algorithm"])
	}
	ha1 := digest(name + ":" + params["realm"] + ":" + password)
	ha2 := digest(method + ":" + uri)

	if !withQop {
		response := digest(ha1 + ":" + params["nonce"] + ":" + ha2)
		return fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q, algorithm=%s`,
			name, params["realm"], params["nonce"], uri, response, params["algorithm"])
	}

	cnonce := randomHex(8)
	response := digest(strings.Join([]string{ha1, params["nonce"], "00000001", cnonce, "auth", ha2}, ":"))
	return fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, qop=auth, nc=00000001, `+
		`cnonce=%q, response=%q, algorithm=%s`,
		name, params["realm"], params["nonce"], uri, cnonce, response, params["algorithm"])
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func md5Hex(in string) string {
	sum := md5.Sum([]byte(in))
	return hex.EncodeToString(sum[:])
}

func sha256Hex(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])
}

func names(entries []entry) []string {
	out := make([]string, 0, len(entries))
	for _, item := range entries {
		out = append(out, item.Name)
	}
	return out
}

var _ = url.PathEscape
