package httpd

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
)

// withProfile gives a server a configuration file holding its accounts, with
// the servers each one sets and a comment that has to survive a change, and
// has a changed password applied to the server as the supervisor would.
func withProfile(t *testing.T, cfg *httpConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	body := "# kept\n[general]\nbaseFolderFiles = " + strconv.Quote(cfg.Basefolder) + "\n" +
		"[ftp]\nenabled = false\n[tftp]\nenabled = false\n[http.main]\nenabled = true\n"
	for _, user := range cfg.Users {
		body += "\n# the account " + user.Username + "\n[[users]]\nusername = " + strconv.Quote(user.Username) +
			"\npassword = " + strconv.Quote(user.Password) + " # its password\n"
		for flag, on := range map[string]bool{"http": user.HTTP, "ftp": user.FTP, "registry": user.Registry} {
			if on {
				body += flag + " = true\n"
			}
		}
		if len(user.Paths) > 0 {
			quoted := make([]string, len(user.Paths))
			for i, pattern := range user.Paths {
				quoted[i] = strconv.Quote(pattern)
			}
			body += "paths = [" + strings.Join(quoted, ", ") + "]\n"
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
}

// profileServer is a server whose accounts can change their passwords: john,
// who may use the files, and ftp, an FTP account that may use nothing here.
func profileServer(t *testing.T, tune func(*httpConfig)) *testServer {
	t.Helper()
	var cfg httpConfig
	server := newServer(t, func(c *httpConfig) {
		c.PathsWithoutAuth = []string{publicExcept("private")}
		c.Users = []config.User{fullUser("john", "doe1234x"),
			{Username: "ftp", Password: "ftp-pass-1", FTP: true}}
		if tune != nil {
			tune(c)
		}
		withProfile(t, c)
		cfg = *c
	})
	server.SetReloader(func() error {
		loaded, err := config.Parse(mustReadFile(t, cfg.ConfigPath))
		if err != nil {
			return err
		}
		users := cfg.Users
		for i := range users {
			for _, user := range loaded.Users {
				if user.Username == users[i].Username {
					users[i].Password = user.Password
				}
			}
		}
		return server.Reload(cfg.HTTP, config.Default().HTTPS, cfg.SSH, users, cfg.Tokens, cfg.Servers)
	})
	return server
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// changePassword posts the overlay's form as its script does.
func changePassword(t *testing.T, server *testServer, session *http.Cookie, current, password, repeat string, headers ...string) (*http.Response, string) {
	t.Helper()
	form := url.Values{"current": {current}, "password": {password}, "repeat": {repeat}}
	req, err := http.NewRequest(http.MethodPost, server.url("/?go-fs=password"), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if session != nil {
		req.AddCookie(session)
	}
	res := do(t, req)
	return res, bodyOf(t, res)
}

func TestAPasswordChangeRewritesTheFileAndKeepsTheSession(t *testing.T) {
	server := profileServer(t, nil)
	server.write(t, "private/secret.txt", "x")
	session := login(t, server, "/", "john", "doe1234x")

	res, body := changePassword(t, server, session, "doe1234x", "n3w-password", "n3w-password")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", res.StatusCode, body)
	}
	fresh := cookieNamed(res, sessionCookie)
	if fresh == nil {
		t.Fatal("the session was not issued again for the new password")
	}

	written := string(mustReadFile(t, server.configPath))
	for _, want := range []string{"# kept", "# the account john", `password = "n3w-password"`, `password = "ftp-pass-1" # its password`} {
		if !strings.Contains(written, want) {
			t.Errorf("the file has no %s:\n%s", want, written)
		}
	}
	if strings.Contains(written, "doe1234x") {
		t.Errorf("the file still holds the old password:\n%s", written)
	}

	// the browser that changed it stays signed in; one that was signed in
	// under the old password is not
	if res := withSession(t, server, http.MethodGet, "/private/secret.txt", fresh); res.StatusCode != http.StatusOK {
		t.Errorf("the renewed session was answered %d, want 200", res.StatusCode)
	}
	if res := withSession(t, server, http.MethodGet, "/private/secret.txt", session); res.StatusCode == http.StatusOK {
		t.Error("a session from before the change still opens the files")
	}
	if res := postLogin(t, server, "/", "john", "doe1234x"); cookieNamed(res, sessionCookie) != nil {
		t.Error("the old password still logs in")
	}
	login(t, server, "/", "john", "n3w-password")

	if server.logs.find("http password changed") == nil {
		t.Error("the change was not recorded")
	}
	server.logs.mu.Lock()
	records := slices.Clone(server.logs.records)
	server.logs.mu.Unlock()
	for _, record := range records {
		if line := fmt.Sprint(record); strings.Contains(line, "n3w-password") || strings.Contains(line, "doe1234x") {
			t.Errorf("a record holds a password: %s", line)
		}
	}
}

func TestAPasswordChangeIsRefused(t *testing.T) {
	server := profileServer(t, nil)
	session := login(t, server, "/", "john", "doe1234x")
	cases := []struct {
		name                     string
		current, password, again string
		want                     int
		says                     string
	}{
		{"wrong current", "nope", "n3w-password", "n3w-password", http.StatusForbidden, "current password is wrong"},
		{"mismatch", "doe1234x", "n3w-password", "n3w-passw0rd", http.StatusBadRequest, "do not match"},
		{"short", "doe1234x", "short", "short", http.StatusBadRequest, "at least 8"},
		{"same", "doe1234x", "doe1234x", "doe1234x", http.StatusBadRequest, "is the current one"},
		{"control", "doe1234x", "new\npassword", "new\npassword", http.StatusBadRequest, "control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := changePassword(t, server, session, tc.current, tc.password, tc.again)
			if res.StatusCode != tc.want || !strings.Contains(body, tc.says) {
				t.Errorf("answer = %d %q, want %d saying %q", res.StatusCode, body, tc.want, tc.says)
			}
		})
	}
	if written := string(mustReadFile(t, server.configPath)); !strings.Contains(written, `"doe1234x"`) {
		t.Errorf("a refused change touched the file:\n%s", written)
	}

	if res, _ := changePassword(t, server, nil, "doe1234x", "n3w-password", "n3w-password"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", res.StatusCode)
	}
	if res, _ := changePassword(t, server, session, "doe1234x", "n3w-password", "n3w-password",
		"Origin", "https://elsewhere.example", "Sec-Fetch-Site", "cross-site"); res.StatusCode != http.StatusForbidden {
		t.Errorf("from another site: %d, want 403", res.StatusCode)
	}
	if res := withSession(t, server, http.MethodGet, "/?go-fs=password", session); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", res.StatusCode)
	}
}

func TestAWrongCurrentPasswordCountsTowardTheLock(t *testing.T) {
	server := profileServer(t, func(cfg *httpConfig) { cfg.LoginAttempts = 2 })
	fixClock(server)
	session := login(t, server, "/", "john", "doe1234x")
	for range 2 {
		changePassword(t, server, session, "nope", "n3w-password", "n3w-password")
	}
	res, _ := changePassword(t, server, session, "doe1234x", "n3w-password", "n3w-password")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after two wrong passwords: %d, want 429", res.StatusCode)
	}
}

