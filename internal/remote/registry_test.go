package remote_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

func openRegistry(t *testing.T, login remote.Login) remote.FS {
	t.Helper()
	remote.ForgetCatalogs()
	checked, err := login.Checked(false)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := remote.Open(context.Background(), checked, sshConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return fsys
}

// names lists a folder as names, a folder's with a slash after it.
func registryNames(t *testing.T, fsys remote.FS, p string) []string {
	t.Helper()
	infos, err := fsys.ReadDir(p)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", p, err)
	}
	var listed []string
	for _, info := range infos {
		name := info.Name()
		if info.IsDir() {
			name += "/"
		}
		listed = append(listed, name)
	}
	return listed
}

func TestRegistryLoginIsChecked(t *testing.T) {
	login, err := remote.Login{Type: config.ServerTypeRegistry, URL: " Acme.jfrog.io/ ", Auth: config.RegistryAuthBearer,
		Token: " t ", Password: "dropped", Host: "ignored"}.Checked(false)
	if err != nil {
		t.Fatal(err)
	}
	if login.URL != "https://acme.jfrog.io" || login.Token != "t" || login.Password != "" || login.Host != "" {
		t.Errorf("the login reads %+v", login)
	}
	if login.Shown() != "acme.jfrog.io" || login.Who() != "the token" {
		t.Errorf("the login is shown as %q, known as %q", login.Shown(), login.Who())
	}
	anonymous, err := remote.Login{Type: config.ServerTypeRegistry, URL: "https://r.example", Username: "x"}.Checked(false)
	if err != nil || anonymous.Auth != config.RegistryAuthNone || anonymous.Username != "" {
		t.Errorf("an anonymous login reads %+v, %v", anonymous, err)
	}
	for name, login := range map[string]remote.Login{
		"no url":               {Type: config.ServerTypeRegistry},
		"basic without a user": {Type: config.ServerTypeRegistry, URL: "https://r.example", Auth: config.RegistryAuthBasic},
		"bearer without token": {Type: config.ServerTypeRegistry, URL: "https://r.example", Auth: config.RegistryAuthBearer},
		"an unknown auth":      {Type: config.ServerTypeRegistry, URL: "https://r.example", Auth: "ntlm"},
		"a cert without key":   {Type: config.ServerTypeRegistry, URL: "https://r.example", ClientCert: "x"},
	} {
		if _, err := login.Checked(false); err == nil {
			t.Errorf("a login with %s was accepted", name)
		}
	}
}

func TestRegistryIsBrowsedByItsCatalog(t *testing.T) {
	for _, c := range []struct {
		name  string
		host  *remotetest.RegistryHost
		login remote.Login
	}{
		{"basic", remotetest.NewRegistryHost(t, "", "ci", "pw", ""),
			remote.Login{Auth: config.RegistryAuthBasic, Username: "ci", Password: "pw"}},
		{"bearer", remotetest.NewRegistryHost(t, "/artifactory/api/docker/local", "", "", "tok"),
			remote.Login{Auth: config.RegistryAuthBearer, Token: "tok"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			host := c.host
			host.LinkWithoutPrefix = host.Prefix != ""
			for _, repository := range []string{"alpine", "team", "team/app", "team/tools/lint", "zeta/one"} {
				host.Tag(repository, "1.0", `{"schemaVersion":2}`)
			}
			host.Tag("team/app", "latest", `{"schemaVersion":2,"x":1}`)
			host.Tag("team", "app", `{}`) // a tag that is a folder's name too
			login := c.login
			login.Type, login.URL, login.Server = config.ServerTypeRegistry, host.URL, "jfrog"
			restore := remote.SetMaxRegistryNames(100)
			defer restore()
			fsys := openRegistry(t, login)

			if err := remote.Test(context.Background(), mustCheck(t, login), sshConfig()); err != nil {
				t.Fatalf("the login test: %v", err)
			}
			if got, want := registryNames(t, fsys, "/"), []string{"alpine/", "team/", "zeta/"}; !reflect.DeepEqual(got, want) {
				t.Errorf("/ lists %v, want %v", got, want)
			}
			if got, want := registryNames(t, fsys, "/team"), []string{"app/", "tools/", "1.0"}; !reflect.DeepEqual(got, want) {
				t.Errorf("/team lists %v, want %v", got, want)
			}
			if got, want := registryNames(t, fsys, "/team/app"), []string{"1.0", "latest"}; !reflect.DeepEqual(got, want) {
				t.Errorf("/team/app lists %v, want %v", got, want)
			}
			if got, want := registryNames(t, fsys, "/team/tools"), []string{"lint/"}; !reflect.DeepEqual(got, want) {
				t.Errorf("/team/tools lists %v, want %v", got, want)
			}
			// the catalog was read once, the second time from what was kept
			if n := host.CatalogRequests.Load(); n != 1 {
				t.Errorf("the catalog was asked for %d times", n)
			}

			info, err := fsys.Stat("/team/app/latest")
			if err != nil || info.IsDir() || info.Size() != int64(len(`{"schemaVersion":2,"x":1}`)) {
				t.Fatalf("Stat of a tag: %v, %v", info, err)
			}
			if info, err := fsys.Stat("/team/tools"); err != nil || !info.IsDir() {
				t.Errorf("Stat of a namespace: %v, %v", info, err)
			}
			for _, missing := range []string{"/nothing", "/team/app/2.0", "/team/tools/2.0"} {
				if _, err := fsys.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("Stat(%q): %v", missing, err)
				}
			}
			file, err := fsys.Open("/team/app/latest")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(file)
			_ = file.Close()
			if string(body) != `{"schemaVersion":2,"x":1}` {
				t.Errorf("the tag reads %s", body)
			}
			if err := fsys.Mkdir("/x"); !errors.Is(err, fs.ErrPermission) {
				t.Errorf("Mkdir: %v", err)
			}
			if _, err := fsys.Put("/team/app/2.0", strings.NewReader("x"), 1); !errors.Is(err, fs.ErrPermission) {
				t.Errorf("Put: %v", err)
			}
			if c.name == "bearer" && !strings.Contains(strings.Join(host.Scopes(), " "), "registry:catalog:*") {
				t.Errorf("the token service was asked for %v", host.Scopes())
			}
		})
	}
}

