package admin

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-fs/internal/config"
	"go-fs/internal/service"
)

func TestStateDescribesTheFile(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)

	body := get(t, front)
	if body.Path != path {
		t.Errorf("the state names %q, want %q", body.Path, path)
	}
	if !body.Writable || body.WriteError != "" {
		t.Errorf("a writable file was reported as %v: %s", body.Writable, body.WriteError)
	}
	if !body.Reload {
		t.Error("reloadConfig is on by default, the state says it is off")
	}
	if len(body.Schema.Sections) == 0 {
		t.Fatal("the state carries no schema")
	}
	if port := section(t, body.Values, "ftp")["port"]; port != float64(2121) {
		t.Errorf("ftp.port is %v, want the 2121 the file says", port)
	}
	// an inherited folder stays inherited in what the page shows
	if folder := section(t, body.Values, "ftp")["basefolder"]; folder != "" {
		t.Errorf("ftp.basefolder is %q, the file leaves it to general.basefolder", folder)
	}
}

func TestApplyWritesTheFile(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)

	body := get(t, front)
	section(t, body.Values, "ftp")["port"] = 2122
	section(t, body.Values, "log")["level"] = "debug"

	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}

	written, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if written.FTP.Port != 2122 || written.Log.Level != "debug" {
		t.Errorf("the file says port %d level %q", written.FTP.Port, written.Log.Level)
	}
	if len(written.FTP.Users) != 1 || written.FTP.Users[0].Username != "john" {
		t.Errorf("the accounts did not survive the write: %+v", written.FTP.Users)
	}
	if !written.FTP.Users[0].Permissions().FileRetrieve {
		t.Error("a granted permission did not survive the write")
	}

	// the previous file is kept, because the rewrite drops its comments
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), "port = 2121") {
		t.Errorf("the backup does not hold the previous file:\n%s", backup)
	}
}

func TestApplyAddsAndRemovesRecords(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)

	body := get(t, front)
	ftp := section(t, body.Values, "ftp")
	ftp["users"] = append(ftp["users"].([]any), map[string]any{
		"username":              "max",
		"password":              "mustermann",
		"allowUserFileRetrieve": true,
	})
	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}
	written, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.FTP.Users) != 2 || written.FTP.Users[1].Username != "max" {
		t.Fatalf("the account was not added: %+v", written.FTP.Users)
	}

	body = get(t, front)
	ftp = section(t, body.Values, "ftp")
	ftp["users"] = ftp["users"].([]any)[:1]
	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}
	written, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.FTP.Users) != 1 {
		t.Errorf("the account was not removed: %+v", written.FTP.Users)
	}
}

// TestApplyRefusesWhatWouldNotStart checks that the page reports what -check
// reports, and that a file that cannot work is never written.
func TestApplyRefusesWhatWouldNotStart(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, front)
	section(t, body.Values, "ftp")["port"] = 70000

	status, answer := post(t, front, body.Values, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("a port of 70000 answered %d: %s", status, answer)
	}
	if !strings.Contains(answer, "ftp.port 70000 is out of range") {
		t.Errorf("the message was %q", answer)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the file was written although the configuration was refused")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a backup was made although nothing was written")
	}
}

func TestApplyReportsAFileItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	path := testConfig(t)
	_, front := testServer(t, path)
	body := get(t, front)

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if state := get(t, front); state.Writable || !strings.Contains(state.WriteError, "permission denied") {
		t.Errorf("a read-only file reported writable=%v error=%q", state.Writable, state.WriteError)
	}
	status, answer := post(t, front, body.Values, nil)
	if status != http.StatusConflict {
		t.Fatalf("writing a read-only file answered %d: %s", status, answer)
	}
	if !strings.Contains(answer, "permission denied") {
		t.Errorf("the message was %q", answer)
	}
}

func TestUnauthenticatedIsRefused(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)

	for _, credentials := range [][2]string{{"", ""}, {"admin", "wrong"}, {"root", "secret"}} {
		request, _ := http.NewRequest(http.MethodGet, front.URL+"/api/config", nil)
		if credentials[0] != "" {
			request.SetBasicAuth(credentials[0], credentials[1])
		}
		answer, err := front.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, answer.Body)
		answer.Body.Close()
		if answer.StatusCode != http.StatusUnauthorized {
			t.Errorf("%v answered %s", credentials, answer.Status)
		}
		if !strings.HasPrefix(answer.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("no Basic challenge: %q", answer.Header.Get("WWW-Authenticate"))
		}
	}
}

// TestApplyRefusesAnotherOrigin covers the two guards against a page on another
// site writing this configuration with the browser's saved credentials.
func TestApplyRefusesAnotherOrigin(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)
	body := get(t, front)

	if status, _ := post(t, front, body.Values, map[string]string{
		"Origin": "https://elsewhere.example",
	}); status != http.StatusForbidden {
		t.Errorf("a foreign origin answered %d", status)
	}
	if status, _ := post(t, front, body.Values, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}); status != http.StatusUnsupportedMediaType {
		t.Errorf("a form post answered %d", status)
	}
	if status, _ := post(t, front, body.Values, map[string]string{
		"Origin": front.URL,
	}); status != http.StatusOK {
		t.Errorf("the interface's own origin answered %d", status)
	}
	// the host is what is compared, so a proxy that terminates TLS in front of
	// the interface does not lock everyone out
	if status, _ := post(t, front, body.Values, map[string]string{
		"Origin": "https://" + strings.TrimPrefix(front.URL, "http://"),
	}); status != http.StatusOK {
		t.Errorf("the same host over another scheme answered %d", status)
	}
}

