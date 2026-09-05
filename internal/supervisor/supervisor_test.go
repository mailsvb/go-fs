package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/httpd"
)

// logStore collects what the supervisor reports.
type logStore struct {
	mu      sync.Mutex
	records []string
}

func (s *logStore) Enabled(context.Context, slog.Level) bool { return true }
func (s *logStore) WithAttrs([]slog.Attr) slog.Handler       { return s }
func (s *logStore) WithGroup(string) slog.Handler            { return s }

func (s *logStore) Handle(_ context.Context, record slog.Record) error {
	line := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", attr.Key, attr.Value.Any())
		return true
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, line)
	return nil
}

func (s *logStore) has(fragment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.records {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func (s *logStore) waitFor(t *testing.T, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.has(fragment) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("never saw %q in:\n%s", fragment, strings.Join(s.records, "\n"))
}

// baseConfig is a configuration with the HTTP and TFTP servers on ephemeral
// ports and one account.
func baseConfig(t *testing.T) config.Config {
	t.Helper()
	folder := t.TempDir()
	cfg := config.Default()
	cfg.General.Basefolder = folder
	cfg.FTP.Enabled = false
	cfg.SFTP.Enabled = false
	cfg.TFTP.Enabled = false
	cfg.HTTP.Enabled = true
	cfg.HTTP.Port = 0
	cfg.HTTP.Basefolder = folder
	cfg.HTTP.LoginFailureDelay = 0
	cfg.HTTP.PathsRequireAuth = []string{"^/private/.*"}
	cfg.HTTP.Users = []config.HTTPUser{{
		Username: "john", Password: "doe", Paths: []string{"^/.*"},
	}}
	cfg.TFTP.Basefolder = folder
	return cfg
}

func newSupervisor(t *testing.T) (*Supervisor, *logStore, context.Context) {
	t.Helper()
	logs := &logStore{}
	sup := New(slog.New(logs))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sup.Shutdown(context.Background())
	})
	return sup, logs, ctx
}

// httpPort digs the bound port out of the running HTTP server.
func httpPort(t *testing.T, sup *Supervisor) int {
	t.Helper()
	sup.mu.Lock()
	defer sup.mu.Unlock()
	server, ok := sup.running["http"].(*httpd.Server)
	if !ok {
		t.Fatal("the http server is not running")
	}
	addr, ok := server.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("the http server has no address")
	}
	return addr.Port
}

func fetch(t *testing.T, port int, path, name, password string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		"http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		req.SetBasicAuth(name, password)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestApplyStartsStopsAndRestarts(t *testing.T) {
	sup, logs, ctx := newSupervisor(t)
	cfg := baseConfig(t)

	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := sup.Running(); len(got) != 1 || got[0] != "http" {
		t.Fatalf("running = %v, want just http", got)
	}
	logs.waitFor(t, "server started server=http")

	// switching another server on starts only that one
	cfg.TFTP.Enabled = true
	cfg.TFTP.Port = 0
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := sup.Running(); len(got) != 2 {
		t.Fatalf("running = %v, want http and tftp", got)
	}
	logs.waitFor(t, "server started server=tftp")

	// switching it off again stops it
	cfg.TFTP.Enabled = false
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := sup.Running(); len(got) != 1 || got[0] != "http" {
		t.Fatalf("running = %v, want just http", got)
	}
	logs.waitFor(t, "server stopped server=tftp")
}

// An account change is applied to the running server; a port change rebinds it.
func TestApplyReloadsOrRestarts(t *testing.T) {
	sup, logs, ctx := newSupervisor(t)
	cfg := baseConfig(t)
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	port := httpPort(t, sup)

	cfg.HTTP.Users = append(cfg.HTTP.Users, config.HTTPUser{
		Username: "jane", Password: "secret", Paths: []string{"^/.*"},
	})
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	logs.waitFor(t, "server reloaded server=http")
	if got := httpPort(t, sup); got != port {
		t.Errorf("the listener was rebound: %d -> %d", port, got)
	}

	// a base folder change cannot be applied to a bound listener
	cfg.HTTP.Basefolder = t.TempDir()
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	logs.waitFor(t, "server restarted server=http")
}

// The whole point: an account change must not disturb a transfer in flight.
func TestReloadDoesNotDisturbATransfer(t *testing.T) {
	sup, logs, ctx := newSupervisor(t)
	cfg := baseConfig(t)

	// a file big enough that the read takes a moment
	payload := strings.Repeat("go-fs payload ", 400000) // about 5.6 MB
	if err := os.WriteFile(filepath.Join(cfg.HTTP.Basefolder, "big.bin"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	port := httpPort(t, sup)

	res := fetch(t, port, "/big.bin", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	// read the first bytes, then reload while the rest is still coming
	head := make([]byte, 1024)
	if _, err := io.ReadFull(res.Body, head); err != nil {
		t.Fatal(err)
	}

	cfg.HTTP.Users = append(cfg.HTTP.Users, config.HTTPUser{
		Username: "jane", Password: "secret", Paths: []string{"^/.*"},
	})
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	logs.waitFor(t, "server reloaded server=http")

	rest, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("the download was cut short: %v", err)
	}
	if got := string(head) + string(rest); got != payload {
		t.Errorf("the download came back changed: %d bytes instead of %d", len(got), len(payload))
	}

	// and the account added mid-transfer works
	if res := fetch(t, port, "/private/", "jane", "secret"); res.StatusCode == http.StatusUnauthorized {
		t.Error("jane should have been accepted")
	}
}