func mustCheck(t *testing.T, login remote.Login) remote.Login {
	t.Helper()
	checked, err := login.Checked(false)
	if err != nil {
		t.Fatal(err)
	}
	return checked
}

func TestRegistryListingsArePagedAndBounded(t *testing.T) {
	host := remotetest.NewRegistryHost(t, "", "", "", "")
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		host.Tag("lib/"+name, "1", "{}")
	}
	for i := range 2500 {
		host.Tag("lib/a", fmt.Sprintf("v%04d", i), "{}")
	}
	restore := remote.SetMaxRegistryNames(2000)
	defer restore()
	fsys := openRegistry(t, remote.Login{Type: config.ServerTypeRegistry, URL: host.URL})
	if got := registryNames(t, fsys, "/lib"); len(got) != 5 {
		t.Errorf("/lib lists %v", got)
	}
	if remote.RegistryTruncated(fsys) {
		t.Error("a whole listing is said to be truncated")
	}
	if got := registryNames(t, fsys, "/lib/a"); len(got) != 2000 {
		t.Errorf("/lib/a lists %d tags", len(got))
	}
	if !remote.RegistryTruncated(fsys) {
		t.Error("a bounded listing is not said to be truncated")
	}
}

func TestRegistryWithoutACatalogIsOpenedByName(t *testing.T) {
	host := remotetest.NewRegistryHost(t, "", "", "", "")
	host.NoCatalog.Store(true)
	host.Tag("library/alpine", "3.20", "{}")
	host.Tag("library/alpine", "latest", "{}")
	login := remote.Login{Type: config.ServerTypeRegistry, URL: host.URL}
	if err := remote.Test(context.Background(), mustCheck(t, login), sshConfig()); err != nil {
		t.Fatalf("the login test of a registry without a catalog: %v", err)
	}
	fsys := openRegistry(t, login)
	if _, err := fsys.ReadDir("/"); !errors.Is(err, remote.ErrNoCatalog) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the top of a registry without a catalog: %v", err)
	}
	if info, err := fsys.Stat("/library/alpine"); err != nil || !info.IsDir() {
		t.Fatalf("Stat of a repository named: %v, %v", info, err)
	}
	if got, want := registryNames(t, fsys, "/library/alpine"), []string{"3.20", "latest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the repository lists %v, want %v", got, want)
	}
	if info, err := fsys.Stat("/library/alpine/latest"); err != nil || info.IsDir() {
		t.Errorf("Stat of a tag: %v, %v", info, err)
	}
	if _, err := fsys.Stat("/library/nothing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat of a repository that is not there: %v", err)
	}
}

func TestRegistryRefusesAWrongLogin(t *testing.T) {
	host := remotetest.NewRegistryHost(t, "", "ci", "pw", "")
	host.Tag("app", "1", "{}")
	login := remote.Login{Type: config.ServerTypeRegistry, URL: host.URL, Auth: config.RegistryAuthBasic,
		Username: "ci", Password: "wrong"}
	err := remote.Test(context.Background(), mustCheck(t, login), sshConfig())
	if err == nil || !strings.Contains(err.Error(), "refused the username and password") {
		t.Errorf("a wrong password: %v", err)
	}
}

// Docker Hub's token service refuses the scope of the catalog outright, which
// is a registry without a catalog all the same.
func TestRegistryWhoseTokenServiceRefusesTheCatalog(t *testing.T) {
	host := remotetest.NewRegistryHost(t, "", "", "", "tok")
	host.TokenRefusesCatalog.Store(true)
	host.Tag("library/alpine", "latest", "{}")
	fsys := openRegistry(t, remote.Login{Type: config.ServerTypeRegistry, URL: host.URL,
		Auth: config.RegistryAuthBearer, Token: "tok"})
	if _, err := fsys.Stat("/"); err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if _, err := fsys.ReadDir("/"); !errors.Is(err, remote.ErrNoCatalog) {
		t.Errorf("the top: %v", err)
	}
	if got := registryNames(t, fsys, "/library/alpine"); !reflect.DeepEqual(got, []string{"latest"}) {
		t.Errorf("the repository lists %v", got)
	}
	wrong := openRegistry(t, remote.Login{Type: config.ServerTypeRegistry, URL: host.URL,
		Auth: config.RegistryAuthBearer, Token: "wrong"})
	if _, err := wrong.Stat("/"); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("a wrong token at the top: %v", err)
	}
}