func TestPageIsServed(t *testing.T) {
	path := testConfig(t)
	_, front := testServer(t, path)

	for name, wantType := range map[string]string{
		"/":          "text/html; charset=utf-8",
		"/admin.css": "text/css; charset=utf-8",
		"/admin.js":  "text/javascript; charset=utf-8",
	} {
		request, _ := http.NewRequest(http.MethodGet, front.URL+name, nil)
		request.SetBasicAuth("admin", "secret")
		answer, err := front.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(answer.Body)
		answer.Body.Close()
		if answer.StatusCode != http.StatusOK {
			t.Errorf("%s answered %s", name, answer.Status)
		}
		if got := answer.Header.Get("Content-Type"); got != wantType {
			t.Errorf("%s is %q, want %q", name, got, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestReload(t *testing.T) {
	path := testConfig(t)
	server, front := testServer(t, path)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// the credentials are swapped without rebinding anything
	next := cfg.General
	next.AdminPassword = "changed"
	if err := server.Reload(next); err != nil {
		t.Fatalf("swapping the password: %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, front.URL+"/api/config", nil)
	request.SetBasicAuth("admin", "secret")
	answer, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, answer.Body)
	answer.Body.Close()
	if answer.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old password still works: %s", answer.Status)
	}

	// the listener cannot move under a running server
	for name, change := range map[string]config.General{
		"port":    withPort(cfg.General, 10444),
		"address": withAddress(cfg.General, ""),
		"scheme":  withPlainHTTP(cfg.General),
	} {
		if err := server.Reload(change); err != service.ErrNeedsRestart {
			t.Errorf("changing the %s reported %v, want a restart", name, err)
		}
	}
}

func withPort(cfg config.General, port int) config.General {
	cfg.AdminInterfacePort = port
	return cfg
}

func withAddress(cfg config.General, address string) config.General {
	cfg.AdminInterfaceAddress = address
	return cfg
}

func withPlainHTTP(cfg config.General) config.General {
	cfg.AdminInterfaceUseHTTPS = false
	return cfg
}

// TestNewNeedsThePath guards the one thing the interface cannot work without.
func TestNewNeedsThePath(t *testing.T) {
	if _, err := New(config.Default().General, "",
		slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("a server was built with no configuration file to edit")
	}
}

// TestWriteFollowsASymlink checks that a linked configuration file is replaced
// where it lives rather than turning the link into a regular file.
func TestWriteFollowsASymlink(t *testing.T) {
	path := testConfig(t)
	link := filepath.Join(filepath.Dir(path), "linked.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}

	_, front := testServer(t, link)
	body := get(t, front)
	section(t, body.Values, "ftp")["port"] = 2123
	if status, answer := post(t, front, body.Values, nil); status != http.StatusOK {
		t.Fatalf("apply answered %d: %s", status, answer)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	written, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if written.FTP.Port != 2123 {
		t.Errorf("the target says port %d", written.FTP.Port)
	}
}

// TestStartServesOverTLS binds a real listener and checks the default: the
// interface carries every password in the file, so it is served over TLS unless
// that is deliberately turned off, which TestStartServesPlainHTTP covers.
func TestStartServesOverTLS(t *testing.T) {
	path := testConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.General.AdminInterfacePort = 0

	server, err := New(cfg.General, path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())

	address := server.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	request, _ := http.NewRequest(http.MethodGet, "https://"+address+"/api/config", nil)
	request.SetBasicAuth("admin", "secret")
	answer, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the interface answered %s", answer.Status)
	}

	// the same port serves nothing over plain HTTP: net/http answers such a
	// request with the 400 that says so rather than with the page
	plain, err := http.Get("http://" + address + "/")
	if err != nil {
		return
	}
	body, _ := io.ReadAll(plain.Body)
	plain.Body.Close()
	if plain.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(body), "HTTPS server") {
		t.Errorf("a plain request was answered %s: %s", plain.Status, body)
	}
}

// TestStartServesPlainHTTP covers the other transport: with
// adminInterfaceUseHttps off the same port answers plain HTTP, for a proxy that
// terminates TLS in front of it, and speaks no TLS of its own.
func TestStartServesPlainHTTP(t *testing.T) {
	path := testConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.General.AdminInterfacePort = 0
	cfg.General.AdminInterfaceUseHTTPS = false

	server, err := New(cfg.General, path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())

	address := server.Addr().String()
	request, _ := http.NewRequest(http.MethodGet, "http://"+address+"/api/config", nil)
	request.SetBasicAuth("admin", "secret")
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the interface answered %s", answer.Status)
	}

	// the account is still required, the transport is all that changed
	plain, err := http.Get("http://" + address + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, plain.Body)
	plain.Body.Close()
	if plain.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request answered %s", plain.Status)
	}

	// and there is no TLS on this port to speak to
	if _, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}); err == nil {
		t.Error("the interface completed a TLS handshake although it serves plain HTTP")
	}
}
