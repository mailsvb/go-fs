package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go-fs/internal/config"
)

// testConfig is a file that validates: the folders it names exist.
func testConfig(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	path := filepath.Join(folder, "go-fs.toml")
	body := `
[general]
basefolder = ` + strconv.Quote(folder) + `
adminInterfaceEnabled = true
adminUsername = "admin"
adminPassword = "secret"

[ftp]
enabled = true
port = 2121

[[ftp.users]]
username = "john"
password = "doe"
allowUserFileRetrieve = true

[tftp]
enabled = false
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testServer(t *testing.T, path string) (*Server, *httptest.Server) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg.General, path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server)
	t.Cleanup(front.Close)
	return server, front
}

// get asks for the state of the configuration.
func get(t *testing.T, front *httptest.Server) state {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, front.URL+"/api/config", nil)
	request.SetBasicAuth("admin", "secret")
	answer, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(answer.Body)
		t.Fatalf("GET /api/config: %s: %s", answer.Status, body)
	}
	var decoded state
	if err := json.NewDecoder(answer.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// post applies a set of values and reports the status and the body.
func post(t *testing.T, front *httptest.Server, values any, headers map[string]string) (int, string) {
	t.Helper()
	body, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/api/config", strings.NewReader(string(body)))
	request.SetBasicAuth("admin", "secret")
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	answer, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	returned, _ := io.ReadAll(answer.Body)
	return answer.StatusCode, strings.TrimSpace(string(returned))
}

// section finds a section of a value tree.
func section(t *testing.T, values map[string]any, name string) map[string]any {
	t.Helper()
	found, ok := values[name].(map[string]any)
	if !ok {
		t.Fatalf("the values have no %s section", name)
	}
	return found
}

// roundTripJSON puts a value tree through the encoding the browser puts it
// through, so that a test sees the types a request really carries.
func roundTripJSON(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