func TestAPasswordNotYetAppliedSaysSo(t *testing.T) {
	server := profileServer(t, nil)
	server.SetReloader(nil)
	server.mkdir(t, "private")
	session := login(t, server, "/", "john", "doe1234x")
	res, body := changePassword(t, server, session, "doe1234x", "n3w-password", "n3w-password")
	if res.StatusCode != http.StatusAccepted || !strings.Contains(body, "reloaded") {
		t.Errorf("answer = %d %q, want 202 saying it waits for a reload", res.StatusCode, body)
	}
	// the session goes on under the password still in force
	if res := withSession(t, server, http.MethodGet, "/private/", cookieNamed(res, sessionCookie)); res.StatusCode != http.StatusOK {
		t.Errorf("the session after the change was answered %d, want 200", res.StatusCode)
	}
}

func TestAnAccountOfAnotherServerLogsInToTheProfileOnly(t *testing.T) {
	server := profileServer(t, nil)
	server.write(t, "private/secret.txt", "x")

	// the Profile page sends a browser to the login, which returns there
	res := browserGet(t, server, "/?go-fs=profile")
	location := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(location, "go-fs=login") ||
		!strings.Contains(location, "go-fs-then=profile") {
		t.Fatalf("answer = %d to %q, want 303 to the login that returns to the profile", res.StatusCode, location)
	}
	if _, body := pageRequest(t, server, http.MethodGet, location, nil); !strings.Contains(body, "Profile") {
		t.Errorf("the login does not say it is for the profile:\n%s", body)
	}
	res = postLoginAt(t, server, location, "ftp", "ftp-pass-1")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?go-fs=profile" {
		t.Fatalf("login = %d to %q, want 303 to the profile", res.StatusCode, res.Header.Get("Location"))
	}
	session := cookieNamed(res, sessionCookie)

	res, body := pageRequest(t, server, http.MethodGet, "/?go-fs=profile", session)
	for _, want := range []string{`id="credentials"`, `data-action="/?go-fs=password"`, "ftp", `class="signin profile"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the profile page has no %s:\n%s", want, body)
		}
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("profile = %d, want 200", res.StatusCode)
	}

	// the session opens nothing else, and is not thrown away by trying
	if res := withSession(t, server, http.MethodGet, "/private/secret.txt", session); res.StatusCode == http.StatusOK {
		t.Error("an FTP account's session opens the files")
	}
	if res, _ := pageRequest(t, server, http.MethodGet, "/?go-fs=profile", session); res.StatusCode != http.StatusOK {
		t.Errorf("after asking for a file the profile is answered %d, want 200", res.StatusCode)
	}

	// a login anywhere else lands on the profile as well
	if res := postLogin(t, server, "/private/", "ftp", "ftp-pass-1"); res.Header.Get("Location") != "/?go-fs=profile" {
		t.Errorf("a login at the files went to %q, want the profile", res.Header.Get("Location"))
	}

	if res, body := changePassword(t, server, session, "ftp-pass-1", "ftp-pass-2", "ftp-pass-2"); res.StatusCode != http.StatusNoContent {
		t.Errorf("change = %d %q, want 204", res.StatusCode, body)
	}
}

// menuItem is the Credentials button of the menu, as the page has it; the
// inlined script names the attribute too.
const menuItem = "data-credentials><svg"

func TestTheMenuOffersCredentialsToASession(t *testing.T) {
	server := profileServer(t, nil)
	server.mkdir(t, "private")
	session := login(t, server, "/", "john", "doe1234x")
	_, body := pageRequest(t, server, http.MethodGet, "/private/", session)
	menu := strings.Index(body, menuItem)
	logout := strings.Index(body, "go-fs=logout")
	if menu < 0 || logout < 0 || menu > logout {
		t.Errorf("the menu has no Credentials above Log out:\n%s", body)
	}
	if !strings.Contains(body, `<dialog id="credentials"`) {
		t.Error("the page has no overlay")
	}

	// nobody signed in: no Credentials
	if _, body := pageRequest(t, server, http.MethodGet, "/", nil); strings.Contains(body, menuItem) {
		t.Error("the menu offers Credentials to nobody")
	}
	// no file to write a password to: no Credentials and no Profile
	plain := newServer(t, nil)
	plain.mkdir(t, "private")
	session = login(t, plain, "/", "john", "doe")
	if _, body := pageRequest(t, plain, http.MethodGet, "/private/", session); strings.Contains(body, menuItem) {
		t.Error("the menu offers Credentials where a password cannot be stored")
	}
	if res, _ := pageRequest(t, plain, http.MethodGet, "/?go-fs=profile", session); res.StatusCode != http.StatusNotFound {
		t.Errorf("the profile page without a file = %d, want 404", res.StatusCode)
	}
}
